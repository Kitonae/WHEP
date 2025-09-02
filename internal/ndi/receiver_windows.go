//go:build windows && cgo

package ndi

/*
#cgo CFLAGS: -DWIN32_LEAN_AND_MEAN
// Update include path to your NDI SDK Include directory
// #cgo CFLAGS: -IC:/Program\ Files/NDI/NDI\ 5\ SDK/Include
// Update library path to your NDI SDK Lib/x64 directory
// #cgo LDFLAGS: -LC:/Program\ Files/NDI/NDI\ 5\ SDK/Lib/x64 -lProcessing.NDI.Lib.x64

#include <stdlib.h>
#include <Processing.NDI.Lib.h>

// Helper to allocate receiver in BGRA fast path
static NDIlib_recv_instance_t go_NDI_recv_create_BGRA(NDIlib_source_t src) {
    NDIlib_recv_create_v3_t cfg = {0};
    cfg.source_to_connect_to = src;
    cfg.color_format = NDIlib_recv_color_format_BGRX_BGRA;
    cfg.bandwidth = NDIlib_recv_bandwidth_highest;
    cfg.allow_video_fields = false;
    return NDIlib_recv_create_v3(&cfg);
}

// Enumerate sources once, returning pointer/length.
static const NDIlib_source_t* go_NDI_find_sources(uint32_t *count, uint32_t timeout_ms) {
    NDIlib_find_instance_t f = NDIlib_find_create_v2(NULL);
    if (!f) { *count = 0; return NULL; }
    NDIlib_find_wait_for_sources(f, timeout_ms);
    const NDIlib_source_t* arr = NDIlib_find_get_current_sources(f, count);
    // Intentionally not destroying finder yet so caller can read pointers before free.
    // Caller must call NDIlib_find_destroy on the returned instance, but since we can't
    // return it across cgo easily in this helper, we copy out in Go and then destroy here.
    return arr;
}

*/
import "C"

import (
    "errors"
    "unsafe"
)

type Receiver struct {
    inst C.NDIlib_recv_instance_t
}

func Initialize() bool { return bool(C.NDIlib_initialize()) }

func FindFirst(timeoutMs int) (name, url string, ok bool) {
    find := C.NDIlib_find_create_v2(nil)
    if find == nil { return "", "", false }
    defer C.NDIlib_find_destroy(find)
    C.NDIlib_find_wait_for_sources(find, C.uint(timeoutMs))
    var no C.uint
    arr := C.NDIlib_find_get_current_sources(find, &no)
    if arr == nil || no == 0 { return "", "", false }
    s := (*[1 << 30]C.NDIlib_source_t)(unsafe.Pointer(arr))[:no:no]
    n := s[0]
    if n.p_ndi_name != nil { name = C.GoString(n.p_ndi_name) }
    if n.p_url_address != nil { url = C.GoString(n.p_url_address) }
    return name, url, true
}

func NewReceiverByURL(url string) (*Receiver, error) {
    cstr := C.CString(url)
    defer C.free(unsafe.Pointer(cstr))
    var src C.NDIlib_source_t
    src.p_ndi_name = nil
    src.p_url_address = cstr
    inst := C.go_NDI_recv_create_BGRA(src)
    if inst == nil { return nil, errors.New("NDIlib_recv_create_v3 failed") }
    return &Receiver{inst: inst}, nil
}

type SourceInfo struct { Name, URL string }

// ListSources performs a one-shot discovery and returns copies of name+url strings.
func ListSources(timeoutMs int) []SourceInfo {
    fi := C.NDIlib_find_create_v2(nil)
    if fi == nil { return nil }
    defer C.NDIlib_find_destroy(fi)
    C.NDIlib_find_wait_for_sources(fi, C.uint(timeoutMs))
    var no C.uint
    arr := C.NDIlib_find_get_current_sources(fi, &no)
    if arr == nil || no == 0 { return nil }
    out := make([]SourceInfo, 0, int(no))
    s := (*[1 << 28]C.NDIlib_source_t)(unsafe.Pointer(arr))[:no:no]
    for i := 0; i < int(no); i++ {
        var name, url string
        if s[i].p_ndi_name != nil { name = C.GoString(s[i].p_ndi_name) }
        if s[i].p_url_address != nil { url = C.GoString(s[i].p_url_address) }
        out = append(out, SourceInfo{Name: name, URL: url})
    }
    return out
}

type VideoFrame struct {
    W, H    int
    Stride  int
    FourCC  int
    Data    []byte // length = Stride*H
}

func (r *Receiver) CaptureVideo(timeoutMs int) (*VideoFrame, bool, error) {
    var vf C.NDIlib_video_frame_v2_t
    ftype := C.NDIlib_recv_capture_v2(r.inst, &vf, nil, nil, C.uint(timeoutMs))
    switch ftype {
    case C.NDIlib_frame_type_video:
        w := int(vf.xres)
        h := int(vf.yres)
        stride := int(vf.line_stride_in_bytes)
        size := stride * h
        // Copy into Go slice
        data := C.GoBytes(unsafe.Pointer(vf.p_data), C.int(size))
        out := &VideoFrame{W: w, H: h, Stride: stride, FourCC: int(vf.FourCC), Data: data}
        C.NDIlib_recv_free_video_v2(r.inst, &vf)
        return out, true, nil
    case C.NDIlib_frame_type_none, C.NDIlib_frame_type_status_change:
        return nil, false, nil
    case C.NDIlib_frame_type_error:
        return nil, false, errors.New("NDI recv error")
    default:
        return nil, false, nil
    }
}

func (r *Receiver) Close() { if r.inst != nil { C.NDIlib_recv_destroy(r.inst); r.inst = nil } }
