//go:build cgo && yuv

package stream

/*
#cgo CFLAGS: -I/usr/include -I/usr/local/include
#cgo LDFLAGS: -lyuv

#include <stdint.h>
#include <libyuv.h>
*/
import "C"
import (
    "os"
    "strings"
)

// BGRAtoI420 converts BGRA to I420 using libyuv (SIMD-accelerated).
func BGRAtoI420(bgra []byte, w, h int, y, u, v []byte) {
    if w <= 0 || h <= 0 { return }
    if len(bgra) < w*h*4 || len(y) < w*h || len(u) < (w/2)*(h/2) || len(v) < (w/2)*(h/2) {
        return
    }
    switch bgraOrder {
    case "RGBA":
        if swapUV {
            C.RGBAToI420((*C.uint8_t)(&bgra[0]), C.int(w*4),
                (*C.uint8_t)(&y[0]), C.int(w),
                (*C.uint8_t)(&v[0]), C.int(w/2),
                (*C.uint8_t)(&u[0]), C.int(w/2),
                C.int(w), C.int(h))
        } else {
            C.RGBAToI420((*C.uint8_t)(&bgra[0]), C.int(w*4),
                (*C.uint8_t)(&y[0]), C.int(w),
                (*C.uint8_t)(&u[0]), C.int(w/2),
                (*C.uint8_t)(&v[0]), C.int(w/2),
                C.int(w), C.int(h))
        }
    case "ARGB":
        if swapUV {
            C.ARGBToI420((*C.uint8_t)(&bgra[0]), C.int(w*4),
                (*C.uint8_t)(&y[0]), C.int(w),
                (*C.uint8_t)(&v[0]), C.int(w/2),
                (*C.uint8_t)(&u[0]), C.int(w/2),
                C.int(w), C.int(h))
        } else {
            C.ARGBToI420((*C.uint8_t)(&bgra[0]), C.int(w*4),
                (*C.uint8_t)(&y[0]), C.int(w),
                (*C.uint8_t)(&u[0]), C.int(w/2),
                (*C.uint8_t)(&v[0]), C.int(w/2),
                C.int(w), C.int(h))
        }
    case "ABGR":
        if swapUV {
            C.ABGRToI420((*C.uint8_t)(&bgra[0]), C.int(w*4),
                (*C.uint8_t)(&y[0]), C.int(w),
                (*C.uint8_t)(&v[0]), C.int(w/2),
                (*C.uint8_t)(&u[0]), C.int(w/2),
                C.int(w), C.int(h))
        } else {
            C.ABGRToI420((*C.uint8_t)(&bgra[0]), C.int(w*4),
                (*C.uint8_t)(&y[0]), C.int(w),
                (*C.uint8_t)(&u[0]), C.int(w/2),
                (*C.uint8_t)(&v[0]), C.int(w/2),
                C.int(w), C.int(h))
        }
    default: // BGRA
        if swapUV {
            C.BGRAToI420((*C.uint8_t)(&bgra[0]), C.int(w*4),
                (*C.uint8_t)(&y[0]), C.int(w),
                (*C.uint8_t)(&v[0]), C.int(w/2),
                (*C.uint8_t)(&u[0]), C.int(w/2),
                C.int(w), C.int(h))
        } else {
            C.BGRAToI420((*C.uint8_t)(&bgra[0]), C.int(w*4),
                (*C.uint8_t)(&y[0]), C.int(w),
                (*C.uint8_t)(&u[0]), C.int(w/2),
                (*C.uint8_t)(&v[0]), C.int(w/2),
                C.int(w), C.int(h))
        }
    }
}

// UYVYtoI420 converts UYVY 4:2:2 to I420 using libyuv.
func UYVYtoI420(src []byte, w, h int, yPlane, uPlane, vPlane []byte) {
    if w <= 0 || h <= 0 { return }
    if len(src) < w*h*2 || len(yPlane) < w*h || len(uPlane) < (w/2)*(h/2) || len(vPlane) < (w/2)*(h/2) {
        return
    }
    C.UYVYToI420(
        (*C.uint8_t)(&src[0]), C.int(w*2),
        (*C.uint8_t)(&yPlane[0]), C.int(w),
        (*C.uint8_t)(&uPlane[0]), C.int(w/2),
        (*C.uint8_t)(&vPlane[0]), C.int(w/2),
        C.int(w), C.int(h),
    )
}

