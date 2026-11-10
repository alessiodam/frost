package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	audioRate        = 48000
	audioBytesPerSec = audioRate * 4
	audioChunk       = 1920
	audioTolerance   = audioBytesPerSec * 30 / 1000 / 4 * 4
)

type encoder struct {
	name     string
	global   []string
	args     []string
	pixFmt   string
	hwFilter string
}

var softwareQuality = map[string]map[string]int{
	"h264": {"low": 28, "medium": 23, "high": 18},
	"hevc": {"low": 30, "medium": 26, "high": 21},
	"av1":  {"low": 42, "medium": 34, "high": 26},
	"vp9":  {"low": 42, "medium": 34, "high": 26},
}

var hardwareQuality = map[string]int{"low": 32, "medium": 26, "high": 20}

func softwareEncoder(o *recOptions) encoder {
	q := strconv.Itoa(softwareQuality[o.codec][o.quality])
	if o.crf >= 0 {
		q = strconv.Itoa(o.crf)
	}
	lossless := o.quality == "lossless" && o.crf < 0 && o.bitrate == ""
	e := encoder{pixFmt: "yuv420p"}
	if lossless {
		e.pixFmt = "yuv444p"
	}
	rate := []string{"-crf", q}
	if o.bitrate != "" {
		rate = []string{"-b:v", o.bitrate, "-maxrate", o.bitrate, "-bufsize", o.bitrate}
	}
	switch o.codec {
	case "h264":
		e.name = "libx264"
		e.args = []string{"-preset", firstNonEmpty(o.preset, "veryfast")}
		if lossless {
			rate = []string{"-qp", "0"}
		}
	case "hevc":
		e.name = "libx265"
		e.args = []string{"-preset", firstNonEmpty(o.preset, "veryfast"), "-x265-params", "log-level=error"}
		if lossless {
			rate = nil
			e.args[3] += ":lossless=1"
		}
	case "av1":
		e.name = "libsvtav1"
		e.args = []string{"-preset", firstNonEmpty(o.preset, "10")}
	case "vp9":
		e.name = "libvpx-vp9"
		e.args = []string{"-deadline", "realtime", "-cpu-used", firstNonEmpty(o.preset, "6"), "-row-mt", "1"}
		if lossless {
			rate = []string{"-lossless", "1"}
		} else if o.bitrate == "" {
			e.args = append(e.args, "-b:v", "0")
		}
	}
	e.args = append(e.args, rate...)
	return e
}

func hardwareRate(o *recOptions, flag string) []string {
	if o.bitrate != "" {
		return []string{"-b:v", o.bitrate, "-maxrate", o.bitrate}
	}
	q := hardwareQuality[o.quality]
	if o.crf >= 0 {
		q = o.crf
	}
	return []string{flag, strconv.Itoa(q)}
}

func vaapiEncoder(o *recOptions, device string) encoder {
	return encoder{
		name:     o.codec + "_vaapi",
		global:   []string{"-vaapi_device", device},
		args:     hardwareRate(o, "-qp"),
		pixFmt:   "nv12",
		hwFilter: "hwupload",
	}
}

func nvencEncoder(o *recOptions) encoder {
	args := append([]string{"-preset", firstNonEmpty(o.preset, "p4"), "-rc", "vbr"}, hardwareRate(o, "-cq")...)
	if o.bitrate == "" {
		args = append(args, "-b:v", "0")
	}
	return encoder{name: o.codec + "_nvenc", args: args, pixFmt: "yuv420p"}
}

func encoderCandidates(o *recOptions) []encoder {
	if o.quality == "lossless" && o.crf < 0 && o.bitrate == "" {
		return []encoder{softwareEncoder(o)}
	}
	var out []encoder
	if oneOf(o.hw, "auto", "vaapi") {
		devices, _ := filepath.Glob("/dev/dri/renderD*")
		if o.vaapiDevice != "" {
			devices = []string{o.vaapiDevice}
		}
		for _, d := range devices {
			out = append(out, vaapiEncoder(o, d))
		}
	}
	if oneOf(o.hw, "auto", "nvenc") && o.codec != "vp9" {
		out = append(out, nvencEncoder(o))
	}
	if oneOf(o.hw, "auto", "off") {
		out = append(out, softwareEncoder(o))
	}
	return out
}

