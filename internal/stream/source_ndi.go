package stream

import (
    "strings"
    "sync/atomic"
    "time"

    "whep/internal/ndi"
)

// NDISource wraps an NDI receiver and provides BGRA frames.
type NDISource struct {
    w, h int
    rx   *ndi.Receiver
    last atomic.Value // []byte (BGRA)
    quit chan struct{}
}

// NewNDISource selects a source by URL if provided, else by name substring, else first available.
func NewNDISource(url, name string) (*NDISource, error) {
    if !ndi.Initialize() { return nil, ErrNDIUnavailable }
    var rx *ndi.Receiver
    var err error
    if url != "" {
        rx, err = ndi.NewReceiverByURL(url)
        if err != nil { return nil, err }
    } else {
        // Try to match by name substring
        var chosen string
        for i := 0; i < 6 && chosen == ""; i++ { // up to ~3s
            srcs := ndi.ListSources(500)
            if name == "" && len(srcs) > 0 { chosen = srcs[0].URL; break }
            low := strings.ToLower(name)
            for _, s := range srcs {
                if strings.Contains(strings.ToLower(s.Name), low) || s.URL == name {
                    chosen = s.URL; break
                }
            }
        }
        if chosen == "" { return nil, ErrNDINoSource }
        rx, err = ndi.NewReceiverByURL(chosen)
        if err != nil { return nil, err }
    }
    s := &NDISource{rx: rx, quit: make(chan struct{})}
    go s.loop()
    return s, nil
}

var (
    ErrNDIUnavailable = fmtErr("NDI not available")
    ErrNDINoSource    = fmtErr("NDI source not found")
)

func (s *NDISource) loop() {
    for {
        select { case <-s.quit: return; default: }
        vf, ok, err := s.rx.CaptureVideo(50)
        if err != nil { time.Sleep(50 * time.Millisecond); continue }
        if !ok { continue }
        if vf == nil || len(vf.Data) == 0 { continue }
        // Copy to avoid holding onto C buffer (we already copied in receiver)
        frame := make([]byte, len(vf.Data))
        copy(frame, vf.Data)
        s.w, s.h = vf.W, vf.H
        s.last.Store(frame)
    }
}

func (s *NDISource) Next() ([]byte, bool) {
    v := s.last.Load()
    if v == nil { return nil, true }
    buf := v.([]byte)
    // return the buffer directly; pipeline will read it before next update
    return buf, true
}

func (s *NDISource) Stop() { close(s.quit); s.rx.Close() }

// tiny error without importing fmt
type tinyErr string
func (e tinyErr) Error() string { return string(e) }
func fmtErr(s string) error { return tinyErr(s) }
