//go:build !vpx

package stream

import "errors"

// StartVP8Pipeline is unavailable without vpx/cgo build tags.
func StartVP8Pipeline(cfg PipelineConfig) (*PipelineVP8, error) {
    return nil, errors.New("vp8 pipeline not available (cgo off)")
}

type PipelineVP8 struct{}

func (p *PipelineVP8) Stop() {}