func (e encoder) colorFilter() string {
	f := "scale=out_color_matrix=bt709:out_range=tv,format=" + e.pixFmt
	if e.hwFilter != "" {
		f += "," + e.hwFilter
	}
	return f
}

func (e encoder) probe() error {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	args = append(args, e.global...)
	args = append(args, "-f", "lavfi", "-i", "color=black:s=640x360:r=30", "-frames:v", "2", "-vf", e.colorFilter(), "-c:v", e.name)
	args = append(args, e.args...)
	args = append(args, "-f", "null", "-")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", e.name, lastLine(out, err))
	}
	return nil
}

func lastLine(out []byte, fallback error) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	if fallback != nil {
		return fallback.Error()
	}
	return "unknown error"
}

func chooseEncoder(o *recOptions) (encoder, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return encoder{}, errors.New("recording needs ffmpeg")
	}
	if o.animated() {
		return encoder{name: "ffv1", args: []string{"-level", "3", "-g", "1"}, pixFmt: "bgr0"}, nil
	}
	var failures []string
	for _, e := range encoderCandidates(o) {
		err := e.probe()
		if err == nil {
			return e, nil
		}
		failures = append(failures, err.Error())
	}
	return encoder{}, fmt.Errorf("no usable %s encoder: %s", o.codec, strings.Join(failures, "; "))
}

func (o *recOptions) videoFilter(e encoder) string {
	var chain []string
	if o.vf != "" {
		chain = append(chain, o.vf)
	}
	if o.scale != 1 {
		chain = append(chain, fmt.Sprintf("scale=trunc(iw*%g/2)*2:trunc(ih*%g/2)*2:flags=lanczos", o.scale, o.scale))
	}
	if o.animated() {
		return strings.Join(chain, ",")
	}
	chain = append(chain, "pad=ceil(iw/2)*2:ceil(ih/2)*2", e.colorFilter())
	return strings.Join(chain, ",")
}

func (o *recOptions) audioEncoderArgs() []string {
	codec := "aac"
	if o.format == "webm" {
		codec = "libopus"
	}
	return []string{"-c:a", codec, "-b:a", o.audioBitrate}
}

func (o *recOptions) encoderArgs(e encoder, w, h, audioInputs int, path string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	args = append(args, e.global...)
	args = append(args, "-thread_queue_size", "64", "-f", "rawvideo", "-pix_fmt", "bgr0",
		"-video_size", fmt.Sprintf("%dx%d", w, h), "-framerate", strconv.Itoa(o.fps), "-i", "pipe:0")
	for i := 0; i < audioInputs; i++ {
		args = append(args, "-thread_queue_size", "1024", "-f", "s16le", "-ar", strconv.Itoa(audioRate),
			"-ac", "2", "-i", fmt.Sprintf("pipe:%d", 3+i))
	}
	args = append(args, "-map", "0:v")
	switch {
	case audioInputs == 0:
	case audioInputs == 1 || o.audioTracks:
		for i := 0; i < audioInputs; i++ {
			args = append(args, "-map", fmt.Sprintf("%d:a", i+1))
		}
	default:
		var in strings.Builder
		for i := 0; i < audioInputs; i++ {
			fmt.Fprintf(&in, "[%d:a]", i+1)
		}
		args = append(args, "-filter_complex",
			fmt.Sprintf("%samix=inputs=%d:duration=longest:normalize=0[frostaudio]", in.String(), audioInputs),
			"-map", "[frostaudio]")
	}
	if f := o.videoFilter(e); f != "" {
		args = append(args, "-vf", f)
	}
	args = append(args, "-c:v", e.name)
	args = append(args, e.args...)
	if !o.animated() {
		args = append(args, "-g", strconv.Itoa(o.fps*2), "-colorspace", "bt709", "-color_primaries", "bt709",
			"-color_trc", "bt709", "-color_range", "tv")
	}
	if audioInputs > 0 {
		args = append(args, o.audioEncoderArgs()...)
	}
	args = append(args, strings.Fields(o.ffmpegArgs)...)
	return append(args, "-f", "matroska", path)
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

type recClock struct {
	mu          sync.Mutex
	running     bool
	paused      bool
	startedAt   time.Time
	accumulated time.Duration
}

func (c *recClock) start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		c.running, c.startedAt = true, time.Now()
	}
}

