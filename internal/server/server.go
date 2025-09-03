package server

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "os"
    "net/http"
    "sync"
    "time"
    "image"
    "image/png"
    "strconv"

    "github.com/google/uuid"
    "github.com/pion/webrtc/v3"
    "whep/internal/stream"
    // optional on non-windows/no-cgo builds via indirection
    "whep/internal/ndi"
    "strings"
)

type Config struct {
    Host   string
    Port   int
    FPS    int
    Width  int
    Height int
    BitrateKbps int
    Codec  string // "vp8" (default), "vp9", or "av1"
    HWAccel string // reserved for HW encoders (not used by AV1 here)
    VP8Speed int
    VP8Dropframe int
}

type WhepServer struct {
    cfg      Config
    mu       sync.Mutex
    sessions map[string]*session
    // NDI selection shared across sessions
    ndiName  string
    ndiURL   string
}

type session struct {
    id       string
    pc       *webrtc.PeerConnection
    sender   *webrtc.RTPSender
    track    interface{}
    stop     func()
    src      stream.Source
    cancelFunc context.CancelFunc
    codec    string
    created  time.Time
    state    string
}

func NewWhepServer(cfg Config) *WhepServer {
    // Start background NDI discovery so API can serve cached results immediately
    ndi.StartBackgroundDiscovery()
    s := &WhepServer{cfg: cfg, sessions: map[string]*session{}}
    // Preflight logs
    log.Printf("Color conversion: %s", stream.ColorConversionImpl())
    // Reset metrics at startup
    stream.ResetCounters()
    return s
}

func (s *WhepServer) RegisterRoutes(mux *http.ServeMux) {
    mux.HandleFunc("/whep", s.handleWHEPPost)
    mux.HandleFunc("/whep/", s.handleWHEPResource)
    mux.HandleFunc("/ndi/sources", s.handleNDISources)
    mux.HandleFunc("/ndi/select", s.handleNDISelect)
    mux.HandleFunc("/ndi/select_url", s.handleNDISelectURL)
    mux.HandleFunc("/config", s.handleConfig)
    mux.HandleFunc("/config/", s.handleConfig) // support trailing slash
    mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
        s.mu.Lock(); name,url := s.ndiName, s.ndiURL; sessCount := len(s.sessions)
        // build detailed session info for leak detection
        details := make([]map[string]any, 0, sessCount)
        for id, ss := range s.sessions {
            details = append(details, map[string]any{
                "id": id,
                "codec": ss.codec,
                "created": ss.created.UTC().Format(time.RFC3339),
                "pc_state": ss.state,
                "has_source": ss.src != nil,
                "has_stop": ss.stop != nil,
            })
        }
        s.mu.Unlock()
        metrics := stream.GetCounters()
        runtimeStats := stream.GetRuntimeStats()
        out := map[string]any{
            "status":   "ok",
            "sessions": sessCount,
            "ndi":      map[string]any{"selected": name, "url": url},
            "metrics":  metrics,
            "runtime":  runtimeStats,
            "sessions_detail": details,
        }
        if v, ok := metrics["frames_dropped"]; ok { out["dropped_frames"] = v }
        _ = json.NewEncoder(w).Encode(out)
    })
    mux.HandleFunc("/frame", s.handleFramePNG)
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        io.WriteString(w, indexHTML)
    })
}