// I420Scale scales an I420 frame from (sw,sh) to (dw,dh) using libyuv.
// If libyuv is not available, a pure-Go fallback will be used (see i420_scale_go.go).
func I420Scale(ySrc, uSrc, vSrc []byte, sw, sh int, yDst, uDst, vDst []byte, dw, dh int) {
    if sw <= 0 || sh <= 0 || dw <= 0 || dh <= 0 { return }
    // Choose libyuv filter mode via env (default BOX for decent quality).
    // Set YUV_SCALE_FILTER to one of: NONE, LINEAR, BILINEAR, BOX
    var fm uint32
    switch getYUVScaleFilter() {
    case "NONE":
        fm = uint32(C.kFilterNone)
    case "LINEAR":
        fm = uint32(C.kFilterLinear)
    case "BILINEAR":
        fm = uint32(C.kFilterBilinear)
    case "BOX", "":
        fm = uint32(C.kFilterBox)
    default:
        fm = uint32(C.kFilterBox)
    }
    C.I420Scale(
        (*C.uint8_t)(&ySrc[0]), C.int(sw),
        (*C.uint8_t)(&uSrc[0]), C.int(sw/2),
        (*C.uint8_t)(&vSrc[0]), C.int(sw/2),
        C.int(sw), C.int(sh),
        (*C.uint8_t)(&yDst[0]), C.int(dw),
        (*C.uint8_t)(&uDst[0]), C.int(dw/2),
        (*C.uint8_t)(&vDst[0]), C.int(dw/2),
        C.int(dw), C.int(dh),
        fm,
    )
}
// ColorConversionImpl reports the active color conversion backend.
func ColorConversionImpl() string { return "libyuv(" + bgraOrder + ")" }

var bgraOrder = func() string {
    v := strings.ToUpper(strings.TrimSpace(os.Getenv("YUV_BGRA_ORDER")))
    switch v {
    case "RGBA", "ARGB", "ABGR", "BGRA":
        return v
    case "":
        // Default to ARGB as it matches common Windows capture sources here
        return "ARGB"
    default:
        return "ARGB"
    }
}()

var swapUV = func() bool {
    v := strings.TrimSpace(os.Getenv("YUV_SWAP_UV"))
    return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}()

// yuvScaleFilter controls libyuv scaling filter; empty or unknown -> BOX (default).
func getYUVScaleFilter() string {
    v := strings.ToUpper(strings.TrimSpace(os.Getenv("YUV_SCALE_FILTER")))
    switch v {
    case "NONE", "LINEAR", "BILINEAR", "BOX":
        return v
    case "":
        return "BOX"
    default:
        return "BOX"
    }
}

// I420ToBGRA converts I420 planes to packed 32-bit BGRA-like buffers according to YUV_BGRA_ORDER.
// Uses libyuv for speed. Respects YUV_SWAP_UV when converting.
func I420ToBGRA(y, u, v []byte, w, h int, out []byte) {
    if w <= 0 || h <= 0 { return }
    if len(y) < w*h || len(u) < (w/2)*(h/2) || len(v) < (w/2)*(h/2) || len(out) < w*h*4 { return }
    // Select appropriate converter by desired output order
    yptr := (*C.uint8_t)(&y[0])
    uptr := (*C.uint8_t)(&u[0])
    vptr := (*C.uint8_t)(&v[0])
    if swapUV {
        uptr, vptr = vptr, uptr
    }
    switch bgraOrder {
    case "RGBA":
        C.I420ToRGBA(yptr, C.int(w), uptr, C.int(w/2), vptr, C.int(w/2), (*C.uint8_t)(&out[0]), C.int(w*4), C.int(w), C.int(h))
    case "ARGB":
        C.I420ToARGB(yptr, C.int(w), uptr, C.int(w/2), vptr, C.int(w/2), (*C.uint8_t)(&out[0]), C.int(w*4), C.int(w), C.int(h))
    case "ABGR":
        C.I420ToABGR(yptr, C.int(w), uptr, C.int(w/2), vptr, C.int(w/2), (*C.uint8_t)(&out[0]), C.int(w*4), C.int(w), C.int(h))
    default: // BGRA
        C.I420ToBGRA(yptr, C.int(w), uptr, C.int(w/2), vptr, C.int(w/2), (*C.uint8_t)(&out[0]), C.int(w*4), C.int(w), C.int(h))
    }
}
