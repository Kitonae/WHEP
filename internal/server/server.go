package server

import (
    "encoding/json"
    "fmt"
    "io"
    "log"
    "os"
    "net/http"
    "sync"

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
    stop   func()
}

func NewWhepServer(cfg Config) *WhepServer {
    return &WhepServer{cfg: cfg, sessions: map[string]*session{}}
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

    // Create a VP8 track (we'll encode using libvpx via cgo)
    videoTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "pion")
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
    // Try libvpx VP8 pipeline first (if cgo/libvpx available); fallback to ffmpeg H.264.
    var stopper interface{ Stop() }
    pipeVP8, err := stream.StartVP8Pipeline(stream.PipelineConfig{
        Width:  s.cfg.Width,
        Height: s.cfg.Height,
        FPS:    fps,
        Source: src,
        Track:  videoTrack,
    })
    if err != nil {
        log.Printf("VP8 pipeline unavailable (%v), falling back to ffmpeg H.264", err)
        pipeH264, err2 := stream.StartH264Pipeline(stream.PipelineConfig{
            Width: s.cfg.Width, Height: s.cfg.Height, FPS: fps, Source: src, Track: videoTrack,
        })
        if err2 != nil {
            _ = pc.Close()
            http.Error(w, fmt.Sprintf("pipeline error: %v", err2), http.StatusInternalServerError)
            return
        }
        stopper = pipeH264
    } else {
        stopper = pipeVP8
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

    sess := &session{pc: pc, sender: sender, stop: stopper.Stop}
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
    srcs := streamNDISources()
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
    s.mu.Lock(); s.ndiName, s.ndiURL = selName, selURL; s.mu.Unlock()
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
    s.mu.Lock(); s.ndiURL = body.URL; s.mu.Unlock()
    _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": body.URL})
}

// helper to get NDI discovery results via cgo wrapper; returns empty list when unavailable
func streamNDISources() []struct{ Name, URL string } {
    out := []struct{ Name, URL string }{}
    // Use the ndi package when available
    for _, s := range ndi.ListSources(1000) {
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

func (s *WhepServer) closeSession(id string) {
    s.mu.Lock(); sess := s.sessions[id]; delete(s.sessions, id); s.mu.Unlock()
    if sess != nil {
        if sess.stop != nil { sess.stop() }
        _ = sess.pc.Close()
        log.Printf("WHEP session %s: closed", id)
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
