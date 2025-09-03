//go:build !yuv

package stream

// ColorConversionImpl reports the active color conversion backend.
func ColorConversionImpl() string { return "pure-go" }

