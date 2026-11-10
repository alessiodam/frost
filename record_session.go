package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/alessiodam/frost/protocol/screencopy"
)

type (
	screencopyBufferEvent     = screencopy.ZwlrScreencopyFrameV1BufferEvent
	screencopyBufferDoneEvent = screencopy.ZwlrScreencopyFrameV1BufferDoneEvent
	screencopyFlagsEvent      = screencopy.ZwlrScreencopyFrameV1FlagsEvent
	screencopyReadyEvent      = screencopy.ZwlrScreencopyFrameV1ReadyEvent
	screencopyFailedEvent     = screencopy.ZwlrScreencopyFrameV1FailedEvent
)

const (
	stateCountdown  = "countdown"
	stateRecording  = "recording"
	statePaused     = "paused"
	stateFinalizing = "finalizing"
)

type recorder struct {
	a      *app
	cfg    *config
	o      *recOptions
	target *recTarget
	enc    encoder

	mu    sync.Mutex
	state string
	frame *canvas
	mask  []uint8
	scale float64

	sources    []*capSource
	countUntil time.Time
	firstInfo  time.Time

	clock     recClock
	encoder   *exec.Cmd
	stdin     io.WriteCloser
	stderr    tailBuffer
	audio     []*audioPump
	writerEnd chan struct{}
	writerRun chan struct{}
	frames    atomic.Int64

	stopped   atomic.Bool
	cancelled atomic.Bool
	stopErr   atomic.Pointer[error]
	wakeFD    int
	finished  chan struct{}
	saved     string
	tmpPath   string
	finalPath string

	badge     *layerBox
	badgeInfo *badge
	outlines  []*layerBox
	drawn     string
}

func record(a *app, cfg *config, o *recOptions, t *recTarget) error {
	if o.out != "" {
		if _, err := os.Stat(o.out); err == nil {
			return fmt.Errorf("%s already exists", o.out)
		}
	}
	enc, err := chooseEncoder(o)
	if err != nil {
		return err
	}
	if err := validateAudioSources(o.audioSources()); err != nil {
		return err
	}
	listener, err := controlListen()
	if err != nil {
		return err
	}
	defer listener.Close()

	wakeFD, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(wakeFD)

	a.quit = false
	r := &recorder{a: a, cfg: cfg, o: o, target: t, enc: enc, wakeFD: wakeFD, finished: make(chan struct{}),
		state: stateCountdown, writerEnd: make(chan struct{}), writerRun: make(chan struct{})}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for range signals {
			r.requestStop(nil)
		}
	}()

	var served sync.WaitGroup
	go r.serve(listener, &served)

	r.setupIndicator()
	if o.delay > 0 {
		r.countUntil = time.Now().Add(o.delay)
	}
	loopErr := r.loop()
	err = r.finish(loopErr)
	close(r.finished)
	finished := make(chan struct{})
	go func() {
		served.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
	}
	return err
}

func (r *recorder) wake() {
	var one [8]byte
	one[0] = 1
	unix.Write(r.wakeFD, one[:])
}

func (r *recorder) requestStop(err error) {
	if err != nil {
		r.stopErr.CompareAndSwap(nil, &err)
	}
	r.stopped.Store(true)
	r.wake()
}

func (r *recorder) currentState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *recorder) setState(s string) {
	r.mu.Lock()
	r.state = s
	r.mu.Unlock()
	r.wake()
}

func (r *recorder) togglePause(want string) string {
	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	if state != stateRecording && state != statePaused {
		return state
	}
	next := stateRecording
	switch want {
	case "pause":
		next = statePaused
	case "toggle":
		if state == stateRecording {
			next = statePaused
		}
	}
	r.clock.setPaused(next == statePaused)
	r.setState(next)
	return next
}

func (r *recorder) startCapture() error {
	if !r.showBadgeWhileRecording() {
		r.destroyBadge()
		if err := r.a.roundtrip(); err != nil {
			return err
		}
	}
	for _, source := range r.o.audioSources() {
		p, err := startAudio(source, &r.clock, r.o.audioOffset)
		if err != nil {
			return err
		}
		r.audio = append(r.audio, p)
	}
	if err := r.buildSources(); err != nil {
		return err
	}
	r.setState(stateRecording)
	return nil
}

func (r *recorder) onSourceInfo() error {
	if r.frame != nil {
		return nil
	}
	for _, s := range r.sources {
		if !s.haveInfo {
			return nil
		}
	}
	w, h := r.canvasSize()
	if w <= 0 || h <= 0 {
		return fmt.Errorf("nothing to record: %dx%d", w, h)
	}
	if err := r.startEncoder(w, h); err != nil {
		return err
	}
	r.buildMask(w, h)
	c := &canvas{make([]byte, w*h*4), w, h}
	r.mu.Lock()
	r.frame = c
	r.mu.Unlock()
	r.firstInfo = time.Now()
	return nil
}