func (s *WhepServer) handleWHEPPost(w http.ResponseWriter, r *http.Request) {
    if r.Method == http.MethodOptions {
        allowCORS(w, r)
        w.WriteHeader(http.StatusNoContent)
        return
    }
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    offerSDP, err := io.ReadAll(r.Body)
    if err != nil || len(offerSDP) == 0 {
        http.Error(w, "empty offer", http.StatusBadRequest)
        return
    }

    // Basic Pion configuration; ICE servers optional via env at client side.
    me := webrtc.MediaEngine{}
    if err := me.RegisterDefaultCodecs(); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    api := webrtc.NewAPI(webrtc.WithMediaEngine(&me))
    pc, err := api.NewPeerConnection(webrtc.Configuration{})
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    id := uuid.New().String()
    log.Printf("WHEP session %s: created", id)

    // Create a video track matching the selected codec
    codec := strings.ToLower(s.cfg.Codec)
    mime := webrtc.MimeTypeVP8
    switch codec {
    case "vp9":
        mime = webrtc.MimeTypeVP9
    case "av1":
        mime = webrtc.MimeTypeAV1
    default:
        codec = "vp8"
        mime = webrtc.MimeTypeVP8
    }
    videoTrack, err := webrtc.NewTrackLocalStaticSample(
        webrtc.RTPCodecCapability{MimeType: mime}, "video", "pion",
    )
    if err != nil {
        _ = pc.Close()
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    sender, err := pc.AddTrack(videoTrack)
    if err != nil {
        _ = pc.Close()
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    // Choose source: prefer NDI if env provided, else synthetic.
    fps := s.cfg.FPS
    if fps <= 0 { fps = 30 }
    var src stream.Source
    s.mu.Lock(); ndiURL := s.ndiURL; ndiName := s.ndiName; s.mu.Unlock()
    if ndiURL == "" { ndiURL = os.Getenv("NDI_SOURCE_URL") }
    if ndiName == "" { ndiName = os.Getenv("NDI_SOURCE") }
    if ndiURL != "" || ndiName != "" {
        if strings.EqualFold(ndiName, "splash") || strings.EqualFold(ndiURL, "ndi://splash") {
            // Special fake NDI source that maps to synthetic generator
            log.Printf("Using fake NDI source 'Splash' -> synthetic")
            src = nil // nil signals pipelines to use synthetic
        } else if nd, err := stream.NewNDISource(ndiURL, ndiName); err == nil {
            log.Printf("Using NDI source (url=%v, name=%v)", ndiURL != "", ndiName)
            src = nd
        } else {
            log.Printf("NDI source unavailable (%v), falling back to synthetic", err)
        }
    }
    // Start AV1/VP9/VP8 pipeline (no H.264 fallback here)
    var stopper interface{ Stop() }
    switch codec {
    case "av1":
        pipeAV1, err := stream.StartAV1Pipeline(stream.PipelineConfig{
            Width:  s.cfg.Width,
            Height: s.cfg.Height,
            FPS:    fps,
            BitrateKbps: s.cfg.BitrateKbps,
            Source: src,
            Track:  videoTrack,
        })
        if err != nil {
            _ = pc.Close()
            http.Error(w, fmt.Sprintf("AV1 pipeline error: %v", err), http.StatusInternalServerError)
            return
        }
        stopper = pipeAV1
    case "vp9":
        pipeVP9, err := stream.StartVP9Pipeline(stream.PipelineConfig{
            Width:  s.cfg.Width,
            Height: s.cfg.Height,
            FPS:    fps,
            BitrateKbps: s.cfg.BitrateKbps,
            Source: src,
            Track:  videoTrack,
        })
        if err != nil {
            _ = pc.Close()
            http.Error(w, fmt.Sprintf("VP9 pipeline error: %v", err), http.StatusInternalServerError)
            return
        }
        stopper = pipeVP9
    default: // vp8
        df := s.cfg.VP8Dropframe
        if src == nil { df = 0 } // ensure synthetic "Splash" animates reliably
        pipeVP8, err := stream.StartVP8Pipeline(stream.PipelineConfig{
            Width:  s.cfg.Width,
            Height: s.cfg.Height,
            FPS:    fps,
            BitrateKbps: s.cfg.BitrateKbps,
            Source: src,
            Track:  videoTrack,
            VP8Speed: s.cfg.VP8Speed,
            VP8Dropframe: df,
        })
        if err != nil {
            _ = pc.Close()
            http.Error(w, fmt.Sprintf("VP8 pipeline error: %v", err), http.StatusInternalServerError)
            return
        }
        stopper = pipeVP8
    }

    // Create a cancellable context for the resolution monitoring goroutine
    ctx, cancelFunc := context.WithCancel(context.Background())
    
    // Monitor for source resolution changes and restart pipeline when needed
    type sourceWithLast interface{ Last() ([]byte,int,int,bool) }
    if src != nil {
        if reporter, ok := src.(sourceWithLast); ok {
            pipeMu := &sync.Mutex{}
            currentW, currentH := s.cfg.Width, s.cfg.Height
            go func() {
                ticker := time.NewTicker(1 * time.Second)
                defer ticker.Stop()
                for {
                    select {
                    case <-ctx.Done():
                        return
                    case <-ticker.C:
                        _, w0, h0, ok := reporter.Last()
                        if !ok || w0 <= 0 || h0 <= 0 { continue }
                        // Avoid restart if unchanged
                        if w0 == currentW && h0 == currentH { continue }
                        log.Printf("Pipeline: source resolution change detected %dx%d -> %dx%d, restarting encoder", currentW, currentH, w0, h0)
                        // Restart pipeline with new size
                        pipeMu.Lock()
                        if stopper != nil { stopper.Stop() }
                        var newStop interface{ Stop() }
                        var p interface{ Stop() }
                        var err error
                        switch codec {
                        case "vp9":
                            p, err = stream.StartVP9Pipeline(stream.PipelineConfig{Width:w0, Height:h0, FPS:fps, BitrateKbps:s.cfg.BitrateKbps, Source:src, Track:videoTrack})
                        case "av1":
                            p, err = stream.StartAV1Pipeline(stream.PipelineConfig{Width:w0, Height:h0, FPS:fps, BitrateKbps:s.cfg.BitrateKbps, Source:src, Track:videoTrack})
                        default:
                            p, err = stream.StartVP8Pipeline(stream.PipelineConfig{Width:w0, Height:h0, FPS:fps, BitrateKbps:s.cfg.BitrateKbps, Source:src, Track:videoTrack, VP8Speed:s.cfg.VP8Speed, VP8Dropframe:s.cfg.VP8Dropframe})
                        }
                        newStop = p
                        if err != nil {
                            log.Printf("Pipeline: restart failed: %v", err)
                            pipeMu.Unlock()
                            continue
                        }
                        stopper = newStop
                        // Update session stop func if session is registered
                        s.mu.Lock()
                        if ss, ok := s.sessions[id]; ok && newStop != nil {
                            ss.stop = newStop.Stop
                        }
                        s.mu.Unlock()
                        currentW, currentH = w0, h0
                        pipeMu.Unlock()
                    }
                }
            }()
        }
    } else {
        // If no NDI source, create a no-op cancelFunc
        cancelFunc = func() {}
    }

    // WHEP semantics: set remote offer, answer, and wait for ICE gather complete
    if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offerSDP)}); err != nil {
        if stopper != nil { stopper.Stop() }
        _ = pc.Close()
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    answer, err := pc.CreateAnswer(nil)
    if err != nil {
        if stopper != nil { stopper.Stop() }; _ = pc.Close()
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    gatherComplete := webrtc.GatheringCompletePromise(pc)
    if err := pc.SetLocalDescription(answer); err != nil {
        if stopper != nil { stopper.Stop() }; _ = pc.Close()
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    <-gatherComplete

    sess := &session{ id: id, pc: pc, sender: sender, track: videoTrack, stop: stopper.Stop, src: src, cancelFunc: cancelFunc, codec: codec, created: time.Now() }
    s.mu.Lock(); s.sessions[id] = sess; s.mu.Unlock()

    pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
        log.Printf("Session %s state: %s", id, state)
        // Track last known state for /health
        s.mu.Lock()
        if ss, ok := s.sessions[id]; ok { ss.state = state.String() }
        s.mu.Unlock()
        if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateDisconnected {
            s.closeSession(id)
        }
    })

    // Add timeout for failed connections - clean up sessions that don't connect within 30 seconds
    go func() {
        timer := time.NewTimer(30 * time.Second)
        defer timer.Stop()
        
        select {
        case <-timer.C:
            // Check if session is still in connecting state and clean it up
            s.mu.Lock()
            if sess, exists := s.sessions[id]; exists {
                currentState := sess.pc.ConnectionState()
                if currentState == webrtc.PeerConnectionStateNew || currentState == webrtc.PeerConnectionStateConnecting {
                    log.Printf("Session %s: timeout after 30s, cleaning up (state: %s)", id, currentState)
                    s.mu.Unlock()
                    s.closeSession(id)
                    return
                }
            }
            s.mu.Unlock()
        case <-ctx.Done():
            // Session was closed before timeout
            return
        }
    }()

    allowCORS(w, r)
    w.Header().Set("Content-Type", "application/sdp")
    w.Header().Set("Location", fmt.Sprintf("/whep/%s", id))
    w.WriteHeader(http.StatusCreated)
    _, _ = io.WriteString(w, pc.LocalDescription().SDP)
}

