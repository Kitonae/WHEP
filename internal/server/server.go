package server

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"whep/internal/stream"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"

	// optional on non-windows/no-cgo builds via indirection
	"strings"
	"whep/internal/ndi"
)

// Idle teardown for per-source mounts
const mountIdleTTL = 60 * time.Second

type Config struct {
	Host         string
	Port         int
	FPS          int
	Width        int
	Height       int
	BitrateKbps  int
	Codec        string // "vp8" (default), "vp9", or "av1"
	HWAccel      string // reserved for HW encoders (not used by AV1 here)
	VP8Speed     int
	VP8Dropframe int
}

type WhepServer struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]*session
	// NDI selection shared across sessions
	ndiName string
	ndiURL  string
	// Shared encoder pipeline so we encode once and fanout to all sessions
	shareBC     *stream.SampleBroadcaster
	shareStop   func()
	shareSrc    stream.Source
	shareCodec  string
	shareCancel context.CancelFunc // cancels resolution monitor

	// Per-source mounts: one shared pipeline per NDI source key
	mounts map[string]*ndiMount
}

type session struct {
	id         string
	pc         *webrtc.PeerConnection
	sender     *webrtc.RTPSender
	track      interface{}
	stop       func()
	src        stream.Source
	cancelFunc context.CancelFunc
	codec      string
	created    time.Time
	state      string
	detach     func() // unsubscribe from broadcaster
	mountKey   string // for per-source mount sessions
}

// ndiMount represents a per-source shared pipeline that fans out to many sessions.
type ndiMount struct {
	key         string
	name        string
	url         string
	codec       string
	width       int
	height      int
	fps         int
	bitrateKbps int
	bc          *stream.SampleBroadcaster
	stop        func()
	src         stream.Source
	cancel      context.CancelFunc
	mu          sync.Mutex
	sessions    map[string]struct{}
	idleTimer   *time.Timer
	noSessTimer *time.Timer
	created     time.Time
}

func (m *ndiMount) refCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func (m *ndiMount) addSession(id string) {
	m.mu.Lock()
	if m.sessions == nil {
		m.sessions = make(map[string]struct{})
	}
	m.sessions[id] = struct{}{}
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	if m.noSessTimer != nil {
		m.noSessTimer.Stop()
		m.noSessTimer = nil
	}
	m.mu.Unlock()
}

func (m *ndiMount) removeSession(id string, onIdle func()) {
	m.mu.Lock()
	delete(m.sessions, id)
	left := len(m.sessions)
	if left == 0 && m.idleTimer == nil {
		m.idleTimer = time.AfterFunc(mountIdleTTL, onIdle)
	}
	m.mu.Unlock()
}

func NewWhepServer(cfg Config) *WhepServer {
	// Start background NDI discovery so API can serve cached results immediately
	ndi.StartBackgroundDiscovery()
	s := &WhepServer{cfg: cfg, sessions: map[string]*session{}, mounts: map[string]*ndiMount{}}
	// Preflight logs
	log.Printf("Color conversion: %s", stream.ColorConversionImpl())
	// Reset metrics at startup
	stream.ResetCounters()
	return s
}

