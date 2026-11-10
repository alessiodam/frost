package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var formatExt = map[string]string{
	"mp4": ".mp4", "mkv": ".mkv", "webm": ".webm", "gif": ".gif", "webp": ".webp", "apng": ".apng",
}

type recOptions struct {
	fps          int
	quality      string
	crf          int
	bitrate      string
	preset       string
	codec        string
	hw           string
	vaapiDevice  string
	format       string
	scale        float64
	vf           string
	ffmpegArgs   string
	audio        string
	audioTracks  bool
	audioOffset  time.Duration
	audioBitrate string
	delay        time.Duration
	duration     time.Duration
	cursor       bool
	indicator    string
	dir          string
	out          string
	output       string
	all          bool
	region       string
	window       string
	mode         string
	noClipboard  bool
	noNotify     bool
	config       string
}

func newRecOptions() *recOptions {
	return &recOptions{quality: "high", crf: -1, codec: "auto", hw: "auto", format: "mp4", scale: 1,
		audio: "none", audioBitrate: "192k", cursor: true, indicator: "auto"}
}

func (o *recOptions) flagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("frost record", flag.ContinueOnError)
	fs.IntVar(&o.fps, "fps", o.fps, "frames per second (default 60, or 15 for gif, webp and apng)")
	fs.StringVar(&o.quality, "quality", o.quality, "low, medium, high or lossless")
	fs.IntVar(&o.crf, "crf", o.crf, "explicit quality value for the encoder, overrides -quality")
	fs.StringVar(&o.bitrate, "bitrate", o.bitrate, "target video bitrate such as 8M, overrides -quality")
	fs.StringVar(&o.preset, "preset", o.preset, "encoder preset for software encoders")
	fs.StringVar(&o.codec, "codec", o.codec, "auto, h264, hevc, av1 or vp9")
	fs.StringVar(&o.hw, "hw", o.hw, "hardware encoding: auto, off, vaapi or nvenc")
	fs.StringVar(&o.vaapiDevice, "vaapi-device", o.vaapiDevice, "VAAPI render node (default: first that works)")
	fs.StringVar(&o.format, "format", o.format, "mp4, mkv, webm, gif, webp or apng")
	fs.Float64Var(&o.scale, "scale", o.scale, "resize factor, for example 0.5")
	fs.StringVar(&o.vf, "vf", o.vf, "extra ffmpeg video filters")
	fs.StringVar(&o.ffmpegArgs, "ffmpeg-args", o.ffmpegArgs, "extra ffmpeg output arguments, split on spaces")
	fs.StringVar(&o.audio, "audio", o.audio, "none, desktop, mic, both, or comma-separated PulseAudio source names")
	fs.BoolVar(&o.audioTracks, "audio-tracks", o.audioTracks, "keep each audio source on its own track instead of mixing")
	fs.DurationVar(&o.audioOffset, "audio-offset", o.audioOffset, "shift audio relative to video, for example 40ms")
	fs.StringVar(&o.audioBitrate, "audio-bitrate", o.audioBitrate, "audio bitrate")
	fs.DurationVar(&o.delay, "delay", o.delay, "countdown before recording starts, for example 3s")
	fs.DurationVar(&o.duration, "duration", o.duration, "stop automatically after this long")
	fs.BoolVar(&o.cursor, "cursor", o.cursor, "include the mouse cursor")
	fs.StringVar(&o.indicator, "indicator", o.indicator, "auto keeps it out of the video, on forces it, off hides it")
	fs.StringVar(&o.dir, "dir", o.dir, "save directory (default: $XDG_VIDEOS_DIR/Screencasts)")
	fs.StringVar(&o.out, "o", o.out, "output file, its extension selects the format")
	fs.StringVar(&o.output, "output", o.output, "record this output, skips the picker")
	fs.BoolVar(&o.all, "all", o.all, "record every output on one canvas, skips the picker")
	fs.StringVar(&o.region, "region", o.region, `record this global region "x,y wxh", skips the picker`)
	fs.StringVar(&o.window, "window", o.window, "record a window: focused, app_id=regex, title=regex, con_id=N or pid=N")
	fs.StringVar(&o.mode, "mode", o.mode, "initial picker mode: rect, free, window or last")
	fs.BoolVar(&o.noClipboard, "no-clipboard", o.noClipboard, "don't copy to the clipboard")
	fs.BoolVar(&o.noNotify, "no-notify", o.noNotify, "don't show a notification")
	fs.StringVar(&o.config, "config", o.config, "config file (default: $XDG_CONFIG_HOME/frost/config)")
	return fs
}