// GET /ndi/sources -> { sources: [ { name, url } ] }
func (s *WhepServer) handleNDISources(w http.ResponseWriter, r *http.Request) {
    allowCORS(w, r)
    if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
    type Info struct{ Name string `json:"name"`; URL string `json:"url"` }
    // Serve from cache immediately for responsiveness
    srcs := ndi.GetCachedSources()
    list := make([]Info, 0, len(srcs)+1)
    // Inject fake source "Splash" which maps to the synthetic generator
    list = append(list, Info{Name: "Splash", URL: "ndi://Splash"})
    for _, si := range srcs { list = append(list, Info{Name: si.Name, URL: si.URL}) }
    _ = json.NewEncoder(w).Encode(map[string]any{"sources": list})
}

// POST /ndi/select { "source": "substring or exact name" }
func (s *WhepServer) handleNDISelect(w http.ResponseWriter, r *http.Request) {
    allowCORS(w, r)
    if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
    if r.Method != http.MethodPost { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }
    var body struct{ Source string `json:"source"` }
    dec := json.NewDecoder(r.Body)
    if err := dec.Decode(&body); err != nil || body.Source == "" {
        http.Error(w, "invalid JSON or missing 'source'", http.StatusBadRequest); return
    }
    // find best match by substring (case-insensitive)
    srcs := streamNDISources()
    selName, selURL := "", ""
    q := strings.ToLower(body.Source)
    for _, si := range srcs {
        if strings.Contains(strings.ToLower(si.Name), q) || strings.EqualFold(si.URL, body.Source) {
            selName, selURL = si.Name, si.URL
            break
        }
    }
    if selName == "" && len(srcs) > 0 { // fallback to first
        selName, selURL = srcs[0].Name, srcs[0].URL
    }
    s.mu.Lock(); s.ndiName, s.ndiURL = selName, selURL; sessions := make([]*session, 0, len(s.sessions)); for _, ss := range s.sessions { sessions = append(sessions, ss) } ; s.mu.Unlock()
    // Restart pipelines for all active sessions to apply new source immediately
    for _, ss := range sessions {
        _ = s.restartSessionPipeline(ss)
    }
    _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "selected": selName, "url": selURL})
}