func (c *recClock) elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running || c.paused {
		return c.accumulated
	}
	return c.accumulated + time.Since(c.startedAt)
}

func (c *recClock) setPaused(p bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running || c.paused == p {
		return
	}
	if p {
		c.accumulated += time.Since(c.startedAt)
	} else {
		c.startedAt = time.Now()
	}
	c.paused = p
}

func (c *recClock) state() (running, paused bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running, c.paused
}

type audioPump struct {
	source  string
	cmd     *exec.Cmd
	stderr  tailBuffer
	out     io.ReadCloser
	first   chan struct{}
	chunks  chan []byte
	pipeR   *os.File
	pipeW   *os.File
	clock   *recClock
	offset  time.Duration
	written int64
	stop    chan struct{}
	done    chan struct{}
	err     error
}

func startAudio(source string, clock *recClock, offset time.Duration) (*audioPump, error) {
	p := &audioPump{source: source, clock: clock, offset: offset, first: make(chan struct{}), chunks: make(chan []byte, 256),
		stop: make(chan struct{}), done: make(chan struct{})}
	p.cmd = exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-f", "pulse",
		"-sample_rate", strconv.Itoa(audioRate), "-channels", "2", "-fragment_size", strconv.Itoa(audioChunk),
		"-name", "frost", "-stream_name", "recording", "-i", source,
		"-f", "s16le", "-ar", strconv.Itoa(audioRate), "-ac", "2", "pipe:1")
	p.cmd.Stderr = &p.stderr
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var err error
	if p.out, err = p.cmd.StdoutPipe(); err != nil {
		return nil, err
	}
	if p.pipeR, p.pipeW, err = os.Pipe(); err != nil {
		return nil, err
	}
	if err := p.cmd.Start(); err != nil {
		p.pipeR.Close()
		p.pipeW.Close()
		return nil, fmt.Errorf("audio %s: %w", source, err)
	}
	failed := make(chan error, 1)
	go p.read(failed)
	go p.run()
	select {
	case <-p.first:
		return p, nil
	case err := <-failed:
		p.finish()
		p.pipeR.Close()
		return nil, fmt.Errorf("audio %s: %s", source, lastLine([]byte(p.stderr.String()), err))
	case <-time.After(3 * time.Second):
		return p, nil
	}
}

func (p *audioPump) read(failed chan<- error) {
	var once sync.Once
	for {
		chunk := make([]byte, audioChunk)
		if _, err := io.ReadFull(p.out, chunk); err != nil {
			select {
			case <-p.stop:
			default:
				fmt.Fprintf(os.Stderr, "frost: audio source %s stopped: %s\n", p.source, lastLine([]byte(p.stderr.String()), err))
			}
			failed <- err
			close(p.chunks)
			return
		}
		once.Do(func() { close(p.first) })
		select {
		case p.chunks <- chunk:
		default:
		}
	}
}

func (p *audioPump) write(b []byte) {
	if p.err != nil {
		return
	}
	if _, err := p.pipeW.Write(b); err != nil {
		p.err = err
		return
	}
	p.written += int64(len(b))
}

func (p *audioPump) silence(n int64) {
	var zero [audioChunk]byte
	for n > 0 && p.err == nil {
		c := min(n, audioChunk)
		p.write(zero[:c])
		n -= c
	}
}

