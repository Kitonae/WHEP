//go:build !aom && !svt

package stream

import "errors"

// StartAV1Pipeline is unavailable without cgo+aom build tags.
func StartAV1Pipeline(cfg PipelineConfig) (*PipelineAV1, error) {
    return nil, errors.New("av1 pipeline not available (build without 'aom' tag)")
}

type PipelineAV1 struct{}

func (p *PipelineAV1) Stop() {}
