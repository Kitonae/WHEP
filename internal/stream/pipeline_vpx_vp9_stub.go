//go:build !vpx

package stream

import "errors"

// StartVP9Pipeline is unavailable without vpx/cgo build tags.
func StartVP9Pipeline(cfg PipelineConfig) (*PipelineVP9, error) {
    return nil, errors.New("vp9 pipeline not available (cgo off)")
}

type PipelineVP9 struct{}

func (p *PipelineVP9) Stop() {}