func (p *audioPump) position() int64 {
	return int64(max(p.clock.elapsed()+p.offset, 0).Seconds()*audioBytesPerSec) / 4 * 4
}

func (p *audioPump) deliver(chunk []byte) {
	running, paused := p.clock.state()
	if !running || paused {
		return
	}
	behind := p.position() - int64(len(chunk)) - p.written
	switch {
	case behind > audioTolerance:
		p.silence(behind)
	case behind < -audioTolerance:
		return
	}
	p.write(chunk)
}

func (p *audioPump) fill(limit int64) {
	if behind := p.position() - p.written; behind > limit {
		p.silence(behind - limit/2)
	}
}

func (p *audioPump) run() {
	defer close(p.done)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	chunks := p.chunks
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			p.deliver(chunk)
		case <-tick.C:
			if running, paused := p.clock.state(); running && !paused {
				p.fill(audioBytesPerSec / 5)
			}
		case <-p.stop:
			if running, _ := p.clock.state(); running {
				p.fill(0)
			}
			return
		}
	}
}

func (p *audioPump) finish() {
	close(p.stop)
	<-p.done
	p.pipeW.Close()
	p.abort()
}

func (p *audioPump) abort() {
	if p.cmd.Process != nil {
		p.cmd.Process.Signal(syscall.SIGTERM)
		timer := time.AfterFunc(2*time.Second, func() { p.cmd.Process.Kill() })
		p.out.Close()
		p.cmd.Wait()
		timer.Stop()
	}
}

func finalizeRecording(o *recOptions, tmp, final string) error {
	var filter []string
	switch o.format {
	case "mkv":
		return os.Rename(tmp, final)
	case "mp4":
		filter = []string{"-map", "0", "-c", "copy", "-movflags", "+faststart"}
		if o.codec == "hevc" {
			filter = append(filter, "-tag:v", "hvc1")
		}
	case "webm":
		filter = []string{"-map", "0", "-c", "copy"}
	case "gif":
		filter = []string{"-filter_complex", "[0:v]split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=sierra2_4a:diff_mode=rectangle", "-loop", "0"}
	case "webp":
		filter = []string{"-c:v", "libwebp_anim", "-lossless", "0", "-q:v", "80", "-loop", "0"}
	case "apng":
		filter = []string{"-c:v", "apng", "-plays", "0", "-f", "apng"}
	}
	args := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", tmp}, filter...)
	args = append(args, final)
	out, err := exec.Command("ffmpeg", args...).CombinedOutput()
	if err != nil {
		os.Remove(final)
		return fmt.Errorf("%s: %s", o.format, lastLine(out, err))
	}
	return os.Remove(tmp)
}

func deliverVideo(path string, o *recOptions, cfg *config, w, h int, length time.Duration) {
	copied := false
	if cfg.clipboard {
		var cmd *exec.Cmd
		if o.format == "gif" {
			cmd = exec.Command("wl-copy", "--type", "image/gif")
			if data, err := os.ReadFile(path); err == nil {
				cmd.Stdin = bytes.NewReader(data)
			}
		} else {
			cmd = exec.Command("wl-copy", "--type", "text/uri-list")
			cmd.Stdin = strings.NewReader((&url.URL{Scheme: "file", Path: path}).String() + "\r\n")
		}
		copied = cmd.Stdin != nil && cmd.Run() == nil
	}
	if cfg.notify {
		thumb := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), fmt.Sprintf("frost-thumb-%d.jpg", os.Getpid()))
		err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", path,
			"-frames:v", "1", "-vf", "scale=320:-2", thumb).Run()
		if err != nil {
			thumb = ""
		}
		title := "Recording saved"
		if copied {
			title = "Recording saved and copied"
		}
		body := fmt.Sprintf("%d × %d · %s · %s", w, h, length.Round(time.Second), filepath.Base(path))
		notifyOpen(thumb, path, title, body, thumb)
	}
	fmt.Println(path)
}
