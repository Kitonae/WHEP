package stream

import (
    "log"
    "strings"
    "sync/atomic"
    "time"

    "whep/internal/ndi"
)

// NDISource wraps an NDI receiver and provides BGRA frames.
type NDISource struct {
    w, h int
    rx   *ndi.Receiver
    last atomic.Value // []byte (packed pixel data)
    quit chan struct{}
    firstLogged bool
    pixfmt string // "bgra" or "uyvy422"
    stopped int32 // atomic flag to make Stop idempotent
    // Optional output scaling requested by server (applied inside source loop when libyuv available)
    outW int
    outH int
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
        // Do a thorough discovery attempt
        var chosen string
        srcs := ndi.ListSources(2000) // single 2-second discovery
        if name == "" {
            if len(srcs) > 0 {
                chosen = srcs[0].URL
            }
        } else {
            // Try to match by name substring
            low := strings.ToLower(name)
            for _, s := range srcs {
                if strings.Contains(strings.ToLower(s.Name), low) || s.URL == name {
                    chosen = s.URL
                    break
                }
            }
        }
        if chosen == "" { return nil, ErrNDINoSource }
        rx, err = ndi.NewReceiverByURL(chosen)
        if err != nil { return nil, err }
    }
    s := &NDISource{rx: rx, quit: make(chan struct{})}
    // Register a live source for health tracking
    registerSource()
    go s.loop()
    return s, nil
}

var (
    ErrNDIUnavailable = fmtErr("NDI not available")
    ErrNDINoSource    = fmtErr("NDI source not found")
)

func (s *NDISource) loop() {
    defer unregisterSource()
    for {
        select { case <-s.quit: return; default: }
        vf, ok, err := s.rx.CaptureVideo(50)
        if err != nil { time.Sleep(50 * time.Millisecond); continue }
        if !ok { continue }
        if vf == nil || len(vf.Data) == 0 { continue }
        // Determine pixel format by FourCC and repack to contiguous buffer
        // Assume UYVY when FourCC corresponds to uyvy (most common); otherwise treat as BGRA
        isUYVY := (vf.FourCC == 0x59565955) // 'UYVY'
        if isUYVY {
            bytesPerPixel := 2
            if vf.Stride == vf.W*bytesPerPixel {
                frame := make([]byte, len(vf.Data))
                copy(frame, vf.Data)
                srcW, srcH := vf.W, vf.H
                // Apply optional scaling to BGRA output if target set
                if s.outW > 0 && s.outH > 0 && (s.outW != srcW || s.outH != srcH) {
                    // Convert -> scale -> convert back to BGRA for pipeline consumption
                    srcY := make([]byte, srcW*srcH)
                    srcU := make([]byte, (srcW/2)*(srcH/2))
                    srcV := make([]byte, (srcW/2)*(srcH/2))
                    UYVYtoI420(frame, srcW, srcH, srcY, srcU, srcV)
                    // Ensure even dims
                    dw, dh := s.outW, s.outH
                    if dw%2 != 0 { dw-- }
                    if dh%2 != 0 { dh-- }
                    if dw < 2 { dw = 2 }; if dh < 2 { dh = 2 }
                    dstY := make([]byte, dw*dh)
                    dstU := make([]byte, (dw/2)*(dh/2))
                    dstV := make([]byte, (dw/2)*(dh/2))
                    I420Scale(srcY, srcU, srcV, srcW, srcH, dstY, dstU, dstV, dw, dh)
                    out := make([]byte, dw*dh*4)
                    I420ToBGRA(dstY, dstU, dstV, dw, dh, out)
                    s.w, s.h = dw, dh
                    s.pixfmt = "bgra"
                    s.last.Store(out)
                } else {
                    s.w, s.h = srcW, srcH
                    s.pixfmt = "uyvy422"
                    s.last.Store(frame)
                }
            } else {
                w, h := vf.W, vf.H
                dst := make([]byte, w*h*bytesPerPixel)
                for y := 0; y < h; y++ {
                    srcOff := y*vf.Stride
                    dstOff := y*w*bytesPerPixel
                    copy(dst[dstOff:dstOff+w*bytesPerPixel], vf.Data[srcOff:srcOff+vf.Stride])
                }
                srcW, srcH := w, h
                if s.outW > 0 && s.outH > 0 && (s.outW != srcW || s.outH != srcH) {
                    srcY := make([]byte, srcW*srcH)
                    srcU := make([]byte, (srcW/2)*(srcH/2))
                    srcV := make([]byte, (srcW/2)*(srcH/2))
                    UYVYtoI420(dst, srcW, srcH, srcY, srcU, srcV)
                    dw, dh := s.outW, s.outH
                    if dw%2 != 0 { dw-- }
                    if dh%2 != 0 { dh-- }
                    if dw < 2 { dw = 2 }; if dh < 2 { dh = 2 }
                    dstY := make([]byte, dw*dh)
                    dstU := make([]byte, (dw/2)*(dh/2))
                    dstV := make([]byte, (dw/2)*(dh/2))
                    I420Scale(srcY, srcU, srcV, srcW, srcH, dstY, dstU, dstV, dw, dh)
                    out := make([]byte, dw*dh*4)
                    I420ToBGRA(dstY, dstU, dstV, dw, dh, out)
                    s.w, s.h = dw, dh
                    s.pixfmt = "bgra"
                    s.last.Store(out)
                } else {
                    s.w, s.h = srcW, srcH
                    s.pixfmt = "uyvy422"
                    s.last.Store(dst)
                }
            }
        } else {
            // BGRA path
            if vf.Stride == vf.W*4 {
                frame := make([]byte, len(vf.Data))
                copy(frame, vf.Data)
                srcW, srcH := vf.W, vf.H
                if s.outW > 0 && s.outH > 0 && (s.outW != srcW || s.outH != srcH) {
                    // Convert -> scale -> back to BGRA
                    srcY := make([]byte, srcW*srcH)
                    srcU := make([]byte, (srcW/2)*(srcH/2))
                    srcV := make([]byte, (srcW/2)*(srcH/2))
                    BGRAtoI420(frame, srcW, srcH, srcY, srcU, srcV)
                    dw, dh := s.outW, s.outH
                    if dw%2 != 0 { dw-- }
                    if dh%2 != 0 { dh-- }
                    if dw < 2 { dw = 2 }; if dh < 2 { dh = 2 }
                    dstY := make([]byte, dw*dh)
                    dstU := make([]byte, (dw/2)*(dh/2))
                    dstV := make([]byte, (dw/2)*(dh/2))
                    I420Scale(srcY, srcU, srcV, srcW, srcH, dstY, dstU, dstV, dw, dh)
                    out := make([]byte, dw*dh*4)
                    I420ToBGRA(dstY, dstU, dstV, dw, dh, out)
                    s.w, s.h = dw, dh
                    s.pixfmt = "bgra"
                    s.last.Store(out)
                } else {
                    s.w, s.h = srcW, srcH
                    s.pixfmt = "bgra"
                    s.last.Store(frame)
                }
            } else {
                w, h := vf.W, vf.H
                dst := make([]byte, w*h*4)
                for y := 0; y < h; y++ {
                    srcOff := y*vf.Stride
                    dstOff := y*w*4
                    copy(dst[dstOff:dstOff+w*4], vf.Data[srcOff:srcOff+vf.Stride])
                }
                srcW, srcH := w, h
                if s.outW > 0 && s.outH > 0 && (s.outW != srcW || s.outH != srcH) {
                    srcY := make([]byte, srcW*srcH)
                    srcU := make([]byte, (srcW/2)*(srcH/2))
                    srcV := make([]byte, (srcW/2)*(srcH/2))
                    BGRAtoI420(dst, srcW, srcH, srcY, srcU, srcV)
                    dw, dh := s.outW, s.outH
                    if dw%2 != 0 { dw-- }
                    if dh%2 != 0 { dh-- }
                    if dw < 2 { dw = 2 }; if dh < 2 { dh = 2 }
                    dstY := make([]byte, dw*dh)
                    dstU := make([]byte, (dw/2)*(dh/2))
                    dstV := make([]byte, (dw/2)*(dh/2))
                    I420Scale(srcY, srcU, srcV, srcW, srcH, dstY, dstU, dstV, dw, dh)
                    out := make([]byte, dw*dh*4)
                    I420ToBGRA(dstY, dstU, dstV, dw, dh, out)
                    s.w, s.h = dw, dh
                    s.pixfmt = "bgra"
                    s.last.Store(out)
                } else {
                    s.w, s.h = srcW, srcH
                    s.pixfmt = "bgra"
                    s.last.Store(dst)
                }
            }
        }
        if !s.firstLogged {
            s.firstLogged = true
            log.Printf("NDI: first frame received %dx%d FourCC=%d", vf.W, vf.H, vf.FourCC)
        }
    }
}

