# WHEP — Minimal WebRTC HTTP Egress server (Go)

WHEP serves live video over WebRTC using the WHEP pattern (HTTP egress). It captures frames from an NDI source (Windows + NDI SDK) or generates a synthetic test pattern, converts to I420, encodes (VP8/VP9/AV1), and streams to browsers via Pion.

- Small Go HTTP server with Pion WebRTC
- Pluggable sources: NDI or synthetic
- Pluggable color conversion: libyuv (SIMD) or pure‑Go
- Encoders: libvpx (VP8/VP9) or AV1 (SVT‑AV1/libaom)
- Simple WHEP endpoints, health, and NDI control APIs


## Quick Start

- Build (CPU-only VP8 with libyuv if available):
  - `go build -tags "vpx yuv" ./cmd/whep`
- Run:
  - `./whep -port 8000 -codec vp8 -bitrate 4000`
- Play in a browser:
  - Open `standalone-player.html`, set endpoint to `http://localhost:8000/whep`, click Play.

No player is embedded at `/`; it exposes links to `/config` and `/health`.

Tip: For a synthetic “Splash” source while testing NDI flows, select the NDI name `splash` (see API below).


## Endpoints

- `POST /whep` (WHEP):
  - Request body: SDP offer (non‑trickle; the player gathers ICE first)
  - Response: SDP answer text, `201 Created`, `Location` header with resource URL
  - Resource URL supports `DELETE` to end the session and `PATCH` per WHEP
- `GET /config`: HTML page with current flags/env and runtime selections
- `GET /health`: JSON with sessions, metrics, runtime stats
- `GET /frame`: Latest frame as PNG (from NDI or synthetic fallback)
- NDI control:
  - `GET /ndi/sources` → list discovered sources
  - `POST /ndi/select` with JSON `{ "name": "substring" }` → pick by display name
  - `POST /ndi/select_url` with JSON `{ "url": "ndi://..." }` → pick by URL


## CLI Flags and Env

Most flags also read from environment variables. See `/config` at runtime for a live view.

- `NDI_SOURCE`: Name of the NDI source to receive (enables NDI)
- `NDI_SOURCE_URL`: Explicit NDI URL (alternative to name)
- `ICE_SERVERS`: Comma-separated STUN/TURN URLs (e.g., `stun:stun.l.google.com:19302`)
- `FPS`, `VIDEO_WIDTH`, `VIDEO_HEIGHT`: Synthetic source configuration
- `VIDEO_MAX_BITRATE`: Optional encoder bitrate cap in bps (e.g., `1500000`)
- `VIDEO_MAX_FPS`: Optional encoder max framerate (e.g., `30`)
- `VIDEO_SCALE_DOWN_BY`: Optional fractional/float downscale factor (encoder-side)
- `VIDEO_PREFERRED_CODEC`: Preferred codec hint (`H264`, `VP8`, `VP9`)
- `NDI_RECV_TIMEOUT_MS`: NDI capture poll timeout (default `50`)
- `NDI_OUTPUT_PIXFMT`: Optional pre-conversion pixel format (e.g., `yuv420p`)
- `NDI_RECV_COLOR`: Requested NDI receiver color format (default `UYVY`)
  - Options: `UYVY`, `BGRA`/`BGRX`, `RGBA`/`RGBX`, `FASTEST`, `BEST`
- `NDI_INTERNAL_RESIZE`: If `1`, resize frames to `VIDEO_WIDTH`/`VIDEO_HEIGHT` before encode (usually keep off)
- `PORT`, `HOST`: Server bind address
- `LOG_LEVEL`: Logging level (`INFO`, `DEBUG`, etc.)
- `-host` / `HOST`: bind host (default `0.0.0.0`)
- `-port` / `PORT`: bind port (default `8000`)
- `-codec` / `VIDEO_CODEC`: `vp8` (default), `vp9`, `av1`
- `-bitrate` / `VIDEO_BITRATE_KBPS`: target kbps (default `6000`)
- `-fps` / `FPS`: frames per second for synthetic source (default `30`)
- `-width` / `VIDEO_WIDTH`, `-height` / `VIDEO_HEIGHT`: initial/synthetic size (default `1280x720`)
- `-vp8speed` / `VIDEO_VP8_SPEED`: VP8 `cpu_used` speed (0..8, default 8)
- `-vp8dropframe` / `VIDEO_VP8_DROPFRAME`: VP8 drop-frame threshold (default 25)
- `-scaleFilter` / `YUV_SCALE_FILTER`: scaler filter for libyuv down/up-scaling: `NONE`, `LINEAR`, `BILINEAR`, `BOX` (default `BOX`)
- `-color` / `NDI_RECV_COLOR`: NDI receive color `bgra` or `uyvy` (Windows + NDI)
- NDI discovery: `NDI_SOURCE`, `NDI_SOURCE_URL`, `NDI_GROUPS`, `NDI_EXTRA_IPS`


## Building

- Simple build (no native libs; pure‑Go color conversion):
  - `go build ./cmd/whep`
- With libvpx + libyuv (recommended CPU path):
  - `go build -tags "vpx yuv" ./cmd/whep`
- With AV1 (choose one backend):
  - SVT‑AV1: `go build -tags "svt yuv" ./cmd/whep`
  - libaom: `go build -tags "aom yuv" ./cmd/whep`

Windows + NDI (cgo) requires the NDI SDK. For reproducible Windows builds and third‑party static libraries, see `docs/BUILD.md`.

### Performance Tips

- Prefer encoder downscale (`VIDEO_SCALE_DOWN_BY`) over pre-scaling inside the NDI pipeline.
- Enable profiling with `NDI_PROF=1` and `STATS_LOG=1` to log conversion / scaling / encode timings.

More on dependencies and build tags: docs/DEPENDENCIES.md. Architecture overview: docs/ARCHITECTURE.md.


## Playing via the included page

`standalone-player.html` is a simple WHEP player you can open directly in a browser:

- Enter the endpoint (e.g., `http://localhost:8000/whep`)
- Optional: supply ICE servers (comma separated, e.g., `stun:stun.l.google.com:19302`)
- Click Play to post an SDP offer and receive the answer
- The page sets/uses the `Location` resource for proper DELETE on Stop

You can also provide `?endpoint=...&ice=...` as URL parameters.


## NDI usage notes (Windows)

- Install the NewTek NDI SDK and ensure headers/libs match the paths in `internal/ndi/receiver_windows.go`.
- Select a source at runtime using the NDI endpoints or env (`NDI_SOURCE`, `NDI_SOURCE_URL`).
- Set `NDI_RECV_COLOR` to `BGRA` or `UYVY` (default UYVY). Build with `-tags yuv` for SIMD conversion.


## Metrics and health

- `/health` returns JSON with session counts, dropped frames, and runtime stats
- Startup logs include the active color conversion backend: `libyuv` or `pure-go`
- Each Windows MinGW build auto-increments a build number and embeds it into the binary. Print it with `whep.exe -version`.


## Development tips

- The server restarts the encoder pipeline when the source resolution changes.
- VP8 has `-vp8speed` and `-vp8dropframe` knobs for realtime tuning.
- Combine build tags to tailor features, e.g., `-tags "vpx yuv"` or `-tags "svt yuv"`.


## License

No license file is included. If you intend to distribute binaries, add an appropriate license and ensure third‑party licenses (libvpx, libyuv, SVT‑AV1, libaom, NDI SDK) are satisfied.

