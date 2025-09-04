package stream

import (
    "sync"
    "github.com/pion/webrtc/v3/pkg/media"
)

// SampleBroadcaster fanouts encoded media.Sample writes to multiple sinks.
// Each sink gets its own small queue so a slow connection doesn't block others.
type SampleBroadcaster struct {
    mu    sync.RWMutex
    sinks map[*sink]struct{}
}

type sink struct {
    ch   chan media.Sample
    quit chan struct{}
    w    interface{ WriteSample(media.Sample) error }
}

// NewSampleBroadcaster creates a broadcaster. Call Close when done.
func NewSampleBroadcaster() *SampleBroadcaster {
    return &SampleBroadcaster{ sinks: make(map[*sink]struct{}) }
}

// Add registers a track-like sink (must implement WriteSample). Returns a
// function to remove the sink when the session ends. If the provided track
// doesn't implement WriteSample, the returned remove is a no-op.
func (b *SampleBroadcaster) Add(track interface{}) (remove func()) {
    w, ok := track.(interface{ WriteSample(media.Sample) error })
    if !ok {
        return func() {}
    }
    s := &sink{ ch: make(chan media.Sample, 4), quit: make(chan struct{}), w: w }
    go func() {
        for {
            select {
            case sm := <-s.ch:
                _ = s.w.WriteSample(sm)
            case <-s.quit:
                return
            }
        }
    }()
    b.mu.Lock()
    if b.sinks == nil { b.sinks = make(map[*sink]struct{}) }
    b.sinks[s] = struct{}{}
    b.mu.Unlock()
    return func() {
        b.mu.Lock()
        if _, ok := b.sinks[s]; ok {
            delete(b.sinks, s)
            close(s.quit)
        }
        b.mu.Unlock()
    }
}

// WriteSample implements WriteSample so the broadcaster can be used anywhere a
// TrackLocalStaticSample would be accepted by our pipelines.
func (b *SampleBroadcaster) WriteSample(sm media.Sample) error {
    b.mu.RLock()
    for s := range b.sinks {
        select {
        case s.ch <- sm:
        default:
            // Drop if the sink's queue is full
        }
    }
    b.mu.RUnlock()
    return nil
}

// Close stops all sink workers and clears the list.
func (b *SampleBroadcaster) Close() {
    b.mu.Lock()
    for s := range b.sinks {
        select { case <-s.quit: default: close(s.quit) }
        delete(b.sinks, s)
    }
    b.mu.Unlock()
}
