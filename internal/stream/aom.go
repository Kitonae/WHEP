//go:build cgo && aom

package stream

/*
#cgo CFLAGS: -I/usr/include -I/usr/local/include
#cgo LDFLAGS: -laom

#include <stdlib.h>
#include <string.h>
#include <aom/aom_encoder.h>
#include <aom/aomcx.h>

static const aom_codec_iface_t* aom_iface_av1() { return aom_codec_av1_cx(); }

typedef struct aom_frame_data {
    void *buf;
    size_t sz;
    aom_codec_pts_t pts;
    unsigned long duration;
    aom_codec_frame_flags_t flags;
} aom_frame_data_t;
*/
import "C"

import (
    "errors"
    "unsafe"
)

type AV1Encoder struct {
    ctx   C.aom_codec_ctx_t
    cfg   C.aom_codec_enc_cfg_t
    img   *C.aom_image_t
    w, h  int
    fps   int
    pts   C.aom_codec_pts_t
    open  bool
}

type AV1Config struct {
    Width, Height int
    FPS           int
    BitrateKbps   int
}

func NewAV1Encoder(cfg AV1Config) (*AV1Encoder, error) {
    if cfg.Width <= 0 || cfg.Height <= 0 || cfg.FPS <= 0 {
        return nil, errors.New("invalid AV1 encoder config")
    }
    e := &AV1Encoder{w: cfg.Width, h: cfg.Height, fps: cfg.FPS}
    if C.aom_codec_enc_config_default(C.aom_iface_av1(), &e.cfg, 0) != C.AOM_CODEC_OK {
        return nil, errors.New("aom_codec_enc_config_default failed")
    }
    e.cfg.g_w = C.uint(cfg.Width)
    e.cfg.g_h = C.uint(cfg.Height)
    e.cfg.g_timebase.num = 1
    e.cfg.g_timebase.den = C.int(cfg.FPS)
    if cfg.BitrateKbps > 0 {
        e.cfg.rc_target_bitrate = C.uint(cfg.BitrateKbps)
    }
    // realtime tuning
    e.cfg.g_pass = C.AOM_RC_ONE_PASS
    e.cfg.g_threads = 4
    e.cfg.rc_end_usage = C.AOM_CBR
    e.cfg.kf_mode = C.AOM_KF_AUTO

    if C.aom_codec_enc_init_ver(&e.ctx, C.aom_iface_av1(), &e.cfg, 0, C.AOM_ENCODER_ABI_VERSION) != C.AOM_CODEC_OK {
        return nil, errors.New("aom_codec_enc_init_ver failed")
    }
    // speed-up for realtime: set cpu-used
    _ = C.aom_codec_control_(&e.ctx, C.AOME_SET_CPUUSED, C.int(6))
    _ = C.aom_codec_control_(&e.ctx, C.AOME_SET_ENABLEAUTOALTREF, C.int(0))
    _ = C.aom_codec_control_(&e.ctx, C.AOME_SET_USAGE, C.int(C.AOM_USAGE_REALTIME))

    // Allocate I420 image
    e.img = C.aom_img_alloc(nil, C.AOM_IMG_FMT_I420, C.uint(e.w), C.uint(e.h), 1)
    if e.img == nil {
        e.Close()
        return nil, errors.New("aom_img_alloc failed")
    }
    e.open = true
    return e, nil
}

// EncodeI420 encodes a single frame. y should be w*h; u and v w/2*h/2.
func (e *AV1Encoder) EncodeI420(y, u, v []byte) (out [][]byte, keyframe bool, err error) {
    if !e.open { return nil, false, errors.New("encoder closed") }
    yw := int(e.img.stride[0])
    uh := e.h / 2
    uw := int(e.img.stride[1])
    vw := int(e.img.stride[2])
    if len(y) < e.w*e.h || len(u) < (e.w/2)*(e.h/2) || len(v) < (e.w/2)*(e.h/2) {
        return nil, false, errors.New("bad plane sizes")
    }
    // Copy planes with stride
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

    flags := C.aom_enc_frame_flags_t(0)
    if C.aom_codec_encode(&e.ctx, e.img, e.pts, 1, flags) != C.AOM_CODEC_OK {
        return nil, false, errors.New("aom_codec_encode failed")
    }
    e.pts++

    var iter C.aom_codec_iter_t
    for {
        pkt := C.aom_codec_get_cx_data(&e.ctx, &iter)
        if pkt == nil { break }
        if pkt._type != C.AOM_CODEC_CX_FRAME_PKT { continue }
        f := (*C.aom_codec_cx_pkt_t)(unsafe.Pointer(pkt))
        var frameData C.aom_frame_data_t
        C.memcpy(unsafe.Pointer(&frameData), unsafe.Pointer(&f.data), C.size_t(unsafe.Sizeof(frameData)))
        goBytes := C.GoBytes(frameData.buf, C.int(frameData.sz))
        out = append(out, goBytes)
        keyframe = keyframe || (frameData.flags&C.AOM_FRAME_IS_KEY) != 0
    }
    return out, keyframe, nil
}

func (e *AV1Encoder) Close() {
    if e.img != nil { C.aom_img_free(e.img); e.img = nil }
    if e.open { C.aom_codec_destroy(&e.ctx); e.open = false }
}