// POST /ndi/select_url { "url": "ndi://..." }
func (s *WhepServer) handleNDISelectURL(w http.ResponseWriter, r *http.Request) {
    allowCORS(w, r)
    if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
    if r.Method != http.MethodPost { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }
    var body struct{ URL string `json:"url"` }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
        http.Error(w, "invalid JSON or missing 'url'", http.StatusBadRequest); return
    }
    s.mu.Lock(); s.ndiURL = body.URL; sessions := make([]*session, 0, len(s.sessions)); for _, ss := range s.sessions { sessions = append(sessions, ss) } ; s.mu.Unlock()
    // Restart pipelines for all active sessions to apply new source immediately
    for _, ss := range sessions {
        _ = s.restartSessionPipeline(ss)
    }
    _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": body.URL})
}

// helper to get NDI discovery results via cgo wrapper; returns empty list when unavailable
func streamNDISources() []struct{ Name, URL string } {
    out := []struct{ Name, URL string }{{Name: "Splash", URL: "ndi://Splash"}}
    // Use cached sources from background discovery
    for _, s := range ndi.GetCachedSources() {
        out = append(out, struct{ Name, URL string }{Name: s.Name, URL: s.URL})
    }
    return out
}

func (s *WhepServer) handleWHEPResource(w http.ResponseWriter, r *http.Request) {
    allowCORS(w, r)
    id := r.URL.Path[len("/whep/"):]
    switch r.Method {
    case http.MethodPatch:
        w.WriteHeader(http.StatusNoContent)
        return
    case http.MethodDelete:
        s.closeSession(id)
        w.WriteHeader(http.StatusNoContent)
        return
    case http.MethodOptions:
        w.WriteHeader(http.StatusNoContent)
        return
    default:
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
}

// restartSessionPipeline stops the current encoder for the session and starts a new one
// using the server's currently selected NDI source. It reuses the existing track.
func (s *WhepServer) restartSessionPipeline(ss *session) error {
    if ss == nil || ss.track == nil { return nil }
    // Build source from current selection
    s.mu.Lock(); ndiURL := s.ndiURL; ndiName := s.ndiName; s.mu.Unlock()
    if ndiURL == "" { ndiURL = os.Getenv("NDI_SOURCE_URL") }
    if ndiName == "" { ndiName = os.Getenv("NDI_SOURCE") }
    var src stream.Source
    if strings.EqualFold(ndiName, "splash") || strings.EqualFold(ndiURL, "ndi://splash") {
        src = nil // use synthetic
    } else if nd, err := stream.NewNDISource(ndiURL, ndiName); err == nil {
        src = nd
    } else {
        // fallback to synthetic if NDI unavailable
        src = nil
    }
    // Restart video pipeline only, using current codec
    fps := s.cfg.FPS; if fps <= 0 { fps = 30 }
    // Stop old
    if ss.stop != nil { ss.stop() }
    // Stop old source to avoid leaking the underlying receiver
    if ss.src != nil { ss.src.Stop() }
    // Start new (auto-detect size inside pipeline)
    var err error
    switch strings.ToLower(s.cfg.Codec) {
    case "av1":
        if p, e := stream.StartAV1Pipeline(stream.PipelineConfig{Width:s.cfg.Width, Height:s.cfg.Height, FPS:fps, BitrateKbps:s.cfg.BitrateKbps, Source:src, Track:ss.track}); e == nil {
            ss.stop = p.Stop
            ss.src = src
        } else { err = e }
    case "vp9":
        if p, e := stream.StartVP9Pipeline(stream.PipelineConfig{Width:s.cfg.Width, Height:s.cfg.Height, FPS:fps, BitrateKbps:s.cfg.BitrateKbps, Source:src, Track:ss.track}); e == nil {
            ss.stop = p.Stop
            ss.src = src
        } else { err = e }
    default:
        df := s.cfg.VP8Dropframe
        if src == nil { df = 0 }
        if p, e := stream.StartVP8Pipeline(stream.PipelineConfig{Width:s.cfg.Width, Height:s.cfg.Height, FPS:fps, BitrateKbps:s.cfg.BitrateKbps, Source:src, Track:ss.track, VP8Speed:s.cfg.VP8Speed, VP8Dropframe:df}); e == nil {
            ss.stop = p.Stop
            ss.src = src
        } else { err = e }
    }
    if err != nil { log.Printf("Pipeline restart error: %v", err); return err }
    return nil
}

func (s *WhepServer) closeSession(id string) {
    s.mu.Lock(); sess := s.sessions[id]; delete(s.sessions, id); s.mu.Unlock()
    if sess != nil {
        // Cancel the resolution monitoring goroutine first
        if sess.cancelFunc != nil {
            sess.cancelFunc()
        }
        if sess.stop != nil { sess.stop() }
        if sess.src != nil { sess.src.Stop() }
        _ = sess.pc.Close()
        log.Printf("WHEP session %s: closed", id)
    }
}

// handleFramePNG returns a single PNG frame from the currently selected NDI source.
// Query param: timeout=ms (default 2000)
func (s *WhepServer) handleFramePNG(w http.ResponseWriter, r *http.Request) {
    allowCORS(w, r)
    if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
    if r.Method != http.MethodGet { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }

    // Get timeout
    timeoutMs := 2000
    if t := r.URL.Query().Get("timeout"); t != "" {
        if v, err := strconv.Atoi(t); err == nil && v > 0 { timeoutMs = v }
    }
    deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)

    // Resolve selection
    s.mu.Lock(); ndiURL := s.ndiURL; ndiName := s.ndiName; s.mu.Unlock()
    if ndiURL == "" { ndiURL = os.Getenv("NDI_SOURCE_URL") }
    if ndiName == "" { ndiName = os.Getenv("NDI_SOURCE") }

    // If the special fake NDI "Splash" is selected, render a synthetic frame instead
    if strings.EqualFold(ndiName, "splash") || strings.EqualFold(ndiURL, "ndi://splash") {
        wpx, hpx := s.cfg.Width, s.cfg.Height
        if wpx <= 0 { wpx = 1280 }
        if hpx <= 0 { hpx = 720 }
        src := stream.NewSynthetic(wpx, hpx, 30, 1)
        buf, _ := src.Next()
        img := image.NewRGBA(image.Rect(0,0,wpx,hpx))
        for y := 0; y < hpx; y++ {
            for x := 0; x < wpx; x++ {
                si := (y*wpx + x) * 4
                di := si
                b := buf[si+0]; g := buf[si+1]; r := buf[si+2]; a := buf[si+3]
                img.Pix[di+0] = r
                img.Pix[di+1] = g
                img.Pix[di+2] = b
                img.Pix[di+3] = a
            }
        }
        w.Header().Set("Content-Type", "image/png")
        _ = png.Encode(w, img)
        return
    }

    // Create a temporary NDI source
    nd, err := stream.NewNDISource(ndiURL, ndiName)
    if err != nil {
        http.Error(w, "NDI not available or source not found", http.StatusServiceUnavailable)
        return
    }
    defer nd.Stop()

    var buf []byte; var wpx,hpx int; var ok bool
    for time.Now().Before(deadline) {
        if b, w0, h0, have := nd.Last(); have && b != nil && len(b) >= w0*h0*4 && w0>0 && h0>0 {
            buf, wpx, hpx, ok = b, w0, h0, true
            break
        }
        time.Sleep(50 * time.Millisecond)
    }
    if !ok {
        http.Error(w, "no frame available", http.StatusServiceUnavailable)
        return
    }

    // Convert BGRA to RGBA and encode PNG
    img := image.NewRGBA(image.Rect(0,0,wpx,hpx))
    // RGBA stride is 4*wpx by default
    for y := 0; y < hpx; y++ {
        for x := 0; x < wpx; x++ {
            si := (y*wpx + x) * 4
            di := si
            b := buf[si+0]; g := buf[si+1]; r := buf[si+2]; a := buf[si+3]
            img.Pix[di+0] = r
            img.Pix[di+1] = g
            img.Pix[di+2] = b
            img.Pix[di+3] = a
        }
    }

    w.Header().Set("Content-Type", "image/png")
    if err := png.Encode(w, img); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
}

