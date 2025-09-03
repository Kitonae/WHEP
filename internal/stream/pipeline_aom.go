//go:build cgo && (aom || svt)

package stream

import (
    "sync/atomic"
    "time"

    "github.com/pion/webrtc/v3/pkg/media"
)

// StartAV1Pipeline encodes frames using libaom and feeds a Pion AV1 track.
func StartAV1Pipeline(cfg PipelineConfig) (*PipelineAV1, error) {
    if cfg.FPS <= 0 { cfg.FPS = 30 }
    if cfg.Width <= 0 { cfg.Width = 1280 }
    if cfg.Height <= 0 { cfg.Height = 720 }
    if cfg.Source == nil { cfg.Source = NewSynthetic(cfg.Width, cfg.Height, cfg.FPS, 1) }
    p := &PipelineAV1{cfg: cfg}
    if err := p.start(); err != nil { return nil, err }
    return p, nil
}

type PipelineAV1 struct {
    cfg PipelineConfig
    enc *AV1Encoder
    quit chan struct{}
    stopped int32
}

func (p *PipelineAV1) start() error {
    // prefer source-reported size
    if p.cfg.Source != nil {
        if s, ok := p.cfg.Source.(interface{ Last()([]byte,int,int,bool) }); ok {
            deadline := time.Now().Add(1 * time.Second)
            for time.Now().Before(deadline) {
                if _, w, h, ok2 := s.Last(); ok2 && w>0 && h>0 { p.cfg.Width, p.cfg.Height = w, h; break }
                time.Sleep(50 * time.Millisecond)
            }
        }
    }
    bk := p.cfg.BitrateKbps; if bk <= 0 { bk = 6000 }
    e, err := NewAV1Encoder(AV1Config{Width:p.cfg.Width, Height:p.cfg.Height, FPS:p.cfg.FPS, BitrateKbps:bk})
    if err != nil { return err }
    p.enc = e
    p.quit = make(chan struct{})
    go p.loop()
    return nil
}

func (p *PipelineAV1) loop() {
    defer p.enc.Close()
    y := make([]byte, p.cfg.Width*p.cfg.Height)
    u := make([]byte, (p.cfg.Width/2)*(p.cfg.Height/2))
    v := make([]byte, (p.cfg.Width/2)*(p.cfg.Height/2))
    var pixfmt string
    if pf, ok := p.cfg.Source.(interface{ PixFmt() string }); ok { pixfmt = pf.PixFmt() }
    if pixfmt == "" { pixfmt = "bgra" }
    ticker := time.NewTicker(time.Second / time.Duration(p.cfg.FPS))
    defer ticker.Stop()
    for {
        select { case <-p.quit: return; case <-ticker.C: }
        frame, ok := p.cfg.Source.Next(); if !ok { return }
        switch pixfmt {
        case "uyvy422":
            if len(frame) < p.cfg.Width*p.cfg.Height*2 { continue }
            UYVYtoI420(frame, p.cfg.Width, p.cfg.Height, y, u, v)
        default:
            if len(frame) < p.cfg.Width*p.cfg.Height*4 { continue }
            BGRAtoI420(frame, p.cfg.Width, p.cfg.Height, y, u, v)
        }
        packets, key, err := p.enc.EncodeI420(y,u,v); if err != nil { return }
        dur := time.Second / time.Duration(p.cfg.FPS)
        for _, au := range packets {
            if w, ok := p.cfg.Track.(interface{ WriteSample(media.Sample) error }); ok {
                _ = w.WriteSample(media.Sample{Data: au, Duration: dur, Timestamp: time.Now()})
            }
            _ = key
        }
    }
}

func (p *PipelineAV1) Stop() {
    if p == nil { return }
    if atomic.CompareAndSwapInt32(&p.stopped, 0, 1) {
        if p.quit != nil { close(p.quit) }
    }
}
