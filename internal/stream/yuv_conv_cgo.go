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
