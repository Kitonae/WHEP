package stream

import (
    "bufio"
    "bytes"
    "errors"
    "io"
    "math/rand"
    "os/exec"
    "time"

    "github.com/pion/webrtc/v3/pkg/media"
)

// PipelineConfig defines how to produce H264 and feed a Pion Track.
type PipelineConfig struct {
    Width, Height int
    FPS           int
    Source        Source
    // Track expects a Pion track with WriteSample(media.Sample) (e.g., *webrtc.TrackLocalStaticSample).
    Track         interface{}
}

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

// StartH264Pipeline starts ffmpeg to encode raw BGRA frames from Source and writes AnnexB to Track as samples.
func StartH264Pipeline(cfg PipelineConfig) (*Pipeline, error) {
    if cfg.FPS <= 0 { cfg.FPS = 30 }
    if cfg.Width <= 0 { cfg.Width = 1280 }
    if cfg.Height <= 0 { cfg.Height = 720 }
    if cfg.Source == nil {
        cfg.Source = NewSynthetic(cfg.Width, cfg.Height, cfg.FPS, int64(rand.Int()))
    }
    p := &Pipeline{cfg: cfg}
    if err := p.start(); err != nil { return nil, err }
    return p, nil
}

type Pipeline struct {
    cfg   PipelineConfig
    cmd   *exec.Cmd
    stdin io.WriteCloser
    stdout io.ReadCloser
    quit  chan struct{}
}

func (p *Pipeline) start() error {
    // ffmpeg command: read raw BGRA from stdin, encode H264 veryfast zerolatency, output AnnexB to stdout
    args := []string{
        "-f", "rawvideo",
        "-pix_fmt", "bgra",
        "-s:v",  sizeArg(p.cfg.Width, p.cfg.Height),
        "-r",     itoa(p.cfg.FPS),
        "-i",     "-",
        "-an",
        "-c:v",  "libx264",
        "-preset", "veryfast",
        "-tune",   "zerolatency",
        "-pix_fmt", "yuv420p",
        "-f",     "h264",
        "-",
    }
    cmd := exec.Command("ffmpeg", args...)
    stdin, err := cmd.StdinPipe()
    if err != nil { return err }
    stdout, err := cmd.StdoutPipe()
    if err != nil { return err }
    cmd.Stderr = bufio.NewWriterSize(bytes.NewBuffer(nil), 0)
    if err := cmd.Start(); err != nil { return err }
    p.cmd, p.stdin, p.stdout = cmd, stdin, stdout
    p.quit = make(chan struct{})

    // Pump frames to ffmpeg stdin
    go func() {
        ticker := time.NewTicker(time.Second / time.Duration(p.cfg.FPS))
        defer ticker.Stop()
        for {
            select { case <-p.quit: return; case <-ticker.C: }
            frame, ok := p.cfg.Source.Next()
            if !ok { return }
            if len(frame) != p.cfg.Width*p.cfg.Height*4 { continue }
            if _, err := p.stdin.Write(frame); err != nil { return }
        }
    }()

    // Read AnnexB and write as samples
    go func() {
        r := bufio.NewReaderSize(p.stdout, 1<<20)
        dur := time.Second / time.Duration(p.cfg.FPS)
        for {
            au, err := readAnnexBAccessUnit(r)
            if err != nil { return }
            if len(au) == 0 { continue }
            // write to track
            if w, ok := p.cfg.Track.(interface{ WriteSample(media.Sample) error }); ok {
                _ = w.WriteSample(media.Sample{Data: au, Duration: dur})
            }
        }
    }()
    return nil
}

func (p *Pipeline) Stop() {
    close(p.quit)
    _ = p.stdin.Close()
    if p.cmd != nil { _ = p.cmd.Process.Kill(); _ = p.cmd.Wait() }
    if p.cfg.Source != nil { p.cfg.Source.Stop() }
}

// --- Synthetic source ---

type synthetic struct {
    w, h, fps int
    buf []byte
    t0 time.Time
    stop bool
}

