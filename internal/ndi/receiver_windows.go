//go:build windows && cgo

package ndi

/*
#cgo CFLAGS: -DWIN32_LEAN_AND_MEAN -Wno-deprecated-declarations
#cgo CFLAGS: -IC:/Program\ Files/NDI/NDI\ 6\ SDK/Include
#cgo LDFLAGS: -LC:/Program\ Files/NDI/NDI\ 6\ SDK/Lib/x64 -lProcessing.NDI.Lib.x64

#include <stdlib.h>
#include <Processing.NDI.Lib.h>

// Helper to allocate receiver with specified color format (0=BGRA, 1=UYVY)
static NDIlib_recv_instance_t go_NDI_recv_create_with_color(NDIlib_source_t src, int color) {
    NDIlib_recv_create_v3_t cfg = {0};
    cfg.source_to_connect_to = src;
    cfg.bandwidth = NDIlib_recv_bandwidth_highest;
    cfg.allow_video_fields = false;
    cfg.p_ndi_recv_name = NULL;
    if (color == 1) {
        cfg.color_format = NDIlib_recv_color_format_UYVY_BGRA;
    } else {
        cfg.color_format = NDIlib_recv_color_format_BGRX_BGRA;
    }
    return NDIlib_recv_create_v3(&cfg);
}

// Enumerate sources using finder (v2 API available in SDK)
static const NDIlib_source_t* go_NDI_find_sources(uint32_t *count, uint32_t timeout_ms) {
    NDIlib_find_create_t cfg = {0};
    cfg.show_local_sources = true;
    cfg.p_extra_ips = NULL;
    NDIlib_find_instance_t f = NDIlib_find_create_v2(&cfg);
    if (!f) { *count = 0; return NULL; }
    NDIlib_find_wait_for_sources(f, timeout_ms);
    const NDIlib_source_t* arr = NDIlib_find_get_current_sources(f, count);
    // Intentionally not destroying finder yet so caller can read pointers before free
    // Caller must call NDIlib_find_destroy on the returned instance
    return arr;
}

// Helpers to set and get union fields in NDIlib_source_t for cgo compatibility
static void go_set_source_url(NDIlib_source_t* src, const char* url) {
    src->p_ndi_name = NULL;
    src->p_url_address = url;
}
static void go_set_source_name(NDIlib_source_t* src, const char* name) {
    src->p_ndi_name = name;
    src->p_url_address = NULL;
}
static const char* go_get_source_url(const NDIlib_source_t* src) {
    return src->p_url_address;
}

*/
import "C"

import (
	"errors"
	"os"
	"strings"
	"unsafe"
)

type Receiver struct {
	inst C.NDIlib_recv_instance_t
}

func Initialize() bool { return bool(C.NDIlib_initialize()) }

func FindFirst(timeoutMs int) (name, url string, ok bool) {
	find := C.NDIlib_find_create_v2(nil)
	if find == nil {
		return "", "", false
	}
	defer C.NDIlib_find_destroy(find)
	C.NDIlib_find_wait_for_sources(find, C.uint(timeoutMs))
	var no C.uint
	arr := C.NDIlib_find_get_current_sources(find, &no)
	if arr == nil || no == 0 {
		return "", "", false
	}
	s := (*[1 << 30]C.NDIlib_source_t)(unsafe.Pointer(arr))[:no:no]
	n := s[0]
	if n.p_ndi_name != nil {
		name = C.GoString(n.p_ndi_name)
	}
	// Build fallback URL from name
	if name != "" {
		url = "ndi://" + name
	}
	return name, url, true
}

func NewReceiverByURL(url string) (*Receiver, error) {
	cstr := C.CString(url)
	defer C.free(unsafe.Pointer(cstr))
	var src C.NDIlib_source_t
	// Heuristic: treat as URL if it has a scheme (e.g., ndi://) OR looks like host:port
	if strings.Contains(url, "://") || strings.Contains(url, ":") {
		C.go_set_source_url(&src, cstr)
	} else {
		C.go_set_source_name(&src, cstr)
	}
	// Choose color format via env NDI_RECV_COLOR: "UYVY" or "BGRA" (default UYVY)
	colorSel := 1
	switch strings.ToUpper(os.Getenv("NDI_RECV_COLOR")) {
	case "BGRA", "BGRX":
		colorSel = 0
	default:
		colorSel = 1
	}
	inst := C.go_NDI_recv_create_with_color(src, C.int(colorSel))
	if inst == nil {
		return nil, errors.New("NDIlib_recv_create_v3 failed")
	}
	return &Receiver{inst: inst}, nil
}

