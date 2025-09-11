//go:build !yuv

package stream

// I420ToBGRA converts planar I420 to packed BGRA using a simple BT.601 full-range approximation.
func I420ToBGRA(y, u, v []byte, w, h int, out []byte) {
    if w <= 0 || h <= 0 { return }
    if len(y) < w*h || len(u) < (w/2)*(h/2) || len(v) < (w/2)*(h/2) || len(out) < w*h*4 { return }
    for yy := 0; yy < h; yy++ {
        for xx := 0; xx < w; xx++ {
            Y := int(y[yy*w+xx])
            U := int(u[(yy/2)*(w/2)+(xx/2)])
            V := int(v[(yy/2)*(w/2)+(xx/2)])
            c := Y - 16
            d := U - 128
            e := V - 128
            if c < 0 { c = 0 }
            // Approximate conversion
            r := (298*c + 409*e + 128) >> 8
            g := (298*c - 100*d - 208*e + 128) >> 8
            b := (298*c + 516*d + 128) >> 8
            if r < 0 { r = 0 } else if r > 255 { r = 255 }
            if g < 0 { g = 0 } else if g > 255 { g = 255 }
            if b < 0 { b = 0 } else if b > 255 { b = 255 }
            off := (yy*w + xx) * 4
            out[off+0] = byte(b)
            out[off+1] = byte(g)
            out[off+2] = byte(r)
            out[off+3] = 255
        }
    }
}

