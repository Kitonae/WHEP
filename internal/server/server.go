package server

import (
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
    Codec  string // "vp8" (default) or "vp9"
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
    pc     *webrtc.PeerConnection
    sender *webrtc.RTPSender
    track  interface{}
    stop   func()
}

func NewWhepServer(cfg Config) *WhepServer {
    // Start background NDI discovery so API can serve cached results immediately
    ndi.StartBackgroundDiscovery()
    s := &WhepServer{cfg: cfg, sessions: map[string]*session{}}
    // Preflight logs
    log.Printf("Color conversion: %s", stream.ColorConversionImpl())
    return s
}

func (s *WhepServer) RegisterRoutes(mux *http.ServeMux) {
    mux.HandleFunc("/whep", s.handleWHEPPost)
    mux.HandleFunc("/whep/", s.handleWHEPResource)
    mux.HandleFunc("/ndi/sources", s.handleNDISources)
    mux.HandleFunc("/ndi/select", s.handleNDISelect)
    mux.HandleFunc("/ndi/select_url", s.handleNDISelectURL)
    mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
        s.mu.Lock(); name,url := s.ndiName, s.ndiURL; s.mu.Unlock()
        _ = json.NewEncoder(w).Encode(map[string]any{
            "status": "ok",
            "sessions": len(s.sessions),
            "ndi": map[string]any{"selected": name, "url": url},
        })
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
        if nd, err := stream.NewNDISource(ndiURL, ndiName); err == nil {
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
        pipeVP8, err := stream.StartVP8Pipeline(stream.PipelineConfig{
            Width:  s.cfg.Width,
            Height: s.cfg.Height,
            FPS:    fps,
            BitrateKbps: s.cfg.BitrateKbps,
            Source: src,
            Track:  videoTrack,
            VP8Speed: s.cfg.VP8Speed,
            VP8Dropframe: s.cfg.VP8Dropframe,
        })
        if err != nil {
            _ = pc.Close()
            http.Error(w, fmt.Sprintf("VP8 pipeline error: %v", err), http.StatusInternalServerError)
            return
        }
        stopper = pipeVP8
    }

    // Monitor for source resolution changes and restart pipeline when needed
    type sourceWithLast interface{ Last() ([]byte,int,int,bool) }
    if src != nil {
        if reporter, ok := src.(sourceWithLast); ok {
            pipeMu := &sync.Mutex{}
            currentW, currentH := s.cfg.Width, s.cfg.Height
            go func() {
                ticker := time.NewTicker(1 * time.Second)
                defer ticker.Stop()
                for range ticker.C {
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
                    currentW, currentH = w0, h0
                    pipeMu.Unlock()
                }
            }()
        }
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

    sess := &session{pc: pc, sender: sender, track: videoTrack, stop: stopper.Stop}
    s.mu.Lock(); s.sessions[id] = sess; s.mu.Unlock()

    pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
        log.Printf("Session %s state: %s", id, state)
        if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateDisconnected {
            s.closeSession(id)
        }
    })

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
    list := make([]Info, 0, len(srcs))
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
    out := []struct{ Name, URL string }{}
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
    if nd, err := stream.NewNDISource(ndiURL, ndiName); err == nil {
        src = nd
    } else {
        // fallback to synthetic if NDI unavailable
        src = nil
    }
    // Restart video pipeline only, using current codec
    fps := s.cfg.FPS; if fps <= 0 { fps = 30 }
    // Stop old
    if ss.stop != nil { ss.stop() }
    // Start new (auto-detect size inside pipeline)
    var err error
    switch strings.ToLower(s.cfg.Codec) {
    case "av1":
        if p, e := stream.StartAV1Pipeline(stream.PipelineConfig{Width:s.cfg.Width, Height:s.cfg.Height, FPS:fps, BitrateKbps:s.cfg.BitrateKbps, Source:src, Track:ss.track}); e == nil {
            ss.stop = p.Stop
        } else { err = e }
    case "vp9":
        if p, e := stream.StartVP9Pipeline(stream.PipelineConfig{Width:s.cfg.Width, Height:s.cfg.Height, FPS:fps, BitrateKbps:s.cfg.BitrateKbps, Source:src, Track:ss.track}); e == nil {
            ss.stop = p.Stop
        } else { err = e }
    default:
        if p, e := stream.StartVP8Pipeline(stream.PipelineConfig{Width:s.cfg.Width, Height:s.cfg.Height, FPS:fps, BitrateKbps:s.cfg.BitrateKbps, Source:src, Track:ss.track, VP8Speed:s.cfg.VP8Speed, VP8Dropframe:s.cfg.VP8Dropframe}); e == nil {
            ss.stop = p.Stop
        } else { err = e }
    }
    if err != nil { log.Printf("Pipeline restart error: %v", err); return err }
    return nil
}

func (s *WhepServer) closeSession(id string) {
    s.mu.Lock(); sess := s.sessions[id]; delete(s.sessions, id); s.mu.Unlock()
    if sess != nil {
        if sess.stop != nil { sess.stop() }
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

const indexHTML = `<!doctype html>
<meta charset="utf-8" />
<title>Pion WHEP Test</title>
<style>body{font-family:system-ui;margin:2rem}video{width:80vw;max-width:1280px;background:#000}</style>
<div>
  <input id="ep" value="/whep" style="width:30rem"/> 
  <button id="play">Play</button>
  <button id="stop" disabled>Stop</button>
  <div id="msg"></div>
</div>
<video id="v" playsinline autoplay muted></video>
<script>
let pc=null, res=null; const $=id=>document.getElementById(id);
$("play").onclick = async ()=>{
  const ep=$("ep").value; pc=new RTCPeerConnection();
  pc.ontrack = ev=>{$("v").srcObject=ev.streams[0];}
  const offer = await pc.createOffer({offerToReceiveVideo:true});
  await pc.setLocalDescription(offer);
  const resp=await fetch(ep,{method:'POST',headers:{'Content-Type':'application/sdp'},body:offer.sdp});
  res=resp.headers.get('Location'); const sdp=await resp.text();
  await pc.setRemoteDescription({type:'answer', sdp});
  $("stop").disabled=false;
}
$("stop").onclick = async ()=>{
  if(res){await fetch(res,{method:'DELETE'})} if(pc){pc.close()} $("stop").disabled=true;
}
</script>`
