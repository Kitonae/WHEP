//go:build cgo && vpx

package stream

import (
    "sync/atomic"
    "time"

    "github.com/pion/webrtc/v3/pkg/media"
)

// StartVP8Pipeline encodes BGRA frames from Source using libvpx and feeds a Pion VP8 track.
func StartVP8Pipeline(cfg PipelineConfig) (*PipelineVP8, error) {
    if cfg.FPS <= 0 { cfg.FPS = 30 }
    if cfg.Width <= 0 { cfg.Width = 1280 }
    if cfg.Height <= 0 { cfg.Height = 720 }
    if cfg.Source == nil {
        cfg.Source = NewSynthetic(cfg.Width, cfg.Height, cfg.FPS, 1)
    }
    p := &PipelineVP8{cfg: cfg}
    if err := p.start(); err != nil { return nil, err }
    return p, nil
}

type PipelineVP8 struct {
    cfg PipelineConfig
    enc *VP8Encoder
    quit chan struct{}
    stopped int32 // 0 active, 1 stopped
}

func (p *PipelineVP8) start() error {
    // If source can report dimensions, prefer those over configured width/height
    if p.cfg.Source != nil {
        if s, ok := p.cfg.Source.(sourceWithLast); ok {
            deadline := time.Now().Add(1 * time.Second)
            for time.Now().Before(deadline) {
                if _, w, h, ok2 := s.Last(); ok2 && w > 0 && h > 0 {
                    p.cfg.Width, p.cfg.Height = w, h
                    break
                }
                time.Sleep(50 * time.Millisecond)
            }
        }
    }
    // Ensure even dimensions for I420 (4:2:0) subsampling
    if p.cfg.Width%2 != 0 { p.cfg.Width-- }
    if p.cfg.Height%2 != 0 { p.cfg.Height-- }
    if p.cfg.Width < 2 { p.cfg.Width = 2 }
    if p.cfg.Height < 2 { p.cfg.Height = 2 }
bk := p.cfg.BitrateKbps
    if bk <= 0 { bk = 6000 }
    e, err := NewVP8Encoder(VP8Config{Width: p.cfg.Width, Height: p.cfg.Height, FPS: p.cfg.FPS, BitrateKbps: bk, Speed: p.cfg.VP8Speed, Dropframe: p.cfg.VP8Dropframe})
    if err != nil { return err }
    p.enc = e
    p.quit = make(chan struct{})
    // Register pipeline as active
    registerPipeline("vp8")
    go p.loop()
    return nil
}

func (p *PipelineVP8) loop() {
    // Track active encoder lifecycle
    defer unregisterPipeline("vp8")
    defer p.enc.Close()
    y := make([]byte, p.cfg.Width*p.cfg.Height)
    u := make([]byte, (p.cfg.Width/2)*(p.cfg.Height/2))
    v := make([]byte, (p.cfg.Width/2)*(p.cfg.Height/2))
    // Detect source pixel format if provided
    var pixfmt string
    if pf, ok := p.cfg.Source.(interface{ PixFmt() string }); ok {
        pixfmt = pf.PixFmt()
    }
    if pixfmt == "" { pixfmt = "bgra" }

    ticker := time.NewTicker(time.Second / time.Duration(p.cfg.FPS))
    defer ticker.Stop()
    enqueue, stopWriter := newAsyncSampleWriter(p.cfg.Track)
    defer stopWriter()
    for {
        select { case <-p.quit: return; case <-ticker.C: }
        frame, ok := p.cfg.Source.Next()
        incFramesIn()
        if !ok { return }
        switch pixfmt {
        case "uyvy422":
            // Expect packed 4:2:2 (2 bytes per pixel)
            if len(frame) < p.cfg.Width*p.cfg.Height*2 { continue }
            UYVYtoI420(frame, p.cfg.Width, p.cfg.Height, y, u, v)
        default: // bgra
            if len(frame) < p.cfg.Width*p.cfg.Height*4 { continue }
            BGRAtoI420(frame, p.cfg.Width, p.cfg.Height, y, u, v)
        }
        packets, key, err := p.enc.EncodeI420(y, u, v)
        if err != nil { return }
        dur := time.Second / time.Duration(p.cfg.FPS)
        if len(packets) == 0 { incFramesDropped() } else { incFramesEncoded() }
        accepted := 0
        for _, au := range packets {
            if enqueue(media.Sample{Data: au, Duration: dur, Timestamp: time.Now()}) {
                accepted++
            }
            _ = key
        }
        incSamplesSent(accepted)
    }
}

func (p *PipelineVP8) Stop() {
    if p == nil { return }
    if atomic.CompareAndSwapInt32(&p.stopped, 0, 1) {
        if p.quit != nil { close(p.quit) }
    }
}
