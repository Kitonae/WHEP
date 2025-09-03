//go:build cgo

package stream

/*
#cgo LDFLAGS: -lvpx

#include <stdlib.h>
#include <string.h>
#include <vpx/vpx_encoder.h>
#include <vpx/vp8cx.h>

// Helpers to call vararg controls from Go safely (VP8)
static int set_vp8_cpuused(vpx_codec_ctx_t *ctx, int v) { return vpx_codec_control(ctx, VP8E_SET_CPUUSED, v); }
static int set_vp8_static_threshold(vpx_codec_ctx_t *ctx, int v) { return vpx_codec_control(ctx, VP8E_SET_STATIC_THRESHOLD, v); }
static int set_vp8_token_partitions(vpx_codec_ctx_t *ctx, int v) { return vpx_codec_control(ctx, VP8E_SET_TOKEN_PARTITIONS, v); }
static int set_vp8_noise_sensitivity(vpx_codec_ctx_t *ctx, int v) { return vpx_codec_control(ctx, VP8E_SET_NOISE_SENSITIVITY, v); }
static int set_vp8_sharpness(vpx_codec_ctx_t *ctx, int v) { return vpx_codec_control(ctx, VP8E_SET_SHARPNESS, v); }

static vpx_codec_iface_t* vpx_iface_vp8() { return vpx_codec_vp8_cx(); }
static vpx_codec_iface_t* vpx_iface_vp9() { return vpx_codec_vp9_cx(); }

typedef struct frame_data {
    void *buf;
    size_t sz;
    vpx_codec_pts_t pts;
    unsigned long duration;
    vpx_codec_frame_flags_t flags;
} frame_data_t;
*/
import "C"

import (
    "errors"
    "fmt"
    "runtime"
    "unsafe"
)

type VP8Encoder struct {
    ctx   C.vpx_codec_ctx_t
    cfg   C.vpx_codec_enc_cfg_t
    img   *C.vpx_image_t
    w, h  int
    fps   int
    pts   C.vpx_codec_pts_t
    open  bool
}

type VP8Config struct {
    Width, Height int
    FPS           int
    BitrateKbps   int // target bitrate
    Speed         int // cpu_used (0..8)
    Dropframe     int // rc_dropframe_thresh
}

func NewVP8Encoder(cfg VP8Config) (*VP8Encoder, error) {
    if cfg.Width <= 0 || cfg.Height <= 0 || cfg.FPS <= 0 {
        return nil, errors.New("invalid VP8 encoder config")
    }
    e := &VP8Encoder{w: cfg.Width, h: cfg.Height, fps: cfg.FPS}
    if C.vpx_codec_enc_config_default(C.vpx_iface_vp8(), &e.cfg, 0) != C.VPX_CODEC_OK {
        return nil, errors.New("vpx_codec_enc_config_default failed")
    }
    e.cfg.g_w = C.uint(cfg.Width)
    e.cfg.g_h = C.uint(cfg.Height)
    e.cfg.g_timebase.num = 1
    e.cfg.g_timebase.den = C.int(cfg.FPS)
    if cfg.BitrateKbps > 0 {
        e.cfg.rc_target_bitrate = C.uint(cfg.BitrateKbps)
    }
    // realtime settings tuned for speed
    e.cfg.g_pass = C.VPX_RC_ONE_PASS
    // Use available CPUs (cap to avoid diminishing returns)
    threads := runtime.NumCPU(); if threads < 1 { threads = 1 }; if threads > 16 { threads = 16 }
    e.cfg.g_threads = C.uint(threads)
    e.cfg.rc_end_usage = C.VPX_CBR
    // Allow dropping frames under sustained overload
    if cfg.Dropframe > 0 { e.cfg.rc_dropframe_thresh = C.uint(cfg.Dropframe) } else { e.cfg.rc_dropframe_thresh = C.uint(0) }
    // Zero-latency pipeline
    e.cfg.g_lag_in_frames = 0
    // Space keyframes to reduce spikes
    e.cfg.kf_mode = C.VPX_KF_AUTO
    e.cfg.kf_min_dist = 0
    e.cfg.kf_max_dist = C.uint(cfg.FPS * 4)

    if st := C.vpx_codec_enc_init_ver(&e.ctx, C.vpx_iface_vp8(), &e.cfg, 0, C.VPX_ENCODER_ABI_VERSION); st != C.VPX_CODEC_OK {
        // Try to extract detailed error message from context
        errStr := C.GoString(C.vpx_codec_err_to_string(st))
        // Some detail may be available on the context even on init failure
        more := C.GoString(C.vpx_codec_error_detail(&e.ctx))
        if more != "" { errStr = fmt.Sprintf("%s: %s", errStr, more) }
        return nil, fmt.Errorf("vpx_codec_enc_init_ver failed (%dx%d@%dfps, %dkbps): %s", cfg.Width, cfg.Height, cfg.FPS, cfg.BitrateKbps, errStr)
    }
    // Apply speed-focused controls
    spd := cfg.Speed
    if spd < 0 { spd = 0 }
    if spd > 8 { spd = 8 }
    _ = C.set_vp8_cpuused(&e.ctx, C.int(spd))
    // Use maximum token partitions when supported (3 == VP8_EIGHT_TOKENPARTITIONS)
    _ = C.set_vp8_token_partitions(&e.ctx, C.int(3))
    _ = C.set_vp8_static_threshold(&e.ctx, 100)
    _ = C.set_vp8_noise_sensitivity(&e.ctx, 0)
    _ = C.set_vp8_sharpness(&e.ctx, 0)
    // Allocate I420 image buffer owned by libvpx
    e.img = C.vpx_img_alloc(nil, C.VPX_IMG_FMT_I420, C.uint(e.w), C.uint(e.h), 1)
    if e.img == nil {
        e.Close()
        return nil, errors.New("vpx_img_alloc failed")
    }
    e.open = true
    return e, nil
}

