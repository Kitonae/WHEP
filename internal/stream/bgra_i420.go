//go:build !yuv

package stream

// BGRAtoI420 converts a BGRA frame (w*h*4) to planar I420 (y, u, v).
// Simple integer approximation of BT.601 full-range.
func BGRAtoI420(bgra []byte, w, h int, y, u, v []byte) {
    // y size: w*h; u,v size: (w/2)*(h/2)
    // For chroma, average 2x2 block
    for yrow := 0; yrow < h; yrow++ {
        for x := 0; x < w; x++ {
            off := (yrow*w + x) * 4
            b := int(bgra[off+0])
            g := int(bgra[off+1])
            r := int(bgra[off+2])
            // luma
            Y := (  66*r + 129*g +  25*b + 128) >> 8
            y[yrow*w+x] = clamp8(Y + 16)
        }
    }
    // chroma subsample
    for yrow := 0; yrow < h; yrow += 2 {
        for x := 0; x < w; x += 2 {
            var rSum, gSum, bSum int
            for dy := 0; dy < 2; dy++ {
                for dx := 0; dx < 2; dx++ {
                    off := ((yrow+dy)*w + (x+dx)) * 4
                    bSum += int(bgra[off+0])
                    gSum += int(bgra[off+1])
                    rSum += int(bgra[off+2])
                }
            }
            r := rSum >> 2; g := gSum >> 2; b := bSum >> 2
            U := ((-38*r - 74*g + 112*b + 128) >> 8) + 128
            Vv := ((112*r - 94*g - 18*b + 128) >> 8) + 128
            u[(yrow/2)*(w/2)+(x/2)] = clamp8(U)
            v[(yrow/2)*(w/2)+(x/2)] = clamp8(Vv)
        }
    }
}

func clamp8(x int) byte { if x < 0 { return 0 }; if x > 255 { return 255 }; return byte(x) }