func NewSynthetic(w, h, fps int, seed int64) Source {
    return &synthetic{w:w, h:h, fps:fps, buf:make([]byte, w*h*4), t0: time.Now()}
}

func (s *synthetic) Next() ([]byte, bool) {
    if s.stop { return nil, false }
    // simple moving gradient pattern in BGRA
    now := time.Since(s.t0).Seconds()
    for y:=0; y<s.h; y++ {
        for x:=0; x<s.w; x++ {
            off := (y*s.w + x) * 4
            r := byte((x + int(now*120)) % 256)
            g := byte((y + int(now*80)) % 256)
            b := byte((x+y + int(now*100)) % 256)
            s.buf[off+0] = b
            s.buf[off+1] = g
            s.buf[off+2] = r
            s.buf[off+3] = 255
        }
    }
    return s.buf, true
}

func (s *synthetic) Stop() { s.stop = true }

// --- H264 AnnexB helpers ---

func readAnnexBAccessUnit(r *bufio.Reader) ([]byte, error) {
    // Collect NAL units until we hit an AUD or IDR that starts a new AU; return previous AU.
    // For simplicity, read chunks and split on start codes, then group by AUD (type 9).
    // This is a minimal parser suitable for low-latency streaming.
    var au []byte
    for {
        nal, err := readNextAnnexBNAL(r)
        if err != nil {
            if errors.Is(err, io.EOF) && len(au) > 0 { return au, nil }
            return nil, err
        }
        if len(nal) < 1 { continue }
        naluType := nal[0] & 0x1F
        if naluType == 9 { // AUD starts a new AU; flush if we have one
            if len(au) > 0 { return au, nil }
            au = append(au, annexBStartCode...)
            au = append(au, nal...)
            continue
        }
        au = append(au, annexBStartCode...)
        au = append(au, nal...)
        // Heuristic: if IDR (5) and we already had some content, flush early to reduce latency
        if naluType == 5 && len(au) > len(annexBStartCode)+1 {
            return au, nil
        }
        // Otherwise keep appending until a new AUD or IDR triggers flush on next loop
        if len(au) > 1<<20 { // safety flush
            return au, nil
        }
    }
}

var annexBStartCode = []byte{0x00,0x00,0x00,0x01}

func readNextAnnexBNAL(r *bufio.Reader) ([]byte, error) {
    // scan for 0x000001/0x00000001 start codes and return payload until next start code
    // We assume ffmpeg outputs properly framed NALs.
    // Discard leading zeros
    for {
        b, err := r.ReadByte(); if err != nil { return nil, err }
        if b == 0 { // possible start
            r.UnreadByte(); break
        }
    }
    // read start code
    zeros := 0
    for {
        b, err := r.ReadByte(); if err != nil { return nil, err }
        if b == 0 { zeros++; if zeros>3 { zeros=3 } ; continue }
        if b == 1 && zeros >= 2 { break }
        zeros = 0 // false alarm
    }
    // now collect until next start code
    var buf []byte
    for {
        b, err := r.ReadByte()
        if err != nil {
            if errors.Is(err, io.EOF) { return buf, io.EOF }
            return nil, err
        }
        buf = append(buf, b)
        l := len(buf)
        if l >= 4 && buf[l-4] == 0 && buf[l-3] == 0 && ((buf[l-2] == 0 && buf[l-1] == 1) || (buf[l-2] == 1)) {
            // found start code; unread it (4 or 3 bytes)
            back := 3
            if buf[l-2] == 0 && buf[l-1] == 1 { back = 4 }
            for i:=0;i<back;i++ { r.UnreadByte() }
            buf = buf[:l-back]
            return buf, nil
        }
    }
}

func sizeArg(w,h int) string { return itoa(w)+"x"+itoa(h) }
func itoa(x int) string { return fmtInt(x) }

// tiny integer to string without fmt to keep deps small
func fmtInt(x int) string {
    if x==0 { return "0" }
    neg := false
    if x<0 { neg=true; x=-x }
    var a [20]byte
    i := len(a)
    for x>0 { i--; a[i] = byte('0'+x%10); x/=10 }
    if neg { i--; a[i]='-' }
    return string(a[i:])
}
