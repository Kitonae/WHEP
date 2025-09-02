package ndi

import (
    "log"
    "sync"
    "time"
)

type cacheState struct {
    mu       sync.RWMutex
    sources  []SourceInfo
    started  bool
    quit     chan struct{}
}

var cs cacheState

// StartBackgroundDiscovery launches a background goroutine that refreshes the
// NDI source list periodically. Safe to call multiple times; only starts once.
func StartBackgroundDiscovery() {
    cs.mu.Lock()
    if cs.started {
        cs.mu.Unlock()
        return
    }
    cs.started = true
    cs.quit = make(chan struct{})
    cs.mu.Unlock()

    go func() {
        ticker := time.NewTicker(2 * time.Second)
        defer ticker.Stop()
        prevCount := -1
        for {
            select {
            case <-cs.quit:
                return
            case <-ticker.C:
                // Perform a thorough discovery attempt (2s)
                srcs := ListSources(2000)
                if srcs != nil {
                    // Log only when the count changes to avoid spam
                    if prevCount != len(srcs) {
                        prevCount = len(srcs)
                        log.Printf("NDI discovery: found %d source(s)", prevCount)
                    }
                    cs.mu.Lock()
                    // copy to avoid races with underlying slice
                    out := make([]SourceInfo, len(srcs))
                    copy(out, srcs)
                    cs.sources = out
                    cs.mu.Unlock()
                }
            }
        }
    }()
}

// StopBackgroundDiscovery stops the background discovery loop.
func StopBackgroundDiscovery() {
    cs.mu.Lock()
    if cs.started {
        close(cs.quit)
        cs.started = false
    }
    cs.mu.Unlock()
}

// GetCachedSources returns the most recently discovered sources.
// It returns a copy of the internal slice.
func GetCachedSources() []SourceInfo {
    cs.mu.RLock()
    defer cs.mu.RUnlock()
    out := make([]SourceInfo, len(cs.sources))
    copy(out, cs.sources)
    return out
}
