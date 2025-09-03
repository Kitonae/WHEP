//go:build cgo && svt

package stream

/*
#cgo CFLAGS: -I/usr/include -I/usr/local/include
#cgo LDFLAGS: -lSvtAv1Enc

#include <stdlib.h>
#include <string.h>
#include "EbSvtAv1Enc.h"

static EbErrorType go_svt_default_cfg(EbSvtAv1EncConfiguration *cfg) {
    // Some builds require init_handle to set defaults
    EbComponentType *handle = NULL;
    EbErrorType err = svt_av1_enc_init_handle(&handle, NULL, cfg);
    if (err == EB_ErrorNone && handle) {
        svt_av1_enc_deinit_handle(handle);
    }
    return err;
}

static EbSvtIOFormat* go_svt_alloc_iofmt() {
    EbSvtIOFormat *io = (EbSvtIOFormat*)calloc(1, sizeof(EbSvtIOFormat));
    return io;
}

static EbBufferHeaderType* go_svt_alloc_bufhdr() {
    EbBufferHeaderType *bh = (EbBufferHeaderType*)calloc(1, sizeof(EbBufferHeaderType));
    return bh;
}

*/
import "C"

import (
    "errors"
    "unsafe"
)

// AV1Encoder backed by SVT-AV1
type AV1Encoder struct {
    handle *C.EbComponentType
    cfg    C.EbSvtAv1EncConfiguration
    io     *C.EbSvtIOFormat
    hdr    *C.EbBufferHeaderType
    w, h   int
    fps    int
    ybuf, ubuf, vbuf unsafe.Pointer
    open   bool
}

type AV1Config struct {
    Width, Height int
    FPS           int
    BitrateKbps   int
}

func NewAV1Encoder(cfg AV1Config) (*AV1Encoder, error) {
    if cfg.Width <= 0 || cfg.Height <= 0 || cfg.FPS <= 0 { return nil, errors.New("invalid AV1 encoder config") }
    e := &AV1Encoder{w: cfg.Width, h: cfg.Height, fps: cfg.FPS}
    if C.go_svt_default_cfg(&e.cfg) != C.EB_ErrorNone {
        return nil, errors.New("svt default config failed")
    }
    e.cfg.source_width = C.uint32_t(cfg.Width)
    e.cfg.source_height = C.uint32_t(cfg.Height)
    // Frame rate as numerator/denominator
    e.cfg.frame_rate_numerator = C.uint32_t(cfg.FPS)
    e.cfg.frame_rate_denominator = 1
    if cfg.BitrateKbps > 0 {
        e.cfg.rate_control_mode = 1 // VBR
        e.cfg.target_bit_rate = C.uint32_t(cfg.BitrateKbps * 1000)
    }
    // realtime speed preset (higher is faster, lower latency)
    e.cfg.enc_mode = 8

    // Create handle with cfg loaded
    if C.svt_av1_enc_init_handle(&e.handle, nil, &e.cfg) != C.EB_ErrorNone {
        return nil, errors.New("svt init handle failed")
    }
    if C.svt_av1_enc_set_parameter(e.handle, &e.cfg) != C.EB_ErrorNone {
        C.svt_av1_enc_deinit_handle(e.handle)
        return nil, errors.New("svt set parameter failed")
    }
    if C.svt_av1_enc_init(e.handle) != C.EB_ErrorNone {
        C.svt_av1_enc_deinit_handle(e.handle)
        return nil, errors.New("svt init failed")
    }
    // Allocate IO format and buffers
    e.io = C.go_svt_alloc_iofmt()
    if e.io == nil { e.Close(); return nil, errors.New("svt iofmt alloc failed") }
    e.io.width = C.uint32_t(e.w)
    e.io.height = C.uint32_t(e.h)
    e.io.y_stride = C.uint32_t(e.w)
    e.io.cb_stride = C.uint32_t(e.w/2)
    e.io.cr_stride = C.uint32_t(e.w/2)
    e.io.color_format = C.EB_YUV420
    e.io.bit_depth = C.uint8_t(8)

    ysz := C.size_t(e.w*e.h)
    usz := C.size_t((e.w/2)*(e.h/2))
    vsz := usz
    e.ybuf = C.malloc(ysz)
    e.ubuf = C.malloc(usz)
    e.vbuf = C.malloc(vsz)
    if e.ybuf == nil || e.ubuf == nil || e.vbuf == nil { e.Close(); return nil, errors.New("svt plane alloc failed") }
    e.io.luma = (*C.uint8_t)(e.ybuf)
    e.io.cb = (*C.uint8_t)(e.ubuf)
    e.io.cr = (*C.uint8_t)(e.vbuf)

    e.hdr = C.go_svt_alloc_bufhdr()
    if e.hdr == nil { e.Close(); return nil, errors.New("svt bufhdr alloc failed") }
    e.hdr.p_buffer = (*C.uint8_t)(unsafe.Pointer(e.io))
    e.hdr.n_alloc_len = 0
    e.hdr.n_filled_len = 0
    e.open = true
    return e, nil
}

func (e *AV1Encoder) EncodeI420(y, u, v []byte) (out [][]byte, keyframe bool, err error) {
    if !e.open { return nil, false, errors.New("encoder closed") }
    if len(y) < e.w*e.h || len(u) < (e.w/2)*(e.h/2) || len(v) < (e.w/2)*(e.h/2) {
        return nil, false, errors.New("bad plane sizes")
    }
    // Copy planes into C-allocated buffers (stride assumed tight)
    C.memcpy(e.ybuf, unsafe.Pointer(&y[0]), C.size_t(e.w*e.h))
    C.memcpy(e.ubuf, unsafe.Pointer(&u[0]), C.size_t((e.w/2)*(e.h/2)))
    C.memcpy(e.vbuf, unsafe.Pointer(&v[0]), C.size_t((e.w/2)*(e.h/2)))

    e.hdr.n_pts++
    if C.svt_av1_enc_send_picture(e.handle, e.hdr) != C.EB_ErrorNone {
        return nil, false, errors.New("svt send picture failed")
    }
    // Drain available packets (non-blocking)
    for {
        var pkt *C.EbBufferHeaderType
        if C.svt_av1_enc_get_packet(e.handle, &pkt, 0) != C.EB_ErrorNone || pkt == nil {
            break
        }
        if pkt.n_filled_len > 0 && pkt.p_buffer != nil {
            goBytes := C.GoBytes(unsafe.Pointer(pkt.p_buffer), C.int(pkt.n_filled_len))
            out = append(out, goBytes)
        }
        C.svt_av1_enc_release_out_buffer(&pkt)
    }
    return out, keyframe, nil
}

func (e *AV1Encoder) Close() {
    if e.handle != nil {
        _ = C.svt_av1_enc_deinit(e.handle)
        _ = C.svt_av1_enc_deinit_handle(e.handle)
        e.handle = nil
    }
    if e.ybuf != nil { C.free(e.ybuf); e.ybuf = nil }
    if e.ubuf != nil { C.free(e.ubuf); e.ubuf = nil }
    if e.vbuf != nil { C.free(e.vbuf); e.vbuf = nil }
    if e.hdr != nil { C.free(unsafe.Pointer(e.hdr)); e.hdr = nil }
    if e.io != nil { C.free(unsafe.Pointer(e.io)); e.io = nil }
    e.open = false
}
