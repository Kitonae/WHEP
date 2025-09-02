//go:build cgo

package stream

/*
#cgo CFLAGS: -I/usr/include -I/usr/local/include
#cgo LDFLAGS: -lvpx

#include <stdlib.h>
#include <string.h>
#include <vpx/vpx_encoder.h>
#include <vpx/vp8cx.h>

static vpx_codec_iface_t* vpx_iface() { return vpx_codec_vp8_cx(); }

*/
import "C"

import (
    "errors"
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
}

func NewVP8Encoder(cfg VP8Config) (*VP8Encoder, error) {
    if cfg.Width <= 0 || cfg.Height <= 0 || cfg.FPS <= 0 {
        return nil, errors.New("invalid VP8 encoder config")
    }
    e := &VP8Encoder{w: cfg.Width, h: cfg.Height, fps: cfg.FPS}
    if C.vpx_codec_enc_config_default(C.vpx_iface(), &e.cfg, 0) != C.VPX_CODEC_OK {
        return nil, errors.New("vpx_codec_enc_config_default failed")
    }
    e.cfg.g_w = C.uint(cfg.Width)
    e.cfg.g_h = C.uint(cfg.Height)
    e.cfg.g_timebase.num = 1
    e.cfg.g_timebase.den = C.int(cfg.FPS)
    if cfg.BitrateKbps > 0 {
        e.cfg.rc_target_bitrate = C.uint(cfg.BitrateKbps)
    }
    // realtime settings
    e.cfg.g_pass = C.VPX_RC_ONE_PASS
    e.cfg.g_threads = 4
    e.cfg.rc_end_usage = C.VPX_CBR
    e.cfg.kf_mode = C.VPX_KF_AUTO

    if C.vpx_codec_enc_init_ver(&e.ctx, C.vpx_iface(), &e.cfg, 0, C.VPX_ENCODER_ABI_VERSION) != C.VPX_CODEC_OK {
        return nil, errors.New("vpx_codec_enc_init_ver failed")
    }
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
        f := (*C.vpx_codec_cx_pkt_t)(pkt)
        frame := (*C.uchar)(f.data.frame.buf)
        size := int(f.data.frame.sz)
        goBytes := C.GoBytes(unsafe.Pointer(frame), C.int(size))
        out = append(out, goBytes)
        keyframe = keyframe || (f.data.frame.flags & C.VPX_FRAME_IS_KEY) != 0
    }
    return out, keyframe, nil
}

func (e *VP8Encoder) Close() {
    if e.img != nil { C.vpx_img_free(e.img); e.img = nil }
    if e.open { C.vpx_codec_destroy(&e.ctx); e.open = false }
}

