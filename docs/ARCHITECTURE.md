**Overview**
- Goal: Serve live video over WHEP (WebRTC HTTP Egress) with pluggable capture sources, color conversion, and encoders.
- Core ideas: small Go HTTP server (Pion WebRTC) + simple streaming pipelines; build tags select native backends.

**Data Flow**
- Source (NDI or synthetic) → Frames (BGRA or UYVY)
- Color Conversion → I420 (YUV 4:2:0)
- Encoder → Compressed frames (VP8/VP9/AV1 or H.264)
- Pion Track → WebRTC → Browser

**Components**
- `cmd/whep` (entrypoint)
  - Parses flags/env (host, port, codec, bitrate, size, VP8 speed/drop, hwaccel placeholder).
  - Creates `internal/server.WhepServer` and starts the HTTP server.

- `internal/server`
  - WHEP endpoints:
    - `POST /whep`: Accepts remote SDP offer, creates PeerConnection, adds a video track with the selected codec, starts the encoder pipeline, returns SDP answer with `201 Created` and `Location`.
    - `PATCH/DELETE /whep/{id}`: Resource operations per WHEP spec (DELETE stops the session).
  - Utilities:
    - `GET /ndi/sources`, `POST /ndi/select`, `POST /ndi/select_url`: Manage NDI source selection at runtime.
    - `GET /frame`: Snapshot PNG from the current NDI source for quick diagnostics.
    - `GET /health`: Basic status.
  - Sessions:
    - Holds `PeerConnection`, `RTPSender`, track, and a `stop` function for the active pipeline.
    - On ICE/connection failure or DELETE: calls `stop`, closes the PC, and removes session.
  - Codec selection:
    - Chooses MIME type and pipeline based on `Config.Codec` (`vp8`, `vp9`, `av1`). H.264 exists as a separate FFmpeg pipeline utility.
  - Preflight logging:
    - Logs active color conversion backend (`libyuv` or `pure-go`).

- `internal/stream`
  - Source abstraction:
    - `Source` interface: `Next() ([]byte, bool)` produces frames; optional `PixFmt() string` (e.g., `bgra`, `uyvy422`); optional `Last()` for size probing.
    - Implementations: Synthetic test pattern; NDI-backed source (Windows + NDI SDK).
  - Pipelines:
    - `PipelineConfig`: width/height/fps, bitrate, `Source`, destination `Track`, plus VP8 tuning knobs.
    - VP8/VP9 (libvpx, `-tags vpx`): `StartVP8Pipeline`, `StartVP9Pipeline` convert to I420 and encode via cgo libvpx wrappers.
    - AV1 (`-tags svt` or `-tags aom`): `StartAV1Pipeline` uses either SVT‑AV1 or libaom backend (both expose the same `AV1Encoder` API).
    - Each pipeline spawns two goroutines: one to feed frames at `FPS`, one to drain encoded outputs and write `media.Sample` to the track.
  - Color conversion:
    - `BGRAtoI420`, `UYVYtoI420`:
      - `//go:build cgo && yuv`: libyuv SIMD via cgo for high throughput.
      - `//go:build !yuv`: pure Go fallback implementation.
  - Encoders (cgo):
    - VP8/VP9: `internal/stream/vpx.go` wraps libvpx (`vpx_codec_*`). Tuned for realtime (threads, zero-lag, dropframe), with flags `-vp8speed` and `-vp8dropframe` controlling `cpu_used` and dropframe threshold.
    - AV1 (SVT‑AV1): `internal/stream/svt_av1.go` wraps `SvtAv1Enc` for realtime-friendly settings. Alternative libaom backend in `internal/stream/aom.go`.

**Build Tags and Backends**
- `vpx`: enable libvpx (VP8/VP9 cgo encoder pipelines).
- `svt`: enable SVT‑AV1 backend.
- `aom`: enable libaom AV1 backend.
- `yuv`: enable libyuv SIMD color conversion.
- `windows && cgo`: enable NDI receive via NewTek NDI SDK; non-Windows or non‑cgo uses stubs.

Combine tags to tailor builds, e.g. `-tags "vpx yuv"` for VPx with fast CPU color conversion, or `-tags "svt yuv"` for AV1.

**Session Lifecycle**
- Create: `POST /whep` → create Pion PC + track, start encoder pipeline, set remote offer, create and set local answer, return SDP.
- Maintain: pipeline loop ticks at `FPS`, pulls frames from `Source`, converts to I420, encodes, writes samples to track.
- Restart: if the `Source` reports a resolution change via `Last()`, the server stops the current pipeline and starts a new one with updated size (reuses the same track).
- Close: on PC disconnect/failure or `DELETE /whep/{id}` → stop pipeline, close PC, remove session.

**Threading Model**
- HTTP server runs request handlers per connection (standard net/http model).
- Each active session has:
  - Encoder goroutine: ticks at frame period, converts and encodes.
  - Drainer goroutine: reads encoder output and writes to the track.
- NDI discovery runs a background goroutine that refreshes the source cache periodically.

**Error Handling and Resilience**
- If an encoder init fails, the WHEP request returns `500` with an error message.
- Pipeline restarts are logged; failures leave the previous pipeline stopped and the session alive to try again on the next trigger.
- NDI discovery and capture tolerate transient errors and continue polling.

**Performance Knobs**
- VP8 (libvpx):
  - `-vp8speed` (0..8) → `cpu_used` speed.
  - `-vp8dropframe` (0..N) → `rc_dropframe_thresh`.
  - Defaults also set threads, token partitions, zero-latency, and keyframe spacing for realtime stability.
- Color conversion: build with `yuv` to use libyuv SIMD converters (significant CPU savings vs pure Go).
- Bitrate/FPS/Resolution: set via flags or env; lower values reduce CPU/network load.