func (s *WhepServer) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/whep", s.handleWHEPPost)
	mux.HandleFunc("/whep/", s.handleWHEPResource)
	// Per-source WHEP mounts
	mux.HandleFunc("/whep/ndi/", s.handleWHEPNDI)
	mux.HandleFunc("/ndi/sources", s.handleNDISources)
	mux.HandleFunc("/ndi/select", s.handleNDISelect)
	mux.HandleFunc("/ndi/select_url", s.handleNDISelectURL)
	mux.HandleFunc("/config", s.handleConfig)
	mux.HandleFunc("/config/", s.handleConfig) // support trailing slash
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		name, url := s.ndiName, s.ndiURL
		sessCount := len(s.sessions)
		// build detailed session info for leak detection
		details := make([]map[string]any, 0, sessCount)
		for id, ss := range s.sessions {
			details = append(details, map[string]any{
				"id":         id,
				"codec":      ss.codec,
				"created":    ss.created.UTC().Format(time.RFC3339),
				"pc_state":   ss.state,
				"has_source": ss.src != nil,
				"has_stop":   ss.stop != nil,
			})
		}
		s.mu.Unlock()
		metrics := stream.GetCounters()
		runtimeStats := stream.GetRuntimeStats()
		out := map[string]any{
			"status":          "ok",
			"sessions":        sessCount,
			"ndi":             map[string]any{"selected": name, "url": url},
			"metrics":         metrics,
			"runtime":         runtimeStats,
			"sessions_detail": details,
		}
		if v, ok := metrics["frames_dropped"]; ok {
			out["dropped_frames"] = v
		}
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

	// Ensure a shared encoder pipeline exists for this codec and current source
	if err := s.ensureSharedPipeline(codec); err != nil {
		_ = pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Attach this session's track to the broadcaster so it receives samples
	s.mu.Lock()
	var detach func()
	if s.shareBC != nil {
		detach = s.shareBC.Add(videoTrack)
	} else {
		detach = func() {}
	}
	s.mu.Unlock()

	// WHEP semantics: set remote offer, answer, and wait for ICE gather complete
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offerSDP)}); err != nil {
		_ = pc.Close()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-gatherComplete

	// Register session (no per-session encoder; we rely on shared pipeline)
	// For legacy shared pipeline, avoid storing shared src/stop in session to prevent double-stop
	sess := &session{id: id, pc: pc, sender: sender, track: videoTrack, stop: func() {}, src: nil, cancelFunc: nil, codec: codec, created: time.Now(), detach: detach}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Session %s state: %s", id, state)
		// Track last known state for /health
		s.mu.Lock()
		if ss, ok := s.sessions[id]; ok {
			ss.state = state.String()
		}
		s.mu.Unlock()
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateDisconnected {
			s.closeSession(id)
		}
	})

	// Add timeout for failed connections - clean up sessions that don't connect within 30 seconds
	go func() {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		<-timer.C
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
	}()

	allowCORS(w, r)
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", fmt.Sprintf("/whep/%s", id))
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, pc.LocalDescription().SDP)
}

// handleWHEPNDI routes both mount creation (POST /whep/ndi/{key}) and session resource
// operations (PATCH/DELETE /whep/ndi/{key}/sessions/{id}).
func (s *WhepServer) handleWHEPNDI(w http.ResponseWriter, r *http.Request) {
	allowCORS(w, r)
	// Trim prefix
	path := strings.TrimPrefix(r.URL.Path, "/whep/ndi/")
	// Session resource path?
	parts := strings.Split(path, "/")
	if len(parts) >= 3 && parts[1] == "sessions" {
		// key := parts[0] // not needed; session close handles mount lookup
		id := parts[2]
		switch r.Method {
		case http.MethodPatch:
			// Trickle-ICE noop for now
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

	// Mount create (POST /whep/ndi/{key}?w=&h=&fps=&bitrateKbps=)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := strings.TrimSuffix(path, "/")
	if key == "" {
		http.Error(w, "missing source key", http.StatusBadRequest)
		return
	}

	offerSDP, err := io.ReadAll(r.Body)
	if err != nil || len(offerSDP) == 0 {
		http.Error(w, "empty offer", http.StatusBadRequest)
		return
	}

	// Parse variant constraints from query params
	q := r.URL.Query()
	wantW, wantH, wantFPS, wantBR := 0, 0, 0, 0
	if v := q.Get("w"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			wantW = n
		}
	}
	if v := q.Get("h"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			wantH = n
		}
	}
	if v := q.Get("fps"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			wantFPS = n
		}
	}
	if v := q.Get("bitrateKbps"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			wantBR = n
		}
	}
	// Ensure a mount exists for this source+variant
	m, err := s.ensureMount(key, wantW, wantH, wantFPS, wantBR)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Build PC and attach track to mount broadcaster
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
	codec := strings.ToLower(s.cfg.Codec)
	mime := webrtc.MimeTypeVP8
	switch codec {
	case "vp9":
		mime = webrtc.MimeTypeVP9
	case "av1":
		mime = webrtc.MimeTypeAV1
	default:
		codec = "vp8"
	}
	videoTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: mime}, "video", "pion")
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

	// Attach to broadcaster
	var detach func()
	m.mu.Lock()
	if m.bc != nil {
		detach = m.bc.Add(videoTrack)
	} else {
		detach = func() {}
	}
	m.mu.Unlock()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offerSDP)}); err != nil {
		_ = pc.Close()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-gatherComplete

	// For mount sessions, do not retain shared src/stop on the session to avoid double stops
	sess := &session{id: id, pc: pc, sender: sender, track: videoTrack, stop: func() {}, src: nil, cancelFunc: nil, codec: codec, created: time.Now(), detach: detach, mountKey: m.key}
	s.mu.Lock()
	s.sessions[id] = sess
	if mm := s.mounts[m.key]; mm != nil {
		mm.addSession(id)
	}
	s.mu.Unlock()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Session %s state: %s", id, state)
		s.mu.Lock()
		if ss, ok := s.sessions[id]; ok {
			ss.state = state.String()
		}
		s.mu.Unlock()
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateDisconnected {
			s.closeSession(id)
		}
	})

	w.Header().Set("Content-Type", "application/sdp")
	// Reflect actual encoder settings
	m.mu.Lock()
	actualW, actualH, actualFPS, actualBR := m.width, m.height, m.fps, m.bitrateKbps
	m.mu.Unlock()
	if actualW > 0 && actualH > 0 {
		w.Header().Set("X-Resolution", fmt.Sprintf("%dx%d@%d", actualW, actualH, actualFPS))
	}
	if actualBR > 0 {
		w.Header().Set("X-Bitrate-Kbps", fmt.Sprintf("%d", actualBR))
	}
	w.Header().Set("Location", fmt.Sprintf("/whep/ndi/%s/sessions/%s", key, id))
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, pc.LocalDescription().SDP)
}

