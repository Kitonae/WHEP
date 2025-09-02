//go:build !windows || !cgo

package ndi

type Receiver struct{}
type VideoFrame struct { W,H,Stride,FourCC int; Data []byte }

func Initialize() bool { return false }
func FindFirst(timeoutMs int) (string,string,bool) { return "","",false }
func NewReceiverByURL(url string) (*Receiver, error) { return nil, nil }
func (r *Receiver) CaptureVideo(timeoutMs int) (*VideoFrame, bool, error) { return nil, false, nil }
func (r *Receiver) Close() {}
type SourceInfo struct{ Name, URL string }
func ListSources(timeoutMs int) []SourceInfo { return nil }
