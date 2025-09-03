package stream

import (
    "github.com/pion/webrtc/v3/pkg/media"
)

// asyncSampleWriter provides a small buffered, asynchronous wrapper around
// TrackLocalStaticSample.WriteSample so encoder loops don't block on network
// backpressure. Writes are best-effort; if the queue is full, the sample is dropped.
type asyncSampleWriter struct {
    ch   chan media.Sample
    quit chan struct{}
}

// newAsyncSampleWriter starts a writer goroutine if the provided track supports
// WriteSample(media.Sample) and returns a non-blocking enqueue function along
// with a stop function. If the track doesn't implement WriteSample, enqueues
// will be treated as no-ops and return false.
func newAsyncSampleWriter(track interface{}) (enqueue func(media.Sample) bool, stop func()) {
    w, ok := track.(interface{ WriteSample(media.Sample) error })
    if !ok {
        // No-op implementation
        return func(media.Sample) bool { return false }, func() {}
    }
    aw := &asyncSampleWriter{ ch: make(chan media.Sample, 4), quit: make(chan struct{}) }
    go func() {
        for {
            select {
            case s := <-aw.ch:
                _ = w.WriteSample(s)
            case <-aw.quit:
                return
            }
        }
    }()
    return func(s media.Sample) bool {
        select {
        case aw.ch <- s:
            return true
        default:
            return false
        }
    }, func() { close(aw.quit) }
}