// EncodeI420 encodes a single frame. y should be size w*h, u and v size w/2*h/2.
func (e *VP8Encoder) EncodeI420(y, u, v []byte) (out [][]byte, keyframe bool, err error) {
    if !e.open { return nil, false, errors.New("encoder closed") }
    // copy into e.img planes considering stride
    yw := int(e.img.stride[0])
    uh := e.h / 2
    uw := int(e.img.stride[1])
    vw := int(e.img.stride[2])
    // Y plane
    if len(y) < e.w*e.h || len(u) < (e.w/2)*(e.h/2) || len(v) < (e.w/2)*(e.h/2) {
        return nil, false, errors.New("bad plane sizes")
    }
    // Copy row by row to handle stride
    pY := unsafe.Pointer(e.img.planes[0])
    for row := 0; row < e.h; row++ {
        dst := unsafe.Add(pY, row*yw)
        src := y[row*e.w : row*e.w+e.w]
        C.memcpy(dst, unsafe.Pointer(&src[0]), C.size_t(e.w))
    }
    // U plane
    pU := unsafe.Pointer(e.img.planes[1])
    for row := 0; row < uh; row++ {
        dst := unsafe.Add(pU, row*uw)
        src := u[row*(e.w/2) : row*(e.w/2)+(e.w/2)]
        C.memcpy(dst, unsafe.Pointer(&src[0]), C.size_t(e.w/2))
    }
    // V plane
    pV := unsafe.Pointer(e.img.planes[2])
    for row := 0; row < uh; row++ {
        dst := unsafe.Add(pV, row*vw)
        src := v[row*(e.w/2) : row*(e.w/2)+(e.w/2)]
        C.memcpy(dst, unsafe.Pointer(&src[0]), C.size_t(e.w/2))
    }

    flags := C.vpx_enc_frame_flags_t(0)
    // Real-time deadline
    if C.vpx_codec_encode(&e.ctx, e.img, e.pts, 1, flags, C.VPX_DL_REALTIME) != C.VPX_CODEC_OK {
        return nil, false, errors.New("vpx_codec_encode failed")
    }
    e.pts++

    var iter C.vpx_codec_iter_t
    for {
        pkt := C.vpx_codec_get_cx_data(&e.ctx, &iter)
        if pkt == nil { break }
        if pkt.kind != C.VPX_CODEC_CX_FRAME_PKT { continue }
        f := (*C.vpx_codec_cx_pkt_t)(unsafe.Pointer(pkt))
        var frameData C.frame_data_t
        // Copy the frame data struct to avoid direct union access
        C.memcpy(unsafe.Pointer(&frameData), unsafe.Pointer(&f.data), C.size_t(unsafe.Sizeof(frameData)))
        // Now we can safely access the frame data
        goBytes := C.GoBytes(frameData.buf, C.int(frameData.sz))
        out = append(out, goBytes)
        keyframe = keyframe || (frameData.flags&C.VPX_FRAME_IS_KEY) != 0
    }
    return out, keyframe, nil
}

func (e *VP8Encoder) Close() {
    if e.img != nil { C.vpx_img_free(e.img); e.img = nil }
    if e.open { C.vpx_codec_destroy(&e.ctx); e.open = false }
}

// --- VP9 encoder (same API) ---

type VP9Encoder struct {
    ctx   C.vpx_codec_ctx_t
    cfg   C.vpx_codec_enc_cfg_t
    img   *C.vpx_image_t
    w, h  int
    fps   int
    pts   C.vpx_codec_pts_t
    open  bool
}

type VP9Config struct {
    Width, Height int
    FPS           int
    BitrateKbps   int
}