// ensureMount ensures a per-source shared pipeline exists for the given key.
func (s *WhepServer) ensureMount(key string, wantW, wantH, wantFPS, wantBR int) (*ndiMount, error) {
	s.mu.Lock()
	// Compose composite key for variant reuse
	if wantFPS <= 0 {
		wantFPS = s.cfg.FPS
		if wantFPS <= 0 {
			wantFPS = 30
		}
	}
	if wantBR <= 0 {
		wantBR = s.cfg.BitrateKbps
	}
	compKey := key
	if wantW > 0 || wantH > 0 || wantFPS > 0 || wantBR > 0 {
		compKey = fmt.Sprintf("%s|w%d|h%d|f%d|b%d", key, wantW, wantH, wantFPS, wantBR)
	}
	if m, ok := s.mounts[compKey]; ok && m.bc != nil {
		s.mu.Unlock()
		return m, nil
	}
	// Resolve key to source info
	idx := s.sourceIndex()
	si, ok := idx[key]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("source not found: %s", key)
	}
	// Create new mount and start pipeline
	m := &ndiMount{key: compKey, name: si.Name, url: si.URL, codec: strings.ToLower(s.cfg.Codec), bc: stream.NewSampleBroadcaster(), sessions: map[string]struct{}{}, width: wantW, height: wantH, fps: wantFPS, bitrateKbps: wantBR, created: time.Now()}
	s.mounts[compKey] = m
	s.mu.Unlock()

	// Build NDI Source (nil for Splash synthetic)
	var src stream.Source
	if strings.EqualFold(si.Name, "splash") || strings.EqualFold(si.URL, "ndi://Splash") {
		src = nil
	} else if nd, err := stream.NewNDISource(si.URL, si.Name); err == nil {
		// If specific output size requested via mount params, ask source to scale to it
		if wantW > 0 && wantH > 0 {
			nd.SetOutputSize(wantW, wantH)
		}
		src = nd
	} else {
		// fall back to synthetic if unavailable
		src = nil
	}

	fps := m.fps
	if fps <= 0 {
		fps = s.cfg.FPS
		if fps <= 0 {
			fps = 30
		}
	}
	width := m.width
	if width <= 0 {
		width = s.cfg.Width
	}
	height := m.height
	if height <= 0 {
		height = s.cfg.Height
	}
	br := m.bitrateKbps
	if br <= 0 {
		br = s.cfg.BitrateKbps
	}
	var stopper interface{ Stop() }
	var err error
	switch m.codec {
	case "av1":
		stopper, err = stream.StartAV1Pipeline(stream.PipelineConfig{Width: width, Height: height, FPS: fps, BitrateKbps: br, Source: src, Track: m.bc})
	case "vp9":
		stopper, err = stream.StartVP9Pipeline(stream.PipelineConfig{Width: width, Height: height, FPS: fps, BitrateKbps: br, Source: src, Track: m.bc})
	default:
		df := s.cfg.VP8Dropframe
		if src == nil {
			df = 0
		}
		stopper, err = stream.StartVP8Pipeline(stream.PipelineConfig{Width: width, Height: height, FPS: fps, BitrateKbps: br, Source: src, Track: m.bc, VP8Speed: s.cfg.VP8Speed, VP8Dropframe: df})
	}
	if err != nil {
		return nil, fmt.Errorf("mount start: %w", err)
	}

	// Monitor source resolution for restarts
	ctx, cancel := context.WithCancel(context.Background())
	// If explicit target width/height provided, we avoid restarting on source resolution change;
	// the encoder/pipeline handles scaling. Otherwise, monitor and restart.
	if src != nil && (m.width == 0 || m.height == 0) {
		if reporter, ok := src.(interface {
			Last() ([]byte, int, int, bool)
		}); ok {
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
						if !ok || w0 <= 0 || h0 <= 0 {
							continue
						}
						if w0 == currentW && h0 == currentH {
							continue
						}
						log.Printf("Pipeline(mount %s): source resolution change %dx%d -> %dx%d, restarting", key, currentW, currentH, w0, h0)
						if stopper != nil {
							stopper.Stop()
						}
						var p interface{ Stop() }
						var e error
						switch m.codec {
						case "vp9":
							p, e = stream.StartVP9Pipeline(stream.PipelineConfig{Width: w0, Height: h0, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: m.bc})
						case "av1":
							p, e = stream.StartAV1Pipeline(stream.PipelineConfig{Width: w0, Height: h0, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: m.bc})
						default:
							p, e = stream.StartVP8Pipeline(stream.PipelineConfig{Width: w0, Height: h0, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: m.bc, VP8Speed: s.cfg.VP8Speed, VP8Dropframe: s.cfg.VP8Dropframe})
						}
						if e != nil {
							log.Printf("Pipeline(mount %s) restart failed: %v", key, e)
							continue
						}
						stopper = p
						// Update mount stop handle to point to the new pipeline
						m.mu.Lock()
						m.stop = stopper.Stop
						m.mu.Unlock()
						currentW, currentH = w0, h0
					}
				}
			}()
		}
	}
	m.mu.Lock()
	m.src = src
	m.stop = stopper.Stop
	m.cancel = cancel
	// Schedule provisional teardown if no session attaches shortly
	if len(m.sessions) == 0 && m.noSessTimer == nil {
		keyForTimer := m.key
		m.noSessTimer = time.AfterFunc(10*time.Second, func() { s.teardownMountIfIdle(keyForTimer) })
	}
	m.mu.Unlock()
	return m, nil
}

