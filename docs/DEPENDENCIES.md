**Overview**
- Purpose: Lists all build/runtime dependencies and how they’re used.
- Build tags: Features are enabled by Go build tags to keep the core portable.

now:
pion, libvpx, libyuv, SVT‑AV1, libaom, NDI SDK DLL

minimal:
pion, libvpx, NDI SDK DLL


**Go Modules**
- webrtc: `github.com/pion/webrtc/v3`
- rtp/rtcp: `github.com/pion/rtp`, `github.com/pion/rtcp`
- utils: `github.com/google/uuid` and Pion subpackages declared as indirect in `go.mod`.

**Native Libraries (by build tag)**
- `vpx` (VP8/VP9 encode via libvpx):
  - Headers: `vpx/vpx_encoder.h`, `vpx/vp8cx.h`, `vpx/vp9cx.h`
  - Link: `-lvpx`
  - Used by: `internal/stream/vpx.go`, `internal/stream/pipeline_vpx*.go`
  - Enable: build with `-tags vpx`

- `yuv` (SIMD color conversion via libyuv):
  - Headers: `libyuv.h`
  - Link: `-lyuv`
  - Used by: `internal/stream/yuv_conv_cgo.go` (BGRA/UYVY → I420)
  - Enable: build with `-tags yuv`
  - Fallback: pure Go converters in `internal/stream/bgra_i420.go` and `uyvy_i420.go` (compiled when `!yuv`).

- `svt` (AV1 encode via SVT‑AV1):
  - Headers: `EbSvtAv1Enc.h`
  - Link: `-lSvtAv1Enc`
  - Used by: `internal/stream/svt_av1.go`
  - Enable: build with `-tags svt`

- `aom` (AV1 encode via libaom):
  - Headers: `aom/aom_encoder.h`, `aom/aomcx.h`
  - Link: `-laom`
  - Used by: `internal/stream/aom.go`
  - Enable: build with `-tags aom`

- `windows + cgo` (NDI receive via NewTek NDI SDK):
  - Headers: `Processing.NDI.Lib.h` (from NDI SDK)
  - Link (Windows): `-lProcessing.NDI.Lib.x64` (NDI SDK `Lib/x64`)
  - Used by: `internal/ndi/receiver_windows.go`
  - Enable: build on Windows with cgo enabled. A stub (`receiver_stub.go`) is used on non‑Windows or without cgo.
  - Note: The repo includes `Processing.NDI.Lib.x64.dll` for runtime on Windows. You still need the SDK installed (or adjust `#cgo CFLAGS/LDFLAGS` paths) to build.crea

**Build Tag Matrix**
- VP8/VP9: `-tags vpx`
- AV1 (SVT‑AV1): `-tags svt`
- AV1 (libaom): `-tags aom`
- libyuv SIMD: `-tags yuv`
- Combine as needed, e.g.: `-tags "vpx yuv"`, `-tags "svt yuv"`, `-tags "vpx svt yuv"`.

**Example Builds**
- VP8/VP9 with libyuv (recommended CPU path):
  - `go build -tags "vpx yuv" ./cmd/whep`

- AV1 via SVT‑AV1 with libyuv:
  - `go build -tags "svt yuv" ./cmd/whep`

- AV1 via libaom (CPU‑only):
  - `go build -tags "aom yuv" ./cmd/whep`

- Windows with NDI (cgo):
  - Install NDI SDK (e.g., NDI 6). Ensure headers and libs match the `#cgo` include/lib paths in `internal/ndi/receiver_windows.go`.
  - Build: `go build -tags "vpx yuv" ./cmd/whep`

**Runtime Notes**
- Color conversion backend is logged at startup:
  - `Color conversion: libyuv` when built with `yuv`.
  - `Color conversion: pure-go` otherwise.
- For AV1: choose between SVT‑AV1 (`svt`) and libaom (`aom`) at build time. Do not enable both simultaneously unless you intend to keep both backends compiled.

**Environment Variables (selected)**
- `VIDEO_CODEC`: `vp8` (default), `vp9`, `av1`
- `VIDEO_BITRATE_KBPS`: target kbps for VPx/AV1
- `VIDEO_VP8_SPEED`: 0..8 (faster at higher values)
- `VIDEO_VP8_DROPFRAME`: 0 disables, higher drops more when overloaded
- `NDI_RECV_COLOR`: `UYVY` (default) or `BGRA` (Windows + NDI)