func NewVP9Encoder(cfg VP9Config) (*VP9Encoder, error) {
    if cfg.Width <= 0 || cfg.Height <= 0 || cfg.FPS <= 0 {
        return nil, errors.New("invalid VP9 encoder config")
    }
    e := &VP9Encoder{w: cfg.Width, h: cfg.Height, fps: cfg.FPS}
    if C.vpx_codec_enc_config_default(C.vpx_iface_vp9(), &e.cfg, 0) != C.VPX_CODEC_OK {
        return nil, errors.New("vpx_codec_enc_config_default failed")
    }
    e.cfg.g_w = C.uint(cfg.Width)
    e.cfg.g_h = C.uint(cfg.Height)
    e.cfg.g_timebase.num = 1
    e.cfg.g_timebase.den = C.int(cfg.FPS)
    if cfg.BitrateKbps > 0 {
        e.cfg.rc_target_bitrate = C.uint(cfg.BitrateKbps)
    }
    e.cfg.g_pass = C.VPX_RC_ONE_PASS
    e.cfg.g_threads = 4
    e.cfg.rc_end_usage = C.VPX_CBR
    e.cfg.kf_mode = C.VPX_KF_AUTO

    if st := C.vpx_codec_enc_init_ver(&e.ctx, C.vpx_iface_vp9(), &e.cfg, 0, C.VPX_ENCODER_ABI_VERSION); st != C.VPX_CODEC_OK {
        errStr := C.GoString(C.vpx_codec_err_to_string(st))
        more := C.GoString(C.vpx_codec_error_detail(&e.ctx))
        if more != "" { errStr = fmt.Sprintf("%s: %s", errStr, more) }
        return nil, fmt.Errorf("vpx_codec_enc_init_ver failed (%dx%d@%dfps, %dkbps): %s", cfg.Width, cfg.Height, cfg.FPS, cfg.BitrateKbps, errStr)
    }
    e.img = C.vpx_img_alloc(nil, C.VPX_IMG_FMT_I420, C.uint(e.w), C.uint(e.h), 1)
    if e.img == nil {
        e.Close()
        return nil, errors.New("vpx_img_alloc failed")
    }
    e.open = true
    return e, nil
}

func (e *VP9Encoder) EncodeI420(y, u, v []byte) (out [][]byte, keyframe bool, err error) {
    if !e.open { return nil, false, errors.New("encoder closed") }
    yw := int(e.img.stride[0])
    uh := e.h / 2
    uw := int(e.img.stride[1])
    vw := int(e.img.stride[2])
    if len(y) < e.w*e.h || len(u) < (e.w/2)*(e.h/2) || len(v) < (e.w/2)*(e.h/2) {
        return nil, false, errors.New("bad plane sizes")
    }
    pY := unsafe.Pointer(e.img.planes[0])
    for row := 0; row < e.h; row++ {
        dst := unsafe.Add(pY, row*yw)
        src := y[row*e.w : row*e.w+e.w]
        C.memcpy(dst, unsafe.Pointer(&src[0]), C.size_t(e.w))
    }
    pU := unsafe.Pointer(e.img.planes[1])
    for row := 0; row < uh; row++ {
        dst := unsafe.Add(pU, row*uw)
        src := u[row*(e.w/2) : row*(e.w/2)+(e.w/2)]
        C.memcpy(dst, unsafe.Pointer(&src[0]), C.size_t(e.w/2))
    }
    pV := unsafe.Pointer(e.img.planes[2])
    for row := 0; row < uh; row++ {
        dst := unsafe.Add(pV, row*vw)
        src := v[row*(e.w/2) : row*(e.w/2)+(e.w/2)]
        C.memcpy(dst, unsafe.Pointer(&src[0]), C.size_t(e.w/2))
    }

    flags := C.vpx_enc_frame_flags_t(0)
    if C.vpx_codec_encode(&e.ctx, e.img, e.pts, 1, flags, C.VPX_DL_REALTIME) != C.VPX_CODEC_OK {
        return nil, false, errors.New("vpx_codec_encode failed")
    }
    e.pts++
    var iter C.vpx_codec_iter_t
    for {
        pkt := C.vpx_codec_get_cx_data(&e.ctx, &iter)
        if pkt == nil { break }
        if pkt.kind != C.VPX_CODEC_CX_FRAME_PKT { continue }
        f := (*C.vpx_codec_cx_pkt_t)(unsafe.Pointer(pkt))
        var frameData C.frame_data_t
        C.memcpy(unsafe.Pointer(&frameData), unsafe.Pointer(&f.data), C.size_t(unsafe.Sizeof(frameData)))
        goBytes := C.GoBytes(frameData.buf, C.int(frameData.sz))
        out = append(out, goBytes)
        keyframe = keyframe || (frameData.flags&C.VPX_FRAME_IS_KEY) != 0
    }
    return out, keyframe, nil
}

func (e *VP9Encoder) Close() {
    if e.img != nil { C.vpx_img_free(e.img); e.img = nil }
    if e.open { C.vpx_codec_destroy(&e.ctx); e.open = false }
}