// teardownMountIfIdle tears down a mount when it has become idle.
func (s *WhepServer) teardownMountIfIdle(key string) {
	s.mu.Lock()
	m := s.mounts[key]
	s.mu.Unlock()
	if m == nil {
		return
	}
	if m.refCount() > 0 {
		return
	}
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	if m.stop != nil {
		m.stop()
	}
	if m.src != nil {
		m.src.Stop()
	}
	if m.bc != nil {
		m.bc.Close()
	}
	m.bc, m.stop, m.src, m.cancel = nil, nil, nil, nil
	m.mu.Unlock()
	log.Printf("Mount %s torn down (idle)", key)
	// Remove mount entry to avoid stale references
	s.mu.Lock()
	delete(s.mounts, key)
	s.mu.Unlock()
}

// sourceIndex returns a key->(Name,URL) mapping including synthetic Splash.
func (s *WhepServer) sourceIndex() map[string]struct{ Name, URL string } {
	out := map[string]struct{ Name, URL string }{}
	// Splash synthetic
	out[slugKey("Splash", "ndi://Splash")] = struct{ Name, URL string }{"Splash", "ndi://Splash"}
	for _, si := range ndi.GetCachedSources() {
		key := slugKey(si.Name, si.URL)
		out[key] = struct{ Name, URL string }{Name: si.Name, URL: si.URL}
	}
	return out
}