func (r *recorder) reservePaths() error {
	final := r.o.out
	if final == "" {
		f, path, err := createUnique(recordingDir(r.o), formatExt[r.o.format])
		if err != nil {
			return err
		}
		f.Close()
		os.Remove(path)
		final = path
	} else if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	r.mu.Lock()
	r.finalPath = final
	r.tmpPath = strings.TrimSuffix(final, filepath.Ext(final)) + ".part.mkv"
	r.mu.Unlock()
	return nil
}

func (r *recorder) startEncoder(w, h int) error {
	if err := r.reservePaths(); err != nil {
		return err
	}
	cmd := exec.Command("ffmpeg", r.o.encoderArgs(r.enc, w, h, len(r.audio), r.tmpPath)...)
	cmd.Stderr = &r.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for _, p := range r.audio {
		cmd.ExtraFiles = append(cmd.ExtraFiles, p.pipeR)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r.encoder, r.stdin = cmd, stdin
	for _, p := range r.audio {
		p.pipeR.Close()
	}
	go r.writeFrames(w, h)
	return nil
}

func (r *recorder) writeFrames(w, h int) {
	defer close(r.writerRun)
	buf := make([]byte, w*h*4)
	interval := time.Second / time.Duration(r.o.fps)
	var written int64
	timer := time.NewTimer(interval)
	defer timer.Stop()
	write := func() bool {
		due := int64(r.clock.elapsed().Seconds() * float64(r.o.fps))
		if due <= written {
			return true
		}
		r.mu.Lock()
		copy(buf, r.frame.pix)
		r.mu.Unlock()
		for ; written < due; written++ {
			if _, err := r.stdin.Write(buf); err != nil {
				r.requestStop(fmt.Errorf("encoder stopped: %s", lastLine([]byte(r.stderr.String()), err)))
				return false
			}
			r.frames.Store(written + 1)
		}
		return true
	}
	for {
		select {
		case <-r.writerEnd:
			write()
			return
		case <-timer.C:
			if !write() {
				return
			}
			timer.Reset(interval - r.clock.elapsed()%interval)
		}
	}
}

func (r *recorder) loop() error {
	fd, err := waylandFD(r.a.ctx)
	if err != nil {
		return err
	}
	interval := time.Second / time.Duration(r.o.fps)
	for {
		if r.stopped.Load() {
			if p := r.stopErr.Load(); p != nil {
				return *p
			}
			return nil
		}
		if r.a.err != nil {
			return r.a.err
		}
		now := time.Now()
		state := r.currentState()
		timeout := 250 * time.Millisecond

		if state == stateCountdown {
			if remaining := r.countUntil.Sub(now); remaining > 0 {
				timeout = min(timeout, remaining)
			} else if err := r.startCapture(); err != nil {
				return err
			}
			state = r.currentState()
		}

		if state == stateRecording {
			for _, s := range r.sources {
				if s.inflight {
					continue
				}
				if wait := s.nextAt.Sub(now); wait > 0 {
					timeout = min(timeout, wait)
					continue
				}
				s.inflight = true
				s.nextAt = now.Add(interval)
				if err := s.request(); err != nil {
					return err
				}
				if s.buf == nil && s.fit {
					s.inflight = false
				}
			}
		}

		if r.frame != nil {
			if running, _ := r.clock.state(); !running {
				ready := true
				for _, s := range r.sources {
					ready = ready && s.hasFrame
				}
				if ready || now.Sub(r.firstInfo) > time.Second {
					r.clock.start()
				}
			}
		}
		if r.o.duration > 0 && r.clock.elapsed() >= r.o.duration {
			return nil
		}
		r.updateBadge()

		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(r.wakeFD), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, max(int(timeout.Milliseconds()), 1)); err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
		if fds[1].Revents != 0 {
			var drain [8]byte
			unix.Read(r.wakeFD, drain[:])
		}
		if fds[0].Revents != 0 {
			if err := r.a.dispatch(); err != nil {
				return fmt.Errorf("compositor connection lost: %w", err)
			}
		}
	}
}