func (o *recOptions) applyConfig(fs *flag.FlagSet, entries [][2]string) error {
	for _, e := range entries {
		name := strings.ReplaceAll(e[0], "_", "-")
		if fs.Lookup(name) == nil {
			return fmt.Errorf("[record]: unknown option %q", e[0])
		}
		if err := fs.Set(name, e[1]); err != nil {
			return fmt.Errorf("[record] %s: %w", e[0], err)
		}
	}
	return nil
}

func configuredRecOptions(cfg *config) (*recOptions, error) {
	o := newRecOptions()
	if err := o.applyConfig(o.flagSet(), cfg.record); err != nil {
		return nil, err
	}
	return o, o.normalize()
}

func (o *recOptions) normalize() error {
	if o.out != "" {
		o.out = expandPath(o.out)
		ext := strings.ToLower(filepath.Ext(o.out))
		matched := false
		for format, e := range formatExt {
			if e == ext {
				o.format, matched = format, true
			}
		}
		if !matched {
			return fmt.Errorf("-o: unsupported extension %q", ext)
		}
	}
	o.dir = expandPath(o.dir)
	if _, ok := formatExt[o.format]; !ok {
		return fmt.Errorf("format: expected mp4, mkv, webm, gif, webp or apng, got %q", o.format)
	}
	if o.fps == 0 {
		o.fps = 60
		if o.animated() {
			o.fps = 15
		}
	}
	if o.fps < 1 || o.fps > 240 {
		return fmt.Errorf("fps: expected 1-240, got %d", o.fps)
	}
	if !oneOf(o.quality, "low", "medium", "high", "lossless") {
		return fmt.Errorf("quality: expected low, medium, high or lossless, got %q", o.quality)
	}
	if !oneOf(o.codec, "auto", "h264", "hevc", "av1", "vp9") {
		return fmt.Errorf("codec: expected auto, h264, hevc, av1 or vp9, got %q", o.codec)
	}
	if o.codec == "auto" {
		o.codec = "h264"
		if o.format == "webm" {
			o.codec = "vp9"
		}
	}
	if o.format == "webm" && !oneOf(o.codec, "vp9", "av1") {
		return fmt.Errorf("webm only holds vp9 or av1, not %s", o.codec)
	}
	if o.format == "mp4" && o.codec == "vp9" {
		return errors.New("use webm or mkv for vp9")
	}
	if o.quality == "lossless" && o.codec == "av1" {
		return errors.New("lossless is not available for av1")
	}
	if !oneOf(o.hw, "auto", "off", "vaapi", "nvenc") {
		return fmt.Errorf("hw: expected auto, off, vaapi or nvenc, got %q", o.hw)
	}
	if !oneOf(o.indicator, "auto", "on", "off") {
		return fmt.Errorf("indicator: expected auto, on or off, got %q", o.indicator)
	}
	if o.scale <= 0 || o.scale > 8 || math.IsNaN(o.scale) {
		return fmt.Errorf("scale: expected a factor above 0, got %v", o.scale)
	}
	if o.delay < 0 || o.duration < 0 {
		return errors.New("delay and duration can't be negative")
	}
	if o.animated() && len(o.audioSources()) > 0 {
		return fmt.Errorf("%s can't hold audio", o.format)
	}
	if o.mode != "" && o.mode != "last" {
		if _, ok := modeNames[o.mode]; !ok {
			return fmt.Errorf("unknown mode %q", o.mode)
		}
	}
	return nil
}