func slugKey(name, url string) string {
	base := url
	if base == "" {
		base = name
	}
	if base == "" {
		base = uuid.New().String()
	}
	// Lowercase and keep safe characters
	b := strings.ToLower(base)
	// Replace unsafe with '-'
	var sb strings.Builder
	for _, r := range b {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('-')
		}
	}
	s := sb.String()
	// Collapse consecutive '-'
	s = strings.Trim(s, "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if s == "" {
		s = "src"
	}
	return s
}

// ensureSharedPipeline ensures there is a single encoder running that writes to a
// broadcaster, so multiple sessions can reuse the same encoded frames.
func (s *WhepServer) ensureSharedPipeline(codec string) error {
	s.mu.Lock()
	// Tear down if codec mismatch
	if s.shareBC != nil && s.shareCodec != "" && s.shareCodec != codec {
		if s.shareCancel != nil {
			s.shareCancel()
		}
		if s.shareStop != nil {
			s.shareStop()
		}
		if s.shareSrc != nil {
			s.shareSrc.Stop()
		}
		s.shareBC.Close()
		s.shareBC, s.shareStop, s.shareSrc, s.shareCodec, s.shareCancel = nil, nil, nil, "", nil
	}
	if s.shareBC != nil {
		s.mu.Unlock()
		return nil
	}
	bc := stream.NewSampleBroadcaster()
	// Snapshot selection
	ndiURL, ndiName := s.ndiURL, s.ndiName
	s.mu.Unlock()
	if ndiURL == "" {
		ndiURL = os.Getenv("NDI_SOURCE_URL")
	}
	if ndiName == "" {
		ndiName = os.Getenv("NDI_SOURCE")
	}
	var src stream.Source
	if ndiURL != "" || ndiName != "" {
		if strings.EqualFold(ndiName, "splash") || strings.EqualFold(ndiURL, "ndi://splash") {
			log.Printf("Using fake NDI source 'Splash' -> synthetic")
			src = nil
		} else if nd, err := stream.NewNDISource(ndiURL, ndiName); err == nil {
			log.Printf("Using NDI source (url=%v, name=%v)", ndiURL != "", ndiName)
			// Pre-scale to configured pipeline size if provided
			if s.cfg.Width > 0 && s.cfg.Height > 0 {
				nd.SetOutputSize(s.cfg.Width, s.cfg.Height)
			}
			src = nd
		} else {
			log.Printf("NDI source unavailable (%v), falling back to synthetic", err)
		}
	}
	fps := s.cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	// Start pipeline -> broadcaster
	var stopper interface{ Stop() }
	var err error
	switch codec {
	case "av1":
		stopper, err = stream.StartAV1Pipeline(stream.PipelineConfig{Width: s.cfg.Width, Height: s.cfg.Height, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc})
	case "vp9":
		stopper, err = stream.StartVP9Pipeline(stream.PipelineConfig{Width: s.cfg.Width, Height: s.cfg.Height, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc})
	default:
		df := s.cfg.VP8Dropframe
		if src == nil {
			df = 0
		}
		stopper, err = stream.StartVP8Pipeline(stream.PipelineConfig{Width: s.cfg.Width, Height: s.cfg.Height, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc, VP8Speed: s.cfg.VP8Speed, VP8Dropframe: df})
	}
	if err != nil {
		return fmt.Errorf("shared pipeline start: %w", err)
	}
	// Monitor for source resolution changes
	ctx, cancel := context.WithCancel(context.Background())
	if src != nil {
		if reporter, ok := src.(interface {
			Last() ([]byte, int, int, bool)
		}); ok {
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
						if !ok || w0 <= 0 || h0 <= 0 {
							continue
						}
						if w0 == currentW && h0 == currentH {
							continue
						}
						log.Printf("Pipeline(shared): source resolution change detected %dx%d -> %dx%d, restarting encoder", currentW, currentH, w0, h0)
						if stopper != nil {
							stopper.Stop()
						}
						var p interface{ Stop() }
						var e error
						switch codec {
						case "vp9":
							p, e = stream.StartVP9Pipeline(stream.PipelineConfig{Width: w0, Height: h0, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc})
						case "av1":
							p, e = stream.StartAV1Pipeline(stream.PipelineConfig{Width: w0, Height: h0, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc})
						default:
							p, e = stream.StartVP8Pipeline(stream.PipelineConfig{Width: w0, Height: h0, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc, VP8Speed: s.cfg.VP8Speed, VP8Dropframe: s.cfg.VP8Dropframe})
						}
						if e != nil {
							log.Printf("Pipeline(shared) restart failed: %v", e)
							continue
						}
						stopper = p
						s.mu.Lock()
						s.shareStop = stopper.Stop
						s.mu.Unlock()
						currentW, currentH = w0, h0
					}
				}
			}()
		}
	}
	s.mu.Lock()
	s.shareBC, s.shareStop, s.shareSrc, s.shareCodec, s.shareCancel = bc, stopper.Stop, src, codec, cancel
	s.mu.Unlock()
	return nil
}

