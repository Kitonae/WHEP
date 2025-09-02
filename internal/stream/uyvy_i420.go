package stream

// UYVYtoI420 converts packed UYVY 4:2:2 (2 bytes per pixel) to planar I420 4:2:0.
// Assumes width and height are even.
func UYVYtoI420(src []byte, w, h int, yPlane, uPlane, vPlane []byte) {
    // For each line, process pairs of pixels: U0 Y0 V0 Y1
    // Chroma is subsampled vertically by averaging two lines.
    // Temporary buffers for half-resolution chroma per row
    halfW := w / 2
    // Accumulate U and V for two lines, then store to output each 2 rows
    for row := 0; row < h; row++ {
        srcOff := row * w * 2
        // Write luma for this row directly
        yi := row * w
        for x := 0; x < w; x += 2 {
            i := srcOff + x*2
            u := int(src[i+0])
            y0 := src[i+1]
            v := int(src[i+2])
            y1 := src[i+3]
            yPlane[yi+x+0] = y0
            yPlane[yi+x+1] = y1
            // For chroma, accumulate on even rows only; we'll average with the next row
            if (row & 1) == 0 {
                // Store interim sums in uPlane/vPlane areas beyond valid region to avoid extra alloc
                // Use the actual chroma destinations as accumulation buffers by writing sums as uint16 in place is complex.
                // Simpler: write to temporary stack arrays per row.
            }
            _ = u; _ = v // handled below
        }
        // Now compute chroma on even rows by averaging this row and the next row
        if (row & 1) == 0 {
            // Average U and V across a 2x2 block
            nextSrcOff := srcOff + w*2
            if row+1 < h {
                for cx := 0; cx < halfW; cx++ {
                    // current row samples
                    i0 := srcOff + cx*4
                    u0 := int(src[i0+0])
                    v0 := int(src[i0+2])
                    // next row samples
                    i1 := nextSrcOff + cx*4
                    u1 := int(src[i1+0])
                    v1 := int(src[i1+2])
                    uAvg := byte((u0 + u1) >> 1)
                    vAvg := byte((v0 + v1) >> 1)
                    uPlane[(row/2)*halfW+cx] = uAvg
                    vPlane[(row/2)*halfW+cx] = vAvg
                }
            } else {
                // Last row (odd height shouldn't happen); just copy chroma from this row
                for cx := 0; cx < halfW; cx++ {
                    i0 := srcOff + cx*4
                    u0 := src[i0+0]
                    v0 := src[i0+2]
                    uPlane[(row/2)*halfW+cx] = u0
                    vPlane[(row/2)*halfW+cx] = v0
                }
            }
        }
    }
}