func oneOf(v string, options ...string) bool {
	for _, option := range options {
		if v == option {
			return true
		}
	}
	return false
}

func (o *recOptions) animated() bool { return oneOf(o.format, "gif", "webp", "apng") }

func validateAudioSources(sources []string) error {
	out, err := exec.Command("pactl", "list", "short", "sources").Output()
	if err != nil {
		return nil
	}
	known := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 1 {
			known[f[1]] = true
		}
	}
	for _, source := range sources {
		if !strings.HasPrefix(source, "@") && !known[source] {
			return fmt.Errorf("audio %s: no such source, run: pactl list short sources", source)
		}
	}
	return nil
}

func (o *recOptions) audioSources() []string {
	switch o.audio {
	case "", "none":
		return nil
	case "desktop":
		return []string{"@DEFAULT_MONITOR@"}
	case "mic":
		return []string{"@DEFAULT_SOURCE@"}
	case "both":
		return []string{"@DEFAULT_MONITOR@", "@DEFAULT_SOURCE@"}
	}
	var sources []string
	for _, name := range strings.Split(o.audio, ",") {
		if name = strings.TrimSpace(name); name != "" {
			sources = append(sources, name)
		}
	}
	return sources
}

func recordMain(args []string) error {
	if len(args) > 0 && oneOf(args[0], "stop", "cancel", "pause", "resume", "toggle-pause", "status") {
		return controlCommand(args[0])
	}
	o := newRecOptions()
	fs := o.flagSet()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	path, explicit := o.config, o.config != ""
	if !explicit {
		path = configPath()
	}
	cfg, err := loadConfig(path, explicit)
	if err != nil {
		return err
	}
	if err := o.applyConfig(fs, cfg.record); err != nil {
		return err
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := o.normalize(); err != nil {
		return err
	}
	cfg.clipboard = cfg.clipboard && !o.noClipboard
	cfg.notify = cfg.notify && !o.noNotify

	if reply, err := controlSend("stop"); err == nil {
		fmt.Println(reply)
		return nil
	}

	outputs, err := swayOutputs()
	if err != nil {
		return err
	}
	t, err := o.resolveTarget(outputs)
	if err != nil {
		return err
	}
	if t == nil {
		cfg.mode = firstNonEmpty(o.mode, cfg.mode)
		a, err := pick(cfg, true)
		if err != nil {
			return err
		}
		if a.result == nil {
			return nil
		}
		if !a.video {
			return deliver(a.result, cfg)
		}
		return record(a, cfg, o, a.result.target())
	}
	a, err := connect()
	if err != nil {
		return err
	}
	a.cfg = cfg
	if _, _, err := a.matchOutputs(outputs); err != nil {
		return err
	}
	a.hide()
	return record(a, cfg, o, t)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type recTarget struct {
	label  string
	area   rect
	window *swayNode
	poly   []pt
}

func (r *result) target() *recTarget {
	o := r.s.info
	if r.full {
		return &recTarget{label: "output " + o.Name, area: o.Rect}
	}
	if r.win != nil {
		return &recTarget{label: windowLabel(r.win), area: r.win.Rect, window: r.win}
	}
	x0 := int(math.Floor(float64(r.r.X) / r.s.sx))
	y0 := int(math.Floor(float64(r.r.Y) / r.s.sy))
	x1 := int(math.Ceil(float64(r.r.X+r.r.W) / r.s.sx))
	y1 := int(math.Ceil(float64(r.r.Y+r.r.H) / r.s.sy))
	t := &recTarget{label: "region", area: rect{o.Rect.X + x0, o.Rect.Y + y0, x1 - x0, y1 - y0}}
	for _, p := range r.poly {
		t.poly = append(t.poly, pt{(p.X - float64(r.r.X)) / float64(r.r.W), (p.Y - float64(r.r.Y)) / float64(r.r.H)})
	}
	return t
}

func windowLabel(w *swayNode) string {
	return "window " + firstNonEmpty(w.AppID, w.Name)
}

func (o *recOptions) resolveTarget(outputs []swayOutput) (*recTarget, error) {
	picked := 0
	for _, set := range []bool{o.output != "", o.all, o.region != "", o.window != ""} {
		if set {
			picked++
		}
	}
	if picked > 1 {
		return nil, errors.New("use only one of -output, -all, -region and -window")
	}
	switch {
	case o.output != "":
		for _, out := range outputs {
			if out.Name == o.output {
				return &recTarget{label: "output " + out.Name, area: out.Rect}, nil
			}
		}
		return nil, fmt.Errorf("no active output named %q", o.output)
	case o.all:
		var area rect
		for _, out := range outputs {
			area = area.union(out.Rect)
		}
		return &recTarget{label: "all outputs", area: area}, nil
	case o.region != "":
		area, err := parseRegion(o.region)
		if err != nil {
			return nil, err
		}
		var covered rect
		for _, out := range outputs {
			covered = covered.union(out.Rect.intersect(area))
		}
		if covered.empty() {
			return nil, fmt.Errorf("region %q is outside every output", o.region)
		}
		return &recTarget{label: "region", area: covered}, nil
	case o.window != "":
		root, err := swayTree()
		if err != nil {
			return nil, err
		}
		w, err := findWindow(root, o.window)
		if err != nil {
			return nil, err
		}
		return &recTarget{label: windowLabel(w), area: w.Rect, window: w}, nil
	}
	return nil, nil
}

func parseRegion(s string) (rect, error) {
	var r rect
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d,%d %dx%d", &r.X, &r.Y, &r.W, &r.H); err != nil || r.empty() {
		return rect{}, fmt.Errorf(`region: expected "x,y wxh", got %q`, s)
	}
	return r, nil
}

func findWindow(root swayNode, criteria string) (*swayNode, error) {
	key, val, _ := strings.Cut(criteria, "=")
	var match func(n *swayNode) bool
	switch key {
	case "focused":
		match = func(n *swayNode) bool { return n.Focused }
	case "app_id", "title":
		re, err := regexp.Compile(val)
		if err != nil {
			return nil, fmt.Errorf("window: %w", err)
		}
		match = func(n *swayNode) bool {
			if key == "app_id" {
				return re.MatchString(n.AppID)
			}
			return re.MatchString(n.Name)
		}
	case "con_id", "pid":
		id, err := strconv.Atoi(val)
		if err != nil {
			return nil, fmt.Errorf("window: %s needs a number, got %q", key, val)
		}
		match = func(n *swayNode) bool {
			if key == "pid" {
				return n.Pid == id
			}
			return n.ID == id
		}
	default:
		return nil, fmt.Errorf("window: expected focused, app_id=regex, title=regex, con_id=N or pid=N, got %q", criteria)
	}
	var found *swayNode
	var walk func(n *swayNode)
	walk = func(n *swayNode) {
		if found != nil {
			return
		}
		if n.Pid != 0 && match(n) {
			found = n
			return
		}
		for i := range n.Nodes {
			walk(&n.Nodes[i])
		}
		for i := range n.FloatingNodes {
			walk(&n.FloatingNodes[i])
		}
	}
	walk(&root)
	if found == nil {
		return nil, fmt.Errorf("no window matches %q", criteria)
	}
	return found, nil
}

func recordingDir(o *recOptions) string {
	if o.dir != "" {
		return o.dir
	}
	return filepath.Join(userDir("VIDEOS", "Videos"), "Screencasts")
}
