package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"whep/internal/server"
    "whep/internal/version"
)

func main() {
    showVersion := flag.Bool("version", false, "print version and exit")
	host := flag.String("host", getEnv("HOST", "0.0.0.0"), "bind host")
	port := flag.Int("port", getEnvInt("PORT", 8000), "bind port")
	fps := flag.Int("fps", getEnvInt("FPS", 30), "synthetic fps if used")
	width := flag.Int("width", getEnvInt("VIDEO_WIDTH", 1280), "synthetic width")
	height := flag.Int("height", getEnvInt("VIDEO_HEIGHT", 720), "synthetic height")
    bitrate := flag.Int("bitrate", getEnvInt("VIDEO_BITRATE_KBPS", 6000), "target video bitrate (kbps) for VP8/VP9")
    codec := flag.String("codec", getEnv("VIDEO_CODEC", "vp8"), "video codec: vp8, vp9, or av1")
    hwaccel := flag.String("hwaccel", getEnv("VIDEO_HWACCEL", "none"), "hardware encoder: none, nvenc, qsv, amf")
    vp8speed := flag.Int("vp8speed", getEnvInt("VIDEO_VP8_SPEED", 8), "VP8 cpu_used speed (0=best, 8=fastest)")
    vp8drop := flag.Int("vp8dropframe", getEnvInt("VIDEO_VP8_DROPFRAME", 25), "VP8 drop-frame threshold (0=off, higher drops more)")
    color := flag.String("color", getEnv("NDI_RECV_COLOR", ""), "NDI receive color: bgra or uyvy (overrides NDI_RECV_COLOR)")
    scaleFilter := flag.String("scaleFilter", getEnv("YUV_SCALE_FILTER", ""), "Scaling filter: NONE, LINEAR, BILINEAR, BOX (overrides YUV_SCALE_FILTER)")
    flag.Parse()

    if showVersion != nil && *showVersion {
        fmt.Println(version.String())
        return
    }

    // Apply -color override for NDI receive color format if provided
    if color != nil && *color != "" {
        c := *color
        if c == "bgra" || c == "BGRA" || c == "bgrx" || c == "BGRX" {
            _ = os.Setenv("NDI_RECV_COLOR", "BGRA")
        } else if c == "uyvy" || c == "UYVY" {
            _ = os.Setenv("NDI_RECV_COLOR", "UYVY")
        } else {
            // pass through raw to allow future formats
            _ = os.Setenv("NDI_RECV_COLOR", c)
        }
    }

    // Apply -scaleFilter override for libyuv scaling filter if provided
    if scaleFilter != nil && *scaleFilter != "" {
        v := *scaleFilter
        // Normalize to uppercase
        switch v {
        case "none", "None": v = "NONE"
        case "linear", "Linear": v = "LINEAR"
        case "bilinear", "Bilinear": v = "BILINEAR"
        case "box", "Box": v = "BOX"
        default:
            v = strings.ToUpper(v)
        }
        _ = os.Setenv("YUV_SCALE_FILTER", v)
    }

	cfg := server.Config{
		Host:        *host,
		Port:        *port,
		FPS:         *fps,
		Width:       *width,
		Height:      *height,
        BitrateKbps: *bitrate,
        Codec:       *codec,
        HWAccel:     *hwaccel,
        VP8Speed:    *vp8speed,
        VP8Dropframe:*vp8drop,
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
		log.Printf("WHEP %s listening on http://%s\n", version.String(), srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	log.Printf("Waiting for interrupt (PID=%d)...", os.Getpid())
	s := <-sig
	log.Printf("Signal received: %v, shutting down", s)
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
