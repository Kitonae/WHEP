//go:build !yuv

package stream

// I420Scale scales an I420 frame from (sw,sh) to (dw,dh) using a simple nearest-neighbor algorithm.
// This is a pure-Go fallback used when libyuv is not enabled.
func I420Scale(ySrc, uSrc, vSrc []byte, sw, sh int, yDst, uDst, vDst []byte, dw, dh int) {
    if sw <= 0 || sh <= 0 || dw <= 0 || dh <= 0 { return }
    // Luma
    for y := 0; y < dh; y++ {
        sy := y * sh / dh
        for x := 0; x < dw; x++ {
            sx := x * sw / dw
            yDst[y*dw+x] = ySrc[sy*sw+sx]
        }
    }
    // Chroma (subsampled 2:1): scale at half resolution
    sw2, sh2 := sw/2, sh/2
    dw2, dh2 := dw/2, dh/2
    for y := 0; y < dh2; y++ {
        sy := y * sh2 / dh2
        for x := 0; x < dw2; x++ {
            sx := x * sw2 / dw2
            uDst[y*dw2+x] = uSrc[sy*sw2+sx]
            vDst[y*dw2+x] = vSrc[sy*sw2+sx]
        }
    }
}