func allowCORS(w http.ResponseWriter, r *http.Request) {
    origin := r.Header.Get("Origin")
    if origin == "" { origin = "*" }
    w.Header().Set("Access-Control-Allow-Origin", origin)
    w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
    w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

// handleConfig serves a simple HTML page that documents and shows current
// configuration as driven by command-line flags and environment variables.
func (s *WhepServer) handleConfig(w http.ResponseWriter, r *http.Request) {
    allowCORS(w, r)
    if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
    if r.Method != http.MethodGet { http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }

    // Snapshot environment and current runtime selections
    getenv := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
    s.mu.Lock(); selNDIName, selNDIURL := s.ndiName, s.ndiURL; s.mu.Unlock()

    // Build rows for flags (and their env equivalents)
    type row struct{ Name, Flag, Env, Value, Default, Desc string }
    rows := []row{
        {Name: "Host", Flag: "-host", Env: "HOST", Value: s.cfg.Host, Default: "0.0.0.0", Desc: "HTTP bind host"},
        {Name: "Port", Flag: "-port", Env: "PORT", Value: fmt.Sprintf("%d", s.cfg.Port), Default: "8000", Desc: "HTTP bind port"},
        {Name: "FPS", Flag: "-fps", Env: "FPS", Value: fmt.Sprintf("%d", s.cfg.FPS), Default: "30", Desc: "Frames per second (synthetic/when used)"},
        {Name: "Width", Flag: "-width", Env: "VIDEO_WIDTH", Value: fmt.Sprintf("%d", s.cfg.Width), Default: "1280", Desc: "Video width (synthetic/initial)"},
        {Name: "Height", Flag: "-height", Env: "VIDEO_HEIGHT", Value: fmt.Sprintf("%d", s.cfg.Height), Default: "720", Desc: "Video height (synthetic/initial)"},
        {Name: "Bitrate", Flag: "-bitrate", Env: "VIDEO_BITRATE_KBPS", Value: fmt.Sprintf("%d", s.cfg.BitrateKbps), Default: "6000", Desc: "Target video bitrate (kbps)"},
        {Name: "Codec", Flag: "-codec", Env: "VIDEO_CODEC", Value: s.cfg.Codec, Default: "vp8", Desc: "Video codec: vp8, vp9, av1"},
        {Name: "HW Accel", Flag: "-hwaccel", Env: "VIDEO_HWACCEL", Value: s.cfg.HWAccel, Default: "none", Desc: "Reserved; hardware encoder selection"},
        {Name: "VP8 Speed", Flag: "-vp8speed", Env: "VIDEO_VP8_SPEED", Value: fmt.Sprintf("%d", s.cfg.VP8Speed), Default: "8", Desc: "VP8 cpu_used speed (0=best, 8=fastest)"},
        {Name: "VP8 Dropframe", Flag: "-vp8dropframe", Env: "VIDEO_VP8_DROPFRAME", Value: fmt.Sprintf("%d", s.cfg.VP8Dropframe), Default: "25", Desc: "VP8 drop-frame threshold (0=off)"},
        {Name: "NDI Color", Flag: "-color", Env: "NDI_RECV_COLOR", Value: getenv("NDI_RECV_COLOR"), Default: "", Desc: "NDI receive color: bgra or uyvy"},
    }

    // Additional environment-only controls
    envOnly := []row{
        {Name: "NDI Source Name", Flag: "(n/a)", Env: "NDI_SOURCE", Value: getenv("NDI_SOURCE"), Default: "", Desc: "Preferred NDI source display name"},
        {Name: "NDI Source URL", Flag: "(n/a)", Env: "NDI_SOURCE_URL", Value: getenv("NDI_SOURCE_URL"), Default: "", Desc: "Preferred NDI source URL (ndi://...)"},
        {Name: "NDI Groups", Flag: "(n/a)", Env: "NDI_GROUPS", Value: getenv("NDI_GROUPS"), Default: "", Desc: "Comma-separated NDI groups for discovery"},
        {Name: "NDI Extra IPs", Flag: "(n/a)", Env: "NDI_EXTRA_IPS", Value: getenv("NDI_EXTRA_IPS"), Default: "", Desc: "Comma-separated unicast IPs for discovery"},
        {Name: "YUV BGRA Order", Flag: "(n/a)", Env: "YUV_BGRA_ORDER", Value: getenv("YUV_BGRA_ORDER"), Default: "", Desc: "Override BGRA byte order for converters"},
        {Name: "YUV Swap UV", Flag: "(n/a)", Env: "YUV_SWAP_UV", Value: getenv("YUV_SWAP_UV"), Default: "", Desc: "Swap U/V planes in converters (1/true)"},
    }

    // Runtime selections/info
    runtimeInfo := []row{
        {Name: "Selected NDI Name", Flag: "(runtime)", Env: "(runtime)", Value: selNDIName, Default: "", Desc: "Current selected source name"},
        {Name: "Selected NDI URL", Flag: "(runtime)", Env: "(runtime)", Value: selNDIURL, Default: "", Desc: "Current selected source URL"},
        {Name: "Color Conversion", Flag: "(build)", Env: "(build)", Value: stream.ColorConversionImpl(), Default: "", Desc: "libyuv or pure-go"},
    }

    // Render HTML
    var b strings.Builder
    b.WriteString("<!doctype html><meta charset=\"utf-8\"><title>WHEP Config</title>")
    b.WriteString(`<style>body{font-family:system-ui;margin:2rem} table{border-collapse:collapse} th,td{border:1px solid #ddd;padding:.4rem .6rem} th{background:#f5f5f5;text-align:left} code{background:#f6f8fa;padding:.1rem .25rem;border-radius:3px}</style>`)
    b.WriteString("<h1>WHEP Configuration</h1>")
    fmt.Fprintf(&b, "<p>Listening on <code>%s:%d</code>. This page lists command-line flags and environment variables that control the server.</p>", s.cfg.Host, s.cfg.Port)

    // Helper to print a table
    printTable := func(title string, list []row) {
        fmt.Fprintf(&b, "<h2>%s</h2>", title)
        b.WriteString("<table><tr><th>Name</th><th>Flag</th><th>Env</th><th>Value</th><th>Default</th><th>Description</th></tr>")
        for _, r := range list {
            fmt.Fprintf(&b, "<tr><td>%s</td><td><code>%s</code></td><td><code>%s</code></td><td><code>%s</code></td><td><code>%s</code></td><td>%s</td></tr>",
                htmlEscape(r.Name), htmlEscape(r.Flag), htmlEscape(r.Env), htmlEscape(r.Value), htmlEscape(r.Default), htmlEscape(r.Desc))
        }
        b.WriteString("</table>")
    }

    printTable("Flags + Env", rows)
    printTable("Environment Only", envOnly)
    printTable("Runtime Info", runtimeInfo)

    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    _, _ = io.WriteString(w, b.String())
}

// htmlEscape performs minimal HTML escaping for text nodes
func htmlEscape(s string) string {
    s = strings.ReplaceAll(s, "&", "&amp;")
    s = strings.ReplaceAll(s, "<", "&lt;")
    s = strings.ReplaceAll(s, ">", "&gt;")
    return s
}

const indexHTML = `<!doctype html>
<meta charset="utf-8" />
<title>WHEP Server</title>
<style>body{font-family:system-ui;margin:2rem} a{color:#0366d6;text-decoration:none} a:hover{text-decoration:underline}</style>
<h1>WHEP Server</h1>
<p>This server exposes a WHEP endpoint for receiving offers and returning answers. No player is embedded on this page.</p>
<ul>
  <li><a href="/config">/config</a> — configuration and runtime info</li>
  <li><a href="/health">/health</a> — health/metrics (JSON)</li>
  <li><code>POST /whep</code> — WHEP endpoint (send SDP offer)</li>
  <li><code>GET /frame</code> — latest frame as PNG (when available)</li>
  <li><code>GET /ndi/sources</code> — list NDI sources</li>
  <li><code>POST /ndi/select</code> — select NDI by name substring</li>
  <li><code>POST /ndi/select_url</code> — select NDI by URL</li>
<ul>`
