package stream

import "sync/atomic"

// Global counters for simple health metrics.
// Intended to observe backpressure (e.g., dropped frames).
var (
    framesIn      atomic.Uint64 // frames pulled from Source
    framesEncoded atomic.Uint64 // frames that produced encoded output
    framesDropped atomic.Uint64 // frames that produced no output (encoder dropped)
    samplesSent   atomic.Uint64 // samples written to RTP track
)

// ResetCounters resets all metrics to zero.
func ResetCounters() {
    framesIn.Store(0)
    framesEncoded.Store(0)
    framesDropped.Store(0)
    samplesSent.Store(0)
}

// GetCounters returns a snapshot of current metrics.
func GetCounters() map[string]uint64 {
    return map[string]uint64{
        "frames_in":       framesIn.Load(),
        "frames_encoded":  framesEncoded.Load(),
        "frames_dropped":  framesDropped.Load(),
        "samples_sent":    samplesSent.Load(),
    }
}

func incFramesIn()      { framesIn.Add(1) }
func incFramesEncoded() { framesEncoded.Add(1) }
func incFramesDropped() { framesDropped.Add(1) }
func incSamplesSent(n int) {
    if n > 0 { samplesSent.Add(uint64(n)) }
}

