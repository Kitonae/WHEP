package stream

import (
    "image"
    "image/png"
    "math"
    "os"
    "time"
)

// PipelineConfig defines how to produce encoded video and feed a Pion Track.
type PipelineConfig struct {
	Width, Height int
	FPS           int
	BitrateKbps   int // used by VP8/VP9/AV1 pipelines
	Source        Source
	// Track expects a Pion track with WriteSample(media.Sample) (e.g., *webrtc.TrackLocalStaticSample).
	Track interface{}
	// Optional VP8 tuning (ignored by other codecs)
	VP8Speed     int // maps to libvpx VP8E_SET_CPUUSED
	VP8Dropframe int // maps to rc_dropframe_thresh
}

// optional capability: source can advertise its pixel format (e.g., "bgra", "uyvy422")
type sourcePixFmt interface{ PixFmt() string }

// Source produces raw BGRA frames of fixed size and FPS.
type Source interface {
	// Next returns a frame of BGRA bytes (len = width*height*4) and a boolean false if source is closed.
	Next() ([]byte, bool)
	Stop()
}

// SourceSynthetic generates a moving gradient pattern.
type SourceSynthetic int64

func (s SourceSynthetic) Next() ([]byte, bool) { return nil, true }
func (s SourceSynthetic) Stop()                {}

// optional capability: some sources can report their last frame size
type sourceWithLast interface {
	Last() ([]byte, int, int, bool)
}

// --- Synthetic source ---

type synthetic struct {
    w, h, fps int
    buf       []byte
    t0        time.Time
    stop      bool
    // optional cached logo (BGRA)
    logoBuf   []byte
    logoW, logoH int
    logoTried bool
}

func NewSynthetic(w, h, fps int, seed int64) Source {
	return &synthetic{w: w, h: h, fps: fps, buf: make([]byte, w*h*4), t0: time.Now()}
}