// restartSharedPipeline applies the current NDI selection to the running shared pipeline.
// If no pipeline exists, it is a no-op.
func (s *WhepServer) restartSharedPipeline() error {
	s.mu.Lock()
	if s.shareBC == nil {
		s.mu.Unlock()
		return nil
	}
	codec := s.shareCodec
	// Tear down existing
	if s.shareCancel != nil {
		s.shareCancel()
	}
	if s.shareStop != nil {
		s.shareStop()
	}
	if s.shareSrc != nil {
		s.shareSrc.Stop()
	}
	s.shareStop, s.shareSrc, s.shareCancel = nil, nil, nil
	bc := s.shareBC
	ndiURL, ndiName := s.ndiURL, s.ndiName
	s.mu.Unlock()
	if ndiURL == "" {
		ndiURL = os.Getenv("NDI_SOURCE_URL")
	}
	if ndiName == "" {
		ndiName = os.Getenv("NDI_SOURCE")
	}
	var src stream.Source
	if ndiURL != "" || ndiName != "" {
		if strings.EqualFold(ndiName, "splash") || strings.EqualFold(ndiURL, "ndi://splash") {
			src = nil
		} else if nd, err := stream.NewNDISource(ndiURL, ndiName); err == nil {
			if s.cfg.Width > 0 && s.cfg.Height > 0 {
				nd.SetOutputSize(s.cfg.Width, s.cfg.Height)
			}
			src = nd
		}
	}
	fps := s.cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	var stopper interface{ Stop() }
	var err error
	switch codec {
	case "av1":
		stopper, err = stream.StartAV1Pipeline(stream.PipelineConfig{Width: s.cfg.Width, Height: s.cfg.Height, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc})
	case "vp9":
		stopper, err = stream.StartVP9Pipeline(stream.PipelineConfig{Width: s.cfg.Width, Height: s.cfg.Height, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc})
	default:
		df := s.cfg.VP8Dropframe
		if src == nil {
			df = 0
		}
		stopper, err = stream.StartVP8Pipeline(stream.PipelineConfig{Width: s.cfg.Width, Height: s.cfg.Height, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc, VP8Speed: s.cfg.VP8Speed, VP8Dropframe: df})
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if src != nil {
		if reporter, ok := src.(interface {
			Last() ([]byte, int, int, bool)
		}); ok {
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
						if !ok || w0 <= 0 || h0 <= 0 {
							continue
						}
						if w0 == currentW && h0 == currentH {
							continue
						}
						log.Printf("Pipeline(shared): source resolution change detected %dx%d -> %dx%d, restarting encoder", currentW, currentH, w0, h0)
						if stopper != nil {
							stopper.Stop()
						}
						var p interface{ Stop() }
						var e error
						switch codec {
						case "vp9":
							p, e = stream.StartVP9Pipeline(stream.PipelineConfig{Width: w0, Height: h0, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc})
						case "av1":
							p, e = stream.StartAV1Pipeline(stream.PipelineConfig{Width: w0, Height: h0, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc})
						default:
							p, e = stream.StartVP8Pipeline(stream.PipelineConfig{Width: w0, Height: h0, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: bc, VP8Speed: s.cfg.VP8Speed, VP8Dropframe: s.cfg.VP8Dropframe})
						}
						if e != nil {
							log.Printf("Pipeline(shared) restart failed: %v", e)
							continue
						}
						stopper = p
						s.mu.Lock()
						s.shareStop = stopper.Stop
						s.mu.Unlock()
						currentW, currentH = w0, h0
					}
				}
			}()
		}
	}
	s.mu.Lock()
	s.shareStop, s.shareSrc, s.shareCancel = stopper.Stop, src, cancel
	s.mu.Unlock()
	return nil
}

