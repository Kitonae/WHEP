package stream

import (
    "runtime"
    "sync/atomic"
)

// Global counters for simple health metrics and runtime tracking.
// Intended to observe backpressure (e.g., dropped frames) and detect leaks.
var (
    // Frame/packet counters
    framesIn      atomic.Uint64 // frames pulled from Source
    framesEncoded atomic.Uint64 // frames that produced encoded output
    framesDropped atomic.Uint64 // frames that produced no output (encoder dropped)
    samplesSent   atomic.Uint64 // samples written to RTP track

    // Runtime resource counters
    activePipelines atomic.Uint64 // total pipelines (any codec)
    activeVP8       atomic.Uint64
    activeVP9       atomic.Uint64
    activeAV1       atomic.Uint64
    activeSources   atomic.Uint64 // total live Sources (e.g., NDI receivers)
)

// ResetCounters resets all metrics to zero.
func ResetCounters() {
    framesIn.Store(0)
    framesEncoded.Store(0)
    framesDropped.Store(0)
    samplesSent.Store(0)
    // Keep runtime counters as-is; they represent live objects.
}

// GetCounters returns a snapshot of current frame/packet metrics.
func GetCounters() map[string]uint64 {
    return map[string]uint64{
        "frames_in":       framesIn.Load(),
        "frames_encoded":  framesEncoded.Load(),
        "frames_dropped":  framesDropped.Load(),
        "samples_sent":    samplesSent.Load(),
    }
}

// GetRuntimeStats returns counts useful to spot orphaned routines/resources.
func GetRuntimeStats() map[string]uint64 {
    return map[string]uint64{
        "active_pipelines": activePipelines.Load(),
        "active_vp8":       activeVP8.Load(),
        "active_vp9":       activeVP9.Load(),
        "active_av1":       activeAV1.Load(),
        "active_sources":   activeSources.Load(),
        "goroutines":       uint64(runtime.NumGoroutine()),
    }
}

// Internal helpers used by pipelines/sources
func incFramesIn()      { framesIn.Add(1) }
func incFramesEncoded() { framesEncoded.Add(1) }
func incFramesDropped() { framesDropped.Add(1) }
func incSamplesSent(n int) { if n > 0 { samplesSent.Add(uint64(n)) } }

func registerPipeline(codec string) {
    activePipelines.Add(1)
    switch codec {
    case "vp8": activeVP8.Add(1)
    case "vp9": activeVP9.Add(1)
    case "av1": activeAV1.Add(1)
    }
}
func unregisterPipeline(codec string) {
    // Use Add(^uint64(0)) to subtract 1 safely even if already 0 (best effort)
    activePipelines.Add(^uint64(0))
    switch codec {
    case "vp8": activeVP8.Add(^uint64(0))
    case "vp9": activeVP9.Add(^uint64(0))
    case "av1": activeAV1.Add(^uint64(0))
    }
}
func registerSource()   { activeSources.Add(1) }
func unregisterSource() { activeSources.Add(^uint64(0)) }