func (s *synthetic) Next() ([]byte, bool) {
	if s.stop {
		return nil, false
	}
	now := time.Since(s.t0).Seconds()

	w, h := s.w, s.h
	if w <= 0 || h <= 0 {
		return nil, true
	}

	// Constants from the shader (tuned for CPU rendering)
	// Increase speeds so each frame visibly changes (avoid encoder dropframes)
	overallSpeed := 0.25
	gridSmoothWidth := 0.015 // kept for parity (not drawn)
	_ = gridSmoothWidth
	axisWidth := 0.05
	_ = axisWidth
	majorLineWidth := 0.025
	_ = majorLineWidth
	minorLineWidth := 0.0125
	_ = minorLineWidth
	majorLineFrequency := 5.0
	_ = majorLineFrequency
	minorLineFrequency := 1.0
	_ = minorLineFrequency
	// gridColor := vec4(0.5) // not used
	scale := 5.0
	lineColor := [4]float64{0.25, 0.5, 1.0, 1.0}
	minLineWidth := 0.02
	maxLineWidth := 0.5
	lineSpeed := 1.5 * overallSpeed
	lineAmplitude := 1.0
	lineFrequency := 0.2
	warpSpeed := 1.0 * overallSpeed
	warpFrequency := 0.5
	warpAmplitude := 1.0
	offsetFrequency := 0.5
	offsetSpeed := 1.7 * overallSpeed
	minOffsetSpread := 0.6
	maxOffsetSpread := 2.0
	// Slightly fewer lines for performance while keeping the look
	linesPerGroup := 12

	// Helper functions mirroring parts of the GLSL shader
	random := func(t float64) float64 {
		return (math.Cos(t) + math.Cos(t*1.3+1.3) + math.Cos(t*1.4+1.4)) / 3.0
	}
	mix := func(a, b, t float64) float64 { return a*(1.0-t) + b*t }

	// Optimized CPU renderer: draw background row-wise, then rasterize lines column-wise
	fw := float64(w)
	fh := float64(h)
	resx := fw

	// Precompute uvx, horizontal fade, and sx per column
	uvx := make([]float64, w)
	hfade := make([]float64, w)
	sxcol := make([]float64, w)
	for x := 0; x < w; x++ {
		u := float64(x) / (fw - 1)
		uvx[x] = u
		hfade[x] = 1.0 - (math.Cos(u*6.28)*0.5 + 0.5)
		sxcol[x] = (float64(x) - fw/2) / resx * 2.0 * scale
	}

	// Background gradient endpoints (as before)
	bg1 := [3]float64{lineColor[0] * 0.5, lineColor[1] * 0.5, lineColor[2] * 0.5}
	bg2 := [3]float64{lineColor[0] - 0.2, lineColor[1] - 0.2, lineColor[2] - 0.7}

	// Fill background
	for y := 0; y < h; y++ {
		uvy := float64(y) / (fh - 1)
		vfade := 1.0 - (math.Cos(uvy*6.28)*0.5 + 0.5)
		row := y * w * 4
		for x := 0; x < w; x++ {
			t := uvx[x]
			r := mix(bg1[0], bg2[0], t) * vfade
			g := mix(bg1[1], bg2[1], t) * vfade
			b := mix(bg1[2], bg2[2], t) * vfade
			off := row + x*4
			s.buf[off+0] = byte(b * 255)
			s.buf[off+1] = byte(g * 255)
			s.buf[off+2] = byte(r * 255)
			s.buf[off+3] = 255
		}
	}

	// Factor to convert space Y units to pixel delta
	pxPerUnit := resx / (2.0 * scale)
	// Precompute per-column warp and base plasma to avoid repeated trig
	warpY := make([]float64, w)
	basePlasma := make([]float64, w)
	tWarp := now * warpSpeed
	tPlasma := now * lineSpeed
	for x := 0; x < w; x++ {
		sx := sxcol[x]
		hf := hfade[x]
		warpY[x] = random(sx*warpFrequency+tWarp) * warpAmplitude * (0.5 + hf)
		basePlasma[x] = random(sx*lineFrequency + tPlasma)
	}

    // draw lines by rasterization (x then a small y neighborhood) with per-line precomputes
    for l := 0; l < linesPerGroup; l++ {
		nIdx := float64(l) / float64(linesPerGroup)
		offsetTime := now * offsetSpeed
		// per-line arrays
		randv := make([]float64, w)
		halfW := make([]float64, w)
		offsetArr := make([]float64, w)
		yCenter := make([]int, w)
		thickPx := make([]int, w)
		e0 := make([]float64, w)
		e1 := make([]float64, w)
		lr := make([]float64, w)
		lg := make([]float64, w)
		lb := make([]float64, w)
		for x := 0; x < w; x++ {
			sx := sxcol[x]
			hf := hfade[x]
			offsetPosition := float64(l) + sx*offsetFrequency
			rbase := random(offsetPosition + offsetTime)
			randv[x] = rbase*0.5 + 0.5
			halfW[x] = mix(minLineWidth, maxLineWidth, randv[x]*hf) / 2.0
			offsetArr[x] = random(offsetPosition+offsetTime*(1.0+nIdx)) * mix(minOffsetSpread, maxOffsetSpread, hf)
			linePos := basePlasma[x]*hf*lineAmplitude + offsetArr[x] + warpY[x]
			yCenter[x] = int(fh/2 + linePos*pxPerUnit)
			tp := int(halfW[x]*pxPerUnit) + 1
			if tp < 1 {
				tp = 1
			}
			thickPx[x] = tp
			e1[x] = halfW[x] * 0.15
			e0[x] = e1[x] + gridSmoothWidth
			// Color scaling
			lr[x] = lineColor[0] * randv[x]
			lg[x] = lineColor[1] * randv[x]
			lb[x] = lineColor[2] * randv[x]
		}
		// rasterize
		for x := 0; x < w; x++ {
			yc := yCenter[x]
			tp := thickPx[x]
			e0x := e0[x]
			e1x := e1[x]
			half := halfW[x]
			// Vertical neighborhood blend (reduced span)
			for dy := -tp; dy <= tp; dy++ {
				yy := yc + dy
				if yy < 0 || yy >= h {
					continue
				}
				dspace := math.Abs(float64(dy)) / pxPerUnit
				// smooth component
				var sm float64
				if half > 0 {
					u := 1.0 - dspace/half
					if u > 0 {
						if u >= 1 {
							sm = 1
						} else {
							sm = u * u * (3 - 2*u)
						}
					}
				}
				// crisp component
				var cr float64
				if e0x != e1x {
					u := (e0x - dspace) / (e0x - e1x)
					if u > 0 {
						if u >= 1 {
							cr = 1
						} else {
							cr = u * u * (3 - 2*u)
						}
					}
				} else if dspace < e1x {
					cr = 1
				}
				lineV := sm*0.5 + cr
				if lineV <= 0 {
					continue
				}
				off := (yy*w + x) * 4
				r0 := float64(s.buf[off+2]) / 255.0
				g0 := float64(s.buf[off+1]) / 255.0
				b0 := float64(s.buf[off+0]) / 255.0
				r1 := r0 + lineV*lr[x]
				g1 := g0 + lineV*lg[x]
				b1 := b0 + lineV*lb[x]
				if r1 > 1 {
					r1 = 1
				}
				if g1 > 1 {
					g1 = 1
				}
				if b1 > 1 {
					b1 = 1
				}
				s.buf[off+2] = byte(r1 * 255)
				s.buf[off+1] = byte(g1 * 255)
				s.buf[off+0] = byte(b1 * 255)
			}
		}
    }

    // Optional NDI logo: if assets/NDI.png exists, center it and alpha-blend
    if !s.logoTried && s.logoBuf == nil {
        s.logoTried = true
        if f, err := os.Open("assets/NDI.png"); err == nil {
            if img, err2 := png.Decode(f); err2 == nil {
                b := img.Bounds()
                lw, lh := b.Dx(), b.Dy()
                buf := make([]byte, lw*lh*4)
                switch src := img.(type) {
                case *image.NRGBA:
                    stride := src.Stride
                    for yy := 0; yy < lh; yy++ {
                        for xx := 0; xx < lw; xx++ {
                            si := yy*stride + xx*4
                            r := src.Pix[si+0]
                            g := src.Pix[si+1]
                            b0 := src.Pix[si+2]
                            a := src.Pix[si+3]
                            di := (yy*lw + xx) * 4
                            buf[di+0] = b0
                            buf[di+1] = g
                            buf[di+2] = r
                            buf[di+3] = a
                        }
                    }
                default:
                    for yy := 0; yy < lh; yy++ {
                        for xx := 0; xx < lw; xx++ {
                            r, g, b0, a := img.At(xx, yy).RGBA()
                            di := (yy*lw + xx) * 4
                            buf[di+0] = byte(b0 >> 8)
                            buf[di+1] = byte(g >> 8)
                            buf[di+2] = byte(r >> 8)
                            buf[di+3] = byte(a >> 8)
                        }
                    }
                }
                s.logoBuf = buf
                s.logoW, s.logoH = lw, lh
            }
            _ = f.Close()
        }
    }
    if s.logoBuf != nil && s.logoW > 0 && s.logoH > 0 {
        tgtH := int(fh * 0.35)
        if tgtH < 8 { tgtH = 8 }
        tgtW := int(float64(tgtH) * float64(s.logoW) / float64(s.logoH))
        if tgtW < 8 { tgtW = 8 }
        if tgtW > w { tgtW = w }
        if tgtH > h { tgtH = h }
        ox := (w - tgtW) / 2
        oy := (h - tgtH) / 2
        for yy := 0; yy < tgtH; yy++ {
            sy := (yy * s.logoH) / tgtH
            for xx := 0; xx < tgtW; xx++ {
                sx := (xx * s.logoW) / tgtW
                si := (sy*s.logoW + sx) * 4
                lb := s.logoBuf[si+0]
                lg := s.logoBuf[si+1]
                lr := s.logoBuf[si+2]
                la := s.logoBuf[si+3]
                if la == 0 { continue }
                dx := ox + xx
                dy := oy + yy
                di := (dy*w + dx) * 4
                a := uint32(la)
                inv := 255 - la
                s.buf[di+0] = byte((uint32(lb)*a + uint32(s.buf[di+0])*uint32(inv)) / 255)
                s.buf[di+1] = byte((uint32(lg)*a + uint32(s.buf[di+1])*uint32(inv)) / 255)
                s.buf[di+2] = byte((uint32(lr)*a + uint32(s.buf[di+2])*uint32(inv)) / 255)
            }
        }
    }
    return s.buf, true
}

func (s *synthetic) Stop() { s.stop = true }