// GET /ndi/sources -> { sources: [ { name, url } ] }
func (s *WhepServer) handleNDISources(w http.ResponseWriter, r *http.Request) {
	allowCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	type Info struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"url"`
		WHEP string `json:"whepEndpoint"`
	}
	idx := s.sourceIndex()
	list := make([]Info, 0, len(idx))
	for k, si := range idx {
		list = append(list, Info{ID: k, Name: si.Name, URL: si.URL, WHEP: "/whep/ndi/" + k})
	}
	// Keep backward-compatible shape: { sources: [ { name, url } ], mounts: [Info] }
	compat := make([]map[string]string, 0, len(list))
	for _, it := range list {
		compat = append(compat, map[string]string{"name": it.Name, "url": it.URL})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"sources": compat, "mounts": list})
}

// POST /ndi/select { "source": "substring or exact name" }
func (s *WhepServer) handleNDISelect(w http.ResponseWriter, r *http.Request) {
	allowCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Source string `json:"source"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil || body.Source == "" {
		http.Error(w, "invalid JSON or missing 'source'", http.StatusBadRequest)
		return
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
	s.mu.Lock()
	s.ndiName, s.ndiURL = selName, selURL
	s.mu.Unlock()
	// Restart shared pipeline so all sessions switch source
	_ = s.restartSharedPipeline()
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "selected": selName, "url": selURL})
}