func (r *recorder) finish(loopErr error) error {
	r.setState(stateFinalizing)
	r.destroyIndicator()
	if r.a.err == nil && loopErr == nil {
		r.a.roundtrip()
	}
	if r.encoder == nil {
		for _, p := range r.audio {
			p.finish()
		}
		return loopErr
	}
	close(r.writerEnd)
	<-r.writerRun
	r.stdin.Close()
	for _, p := range r.audio {
		p.finish()
	}
	encErr := r.encoder.Wait()
	length := r.clock.elapsed()
	w, h := r.frame.w, r.frame.h

	if r.cancelled.Load() {
		os.Remove(r.tmpPath)
		return loopErr
	}
	if r.frames.Load() == 0 {
		os.Remove(r.tmpPath)
		if loopErr != nil {
			return loopErr
		}
		if encErr != nil {
			return fmt.Errorf("encoder: %s", lastLine([]byte(r.stderr.String()), encErr))
		}
		return errors.New("recording stopped before any frame was captured")
	}
	if encErr != nil && !fileHasData(r.tmpPath) {
		os.Remove(r.tmpPath)
		return fmt.Errorf("encoder: %s", lastLine([]byte(r.stderr.String()), encErr))
	}
	if err := finalizeRecording(r.o, r.tmpPath, r.finalPath); err != nil {
		kept := strings.TrimSuffix(r.tmpPath, ".part.mkv") + ".mkv"
		if renameErr := os.Rename(r.tmpPath, kept); renameErr != nil {
			kept = r.tmpPath
		}
		r.saved = kept
		return fmt.Errorf("%w; recording kept at %s", err, kept)
	}
	r.saved = r.finalPath
	deliverVideo(r.finalPath, r.o, r.cfg, w, h, length)
	if loopErr != nil {
		return fmt.Errorf("%w; recording saved to %s", loopErr, r.finalPath)
	}
	return nil
}

func fileHasData(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

type recStatus struct {
	State   string   `json:"state"`
	Elapsed float64  `json:"elapsed"`
	File    string   `json:"file,omitempty"`
	Size    int64    `json:"size,omitempty"`
	Target  string   `json:"target,omitempty"`
	Width   int      `json:"width,omitempty"`
	Height  int      `json:"height,omitempty"`
	FPS     int      `json:"fps,omitempty"`
	Frames  int64    `json:"frames,omitempty"`
	Encoder string   `json:"encoder,omitempty"`
	Audio   []string `json:"audio,omitempty"`
}

func (r *recorder) status() recStatus {
	st := recStatus{State: r.currentState(), Elapsed: r.clock.elapsed().Seconds(), Target: r.target.label,
		FPS: r.o.fps, Frames: r.frames.Load(), Encoder: r.enc.name, Audio: r.o.audioSources()}
	r.mu.Lock()
	if r.frame != nil {
		st.Width, st.Height = r.frame.w, r.frame.h
	}
	st.File = r.finalPath
	tmp := r.tmpPath
	r.mu.Unlock()
	if info, err := os.Stat(tmp); err == nil {
		st.Size = info.Size()
	}
	return st
}

func controlPath() string {
	display := firstNonEmpty(os.Getenv("WAYLAND_DISPLAY"), "wayland-0")
	return filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "frost-record-"+filepath.Base(display)+".sock")
}

func controlListen() (net.Listener, error) {
	path := controlPath()
	l, err := net.Listen("unix", path)
	if err == nil {
		return l, nil
	}
	if conn, dialErr := net.Dial("unix", path); dialErr == nil {
		conn.Close()
		return nil, errors.New("another recording is active, run: frost record stop")
	}
	os.Remove(path)
	return net.Listen("unix", path)
}

func controlSend(cmd string) (string, error) {
	conn, err := net.Dial("unix", controlPath())
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, cmd); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func controlCommand(cmd string) error {
	reply, err := controlSend(cmd)
	if err != nil {
		if cmd == "status" {
			fmt.Println(`{"state":"idle","elapsed":0}`)
			return nil
		}
		return errors.New("no active recording")
	}
	if strings.HasPrefix(reply, "error: ") {
		return errors.New(strings.TrimPrefix(reply, "error: "))
	}
	fmt.Println(reply)
	return nil
}

func (r *recorder) serve(l net.Listener, served *sync.WaitGroup) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		served.Add(1)
		go func() {
			defer served.Done()
			defer conn.Close()
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				return
			}
			fmt.Fprintln(conn, r.handle(strings.TrimSpace(line)))
		}()
	}
}

func (r *recorder) handle(cmd string) string {
	switch cmd {
	case "status":
		out, err := json.Marshal(r.status())
		if err != nil {
			return "error: " + err.Error()
		}
		return string(out)
	case "pause":
		return r.togglePause("pause")
	case "resume":
		return r.togglePause("resume")
	case "toggle-pause":
		return r.togglePause("toggle")
	case "cancel":
		r.cancelled.Store(true)
		r.requestStop(nil)
		<-r.finished
		return "cancelled"
	case "stop":
		r.requestStop(nil)
		<-r.finished
		if r.saved != "" {
			return r.saved
		}
		return "stopped"
	}
	return "error: unknown command " + cmd
}
