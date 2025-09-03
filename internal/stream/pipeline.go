package stream

import (
    "time"
)

// PipelineConfig defines how to produce encoded video and feed a Pion Track.
type PipelineConfig struct {
    Width, Height int
    FPS           int
    BitrateKbps   int // used by VP8/VP9/AV1 pipelines
    Source        Source
    // Track expects a Pion track with WriteSample(media.Sample) (e.g., *webrtc.TrackLocalStaticSample).
    Track         interface{}
    // Optional VP8 tuning (ignored by other codecs)
    VP8Speed      int // maps to libvpx VP8E_SET_CPUUSED
    VP8Dropframe  int // maps to rc_dropframe_thresh
}

// optional capability: source can advertise its pixel format (e.g., "bgra", "uyvy422")
type sourcePixFmt interface{ PixFmt() string }

// Source produces raw BGRA frames of fixed size and FPS.
type Source interface {
    // Next returns a frame of BGRA bytes (len = width*height*4) and a boolean false if source is closed.
    Next() ([]byte, bool)
    Stop()
}

// SourceSynthetic generates a moving gradient pattern.
type SourceSynthetic int64

func (s SourceSynthetic) Next() ([]byte, bool) { return nil, true }
func (s SourceSynthetic) Stop()                {}

// optional capability: some sources can report their last frame size
type sourceWithLast interface{ Last() ([]byte,int,int,bool) }

// --- Synthetic source ---

type synthetic struct {
    w, h, fps int
    buf []byte
    t0 time.Time
    stop bool
}

func NewSynthetic(w, h, fps int, seed int64) Source {
    return &synthetic{w:w, h:h, fps:fps, buf:make([]byte, w*h*4), t0: time.Now()}
}

func (s *synthetic) Next() ([]byte, bool) {
    if s.stop { return nil, false }
    // simple moving gradient pattern in BGRA
    now := time.Since(s.t0).Seconds()
    for y:=0; y<s.h; y++ {
        for x:=0; x<s.w; x++ {
            off := (y*s.w + x) * 4
            r := byte((x + int(now*120)) % 256)
            g := byte((y + int(now*80)) % 256)
            b := byte((x+y + int(now*100)) % 256)
            s.buf[off+0] = b
            s.buf[off+1] = g
            s.buf[off+2] = r
            s.buf[off+3] = 255
        }
    }
    return s.buf, true
}

func (s *synthetic) Stop() { s.stop = true }