// POST /ndi/select_url { "url": "ndi://..." }
func (s *WhepServer) handleNDISelectURL(w http.ResponseWriter, r *http.Request) {
	allowCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
		http.Error(w, "invalid JSON or missing 'url'", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.ndiURL = body.URL
	s.mu.Unlock()
	// Restart shared pipeline so all sessions switch source
	_ = s.restartSharedPipeline()
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
	if ss == nil || ss.track == nil {
		return nil
	}
	// Build source from current selection
	s.mu.Lock()
	ndiURL := s.ndiURL
	ndiName := s.ndiName
	s.mu.Unlock()
	if ndiURL == "" {
		ndiURL = os.Getenv("NDI_SOURCE_URL")
	}
	if ndiName == "" {
		ndiName = os.Getenv("NDI_SOURCE")
	}
	var src stream.Source
	if strings.EqualFold(ndiName, "splash") || strings.EqualFold(ndiURL, "ndi://splash") {
		src = nil // use synthetic
	} else if nd, err := stream.NewNDISource(ndiURL, ndiName); err == nil {
		// Ask source to pre-scale to the configured pipeline size if provided
		if s.cfg.Width > 0 && s.cfg.Height > 0 {
			nd.SetOutputSize(s.cfg.Width, s.cfg.Height)
		}
		src = nd
	} else {
		// fallback to synthetic if NDI unavailable
		src = nil
	}
	// Restart video pipeline only, using current codec
	fps := s.cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	// Stop old
	if ss.stop != nil {
		ss.stop()
	}
	// Stop old source to avoid leaking the underlying receiver
	if ss.src != nil {
		ss.src.Stop()
	}
	// Start new (auto-detect size inside pipeline)
	var err error
	switch strings.ToLower(s.cfg.Codec) {
	case "av1":
		if p, e := stream.StartAV1Pipeline(stream.PipelineConfig{Width: s.cfg.Width, Height: s.cfg.Height, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: ss.track}); e == nil {
			ss.stop = p.Stop
			ss.src = src
		} else {
			err = e
		}
	case "vp9":
		if p, e := stream.StartVP9Pipeline(stream.PipelineConfig{Width: s.cfg.Width, Height: s.cfg.Height, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: ss.track}); e == nil {
			ss.stop = p.Stop
			ss.src = src
		} else {
			err = e
		}
	default:
		df := s.cfg.VP8Dropframe
		if src == nil {
			df = 0
		}
		if p, e := stream.StartVP8Pipeline(stream.PipelineConfig{Width: s.cfg.Width, Height: s.cfg.Height, FPS: fps, BitrateKbps: s.cfg.BitrateKbps, Source: src, Track: ss.track, VP8Speed: s.cfg.VP8Speed, VP8Dropframe: df}); e == nil {
			ss.stop = p.Stop
			ss.src = src
		} else {
			err = e
		}
	}
	if err != nil {
		log.Printf("Pipeline restart error: %v", err)
		return err
	}
	return nil
}

func (s *WhepServer) closeSession(id string) {
	s.mu.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if sess != nil {
		// Cancel the resolution monitoring goroutine first
		if sess.cancelFunc != nil {
			sess.cancelFunc()
		}
		if sess.detach != nil {
			sess.detach()
		}
		if sess.stop != nil {
			sess.stop()
		}
		if sess.src != nil {
			sess.src.Stop()
		}
		_ = sess.pc.Close()
		log.Printf("WHEP session %s: closed", id)
		// Update mount refcounts if applicable
		if sess.mountKey != "" {
			s.mu.Lock()
			if m := s.mounts[sess.mountKey]; m != nil {
				m.removeSession(id, func() { s.teardownMountIfIdle(sess.mountKey) })
			}
			s.mu.Unlock()
		}
	}
	// If no more sessions, stop shared pipeline to save CPU
	s.mu.Lock()
	if len(s.sessions) == 0 && s.shareBC != nil {
		if s.shareCancel != nil {
			s.shareCancel()
		}
		if s.shareStop != nil {
			s.shareStop()
		}
		if s.shareSrc != nil {
			s.shareSrc.Stop()
		}
		s.shareBC.Close()
		s.shareBC, s.shareStop, s.shareSrc, s.shareCodec, s.shareCancel = nil, nil, nil, "", nil
		log.Printf("Shared pipeline stopped (no active sessions)")
	}
	s.mu.Unlock()
}

// handleFramePNG returns a single PNG frame from the currently selected NDI source.
// Query param: timeout=ms (default 2000)
func (s *WhepServer) handleFramePNG(w http.ResponseWriter, r *http.Request) {
	allowCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get timeout
	timeoutMs := 2000
	if t := r.URL.Query().Get("timeout"); t != "" {
		if v, err := strconv.Atoi(t); err == nil && v > 0 {
			timeoutMs = v
		}
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)

	// Resolve selection
	s.mu.Lock()
	ndiURL := s.ndiURL
	ndiName := s.ndiName
	s.mu.Unlock()
	if ndiURL == "" {
		ndiURL = os.Getenv("NDI_SOURCE_URL")
	}
	if ndiName == "" {
		ndiName = os.Getenv("NDI_SOURCE")
	}

	// If the special fake NDI "Splash" is selected, render a synthetic frame instead
	if strings.EqualFold(ndiName, "splash") || strings.EqualFold(ndiURL, "ndi://splash") {
		wpx, hpx := s.cfg.Width, s.cfg.Height
		if wpx <= 0 {
			wpx = 1280
		}
		if hpx <= 0 {
			hpx = 720
		}
		src := stream.NewSynthetic(wpx, hpx, 30, 1)
		buf, _ := src.Next()
		img := image.NewRGBA(image.Rect(0, 0, wpx, hpx))
		for y := 0; y < hpx; y++ {
			for x := 0; x < wpx; x++ {
				si := (y*wpx + x) * 4
				di := si
				b := buf[si+0]
				g := buf[si+1]
				r := buf[si+2]
				a := buf[si+3]
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

	var buf []byte
	var wpx, hpx int
	var ok bool
	for time.Now().Before(deadline) {
		if b, w0, h0, have := nd.Last(); have && b != nil && len(b) >= w0*h0*4 && w0 > 0 && h0 > 0 {
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
	img := image.NewRGBA(image.Rect(0, 0, wpx, hpx))
	// RGBA stride is 4*wpx by default
	for y := 0; y < hpx; y++ {
		for x := 0; x < wpx; x++ {
			si := (y*wpx + x) * 4
			di := si
			b := buf[si+0]
			g := buf[si+1]
			r := buf[si+2]
			a := buf[si+3]
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
	if origin == "" {
		origin = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

// handleConfig serves a simple HTML page that documents and shows current
// configuration as driven by command-line flags and environment variables.
func (s *WhepServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	allowCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Snapshot environment and current runtime selections
	getenv := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	s.mu.Lock()
	selNDIName, selNDIURL := s.ndiName, s.ndiURL
	s.mu.Unlock()

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
		{Name: "Scale Filter", Flag: "-scaleFilter", Env: "YUV_SCALE_FILTER", Value: getenv("YUV_SCALE_FILTER"), Default: "BOX", Desc: "libyuv scaler: NONE, LINEAR, BILINEAR, BOX"},
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
