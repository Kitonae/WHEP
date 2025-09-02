package main

import (
    "context"
    "flag"
    "fmt"
    "log"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "whep/internal/server"
)

func main() {
    host := flag.String("host", getEnv("HOST", "0.0.0.0"), "bind host")
    port := flag.Int("port", getEnvInt("PORT", 8000), "bind port")
    fps := flag.Int("fps", getEnvInt("FPS", 30), "synthetic fps if used")
    width := flag.Int("width", getEnvInt("VIDEO_WIDTH", 1280), "synthetic width")
    height := flag.Int("height", getEnvInt("VIDEO_HEIGHT", 720), "synthetic height")
    flag.Parse()

    cfg := server.Config{
        Host:   *host,
        Port:   *port,
        FPS:    *fps,
        Width:  *width,
        Height: *height,
    }

    mux := http.NewServeMux()
    whep := server.NewWhepServer(cfg)
    whep.RegisterRoutes(mux)

    srv := &http.Server{
        Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
        Handler:           mux,
        ReadHeaderTimeout: 10 * time.Second,
    }
    go func() {
        log.Printf("WHEP server listening on http://%s\n", srv.Addr)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Fatalf("ListenAndServe: %v", err)
        }
    }()

    sig := make(chan os.Signal, 1)
    signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
    <-sig
    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()
    _ = srv.Shutdown(ctx)
}

func getEnv(key, def string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return def
}

func getEnvInt(key string, def int) int {
    if v := os.Getenv(key); v != "" {
        var x int
        if _, err := fmt.Sscanf(v, "%d", &x); err == nil {
            return x
        }
    }
    return def
}

