//go:build cgo && vpx

package stream

import (
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
}

func (p *PipelineVP8) start() error {
    e, err := NewVP8Encoder(VP8Config{Width: p.cfg.Width, Height: p.cfg.Height, FPS: p.cfg.FPS, BitrateKbps: 2000})
    if err != nil { return err }
    p.enc = e
    p.quit = make(chan struct{})
    go p.loop()
    return nil
}

func (p *PipelineVP8) loop() {
    defer p.enc.Close()
    y := make([]byte, p.cfg.Width*p.cfg.Height)
    u := make([]byte, (p.cfg.Width/2)*(p.cfg.Height/2))
    v := make([]byte, (p.cfg.Width/2)*(p.cfg.Height/2))
    ticker := time.NewTicker(time.Second / time.Duration(p.cfg.FPS))
    defer ticker.Stop()
    for {
        select { case <-p.quit: return; case <-ticker.C: }
        frame, ok := p.cfg.Source.Next()
        if !ok { return }
        if len(frame) < p.cfg.Width*p.cfg.Height*4 { continue }
        BGRAtoI420(frame, p.cfg.Width, p.cfg.Height, y, u, v)
        packets, key, err := p.enc.EncodeI420(y, u, v)
        if err != nil { return }
        dur := time.Second / time.Duration(p.cfg.FPS)
        for _, au := range packets {
            if w, ok := p.cfg.Track.(interface{ WriteSample(media.Sample) error }); ok {
                _ = w.WriteSample(media.Sample{Data: au, Duration: dur, Timestamp: time.Now()})
            }
            _ = key
        }
    }
}

func (p *PipelineVP8) Stop() { close(p.quit); p.cfg.Source.Stop() }