func (s *NDISource) Next() ([]byte, bool) {
    v := s.last.Load()
    if v == nil { return nil, true }
    buf := v.([]byte)
    // return the buffer directly; pipeline will read it before next update
    return buf, true
}

// Last returns the most recent frame buffer along with its width and height.
// The buffer is BGRA format, with stride assumed to be w*4.
func (s *NDISource) Last() ([]byte, int, int, bool) {
    v := s.last.Load()
    if v == nil { return nil, 0, 0, false }
    buf := v.([]byte)
    return buf, s.w, s.h, true
}

// PixFmt returns the current pixel format string suitable for ffmpeg rawvideo (e.g., "bgra" or "uyvy422").
func (s *NDISource) PixFmt() string {
    if s.pixfmt == "" { return "bgra" }
    return s.pixfmt
}

func (s *NDISource) Stop() {
    if atomic.CompareAndSwapInt32(&s.stopped, 0, 1) {
        close(s.quit)
        s.rx.Close()
    }
}

// SetOutputSize requests that the source rescale frames to the given size before handing to encoders.
// Only effective when built with libyuv; otherwise frames remain at native size.
func (s *NDISource) SetOutputSize(w, h int) {
    if w%2 != 0 { w-- }
    if h%2 != 0 { h-- }
    if w < 2 { w = 2 }
    if h < 2 { h = 2 }
    s.outW, s.outH = w, h
}

// tiny error without importing fmt
type tinyErr string
func (e tinyErr) Error() string { return string(e) }
func fmtErr(s string) error { return tinyErr(s) }