type SourceInfo struct{ Name, URL string }

// ListSources polls discovery in short intervals up to timeoutMs and returns the latest set.
// This mimics the working implementation that samples get_current_sources repeatedly.
func ListSources(timeoutMs int) []SourceInfo {
	if timeoutMs <= 0 {
		timeoutMs = 2000 // default 2s
	}

	// Create finder with explicit config
	var cfg C.NDIlib_find_create_t
	cfg.show_local_sources = C.bool(true)
	// Optional groups and extra IPs from env to match SDK examples/NDI Monitor behavior
	var cGroups, cExtra *C.char
	if g := os.Getenv("NDI_GROUPS"); g != "" {
		cGroups = C.CString(g)
		cfg.p_groups = cGroups
	}
	if ips := os.Getenv("NDI_EXTRA_IPS"); ips != "" {
		cExtra = C.CString(ips)
		cfg.p_extra_ips = cExtra
	}
	fi := C.NDIlib_find_create_v2(&cfg)
	if fi == nil {
		if cGroups != nil { C.free(unsafe.Pointer(cGroups)) }
		if cExtra != nil { C.free(unsafe.Pointer(cExtra)) }
		return nil
	}
	defer func() {
		C.NDIlib_find_destroy(fi)
		if cGroups != nil { C.free(unsafe.Pointer(cGroups)) }
		if cExtra != nil { C.free(unsafe.Pointer(cExtra)) }
	}()

	// Poll in ~200ms steps until timeout, keeping the latest non-empty list
	remaining := timeoutMs
	step := 200
	var latest []SourceInfo
	for remaining >= 0 {
		var no C.uint
		arr := C.NDIlib_find_get_current_sources(fi, &no)
		if arr != nil && no > 0 {
			tmp := make([]SourceInfo, 0, int(no))
			s := (*[1 << 28]C.NDIlib_source_t)(unsafe.Pointer(arr))[:no:no]
			for i := 0; i < int(no); i++ {
				var name, url string
				if s[i].p_ndi_name != nil { name = C.GoString(s[i].p_ndi_name) }
				// Hard-filter out NDI Remote Connection helper sources
				ln := strings.ToLower(name)
				if strings.Contains(ln, "remote connection") {
					continue
				}
				if p := C.go_get_source_url(&s[i]); p != nil { url = C.GoString(p) } else if name != "" { url = "ndi://" + name }
				if name != "" || url != "" { tmp = append(tmp, SourceInfo{Name: name, URL: url}) }
			}
			latest = tmp
		}
		if remaining == 0 { break }
		if remaining < step { step = remaining }
		C.NDIlib_find_wait_for_sources(fi, C.uint(step))
		remaining -= step
	}
	return latest
}

type VideoFrame struct {
	W, H   int
	Stride int
	FourCC int
	Data   []byte // length = Stride*H
}

func (r *Receiver) CaptureVideo(timeoutMs int) (*VideoFrame, bool, error) {
	var vf C.NDIlib_video_frame_v2_t
	var af C.NDIlib_audio_frame_v3_t
	var mf C.NDIlib_metadata_frame_t
	ftype := C.NDIlib_recv_capture_v3(r.inst, &vf, &af, &mf, C.uint(timeoutMs))
	switch ftype {
	case C.NDIlib_frame_type_video:
		w := int(vf.xres)
		h := int(vf.yres)
		// Determine stride by FourCC (SDK variant here lacks line_stride_in_bytes)
		bpp := 4
		if vf.FourCC == C.NDIlib_FourCC_type_UYVY {
			bpp = 2
		}
		stride := w * bpp
		size := stride * h
		// Copy into Go slice
		data := C.GoBytes(unsafe.Pointer(vf.p_data), C.int(size))
		out := &VideoFrame{W: w, H: h, Stride: stride, FourCC: int(vf.FourCC), Data: data}
		C.NDIlib_recv_free_video_v2(r.inst, &vf)
		return out, true, nil
	case C.NDIlib_frame_type_audio:
		C.NDIlib_recv_free_audio_v3(r.inst, &af)
		return nil, false, nil
	case C.NDIlib_frame_type_metadata:
		C.NDIlib_recv_free_metadata(r.inst, &mf)
		return nil, false, nil
	case C.NDIlib_frame_type_none, C.NDIlib_frame_type_status_change:
		return nil, false, nil
	case C.NDIlib_frame_type_error:
		return nil, false, errors.New("NDI recv error")
	default:
		return nil, false, nil
	}
}

func (r *Receiver) Close() {
	if r.inst != nil {
		C.NDIlib_recv_destroy(r.inst)
		r.inst = nil
	}
}
