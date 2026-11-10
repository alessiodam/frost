package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var modeNames = map[string]action{"rect": actRect, "free": actFree, "window": actWindow}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "record" {
		if err := recordMain(os.Args[2:]); err != nil {
			fail(err)
		}
		return
	}
	cfgPath := flag.String("config", "", "config file (default: $XDG_CONFIG_HOME/frost/config)")
	mode := flag.String("mode", "", "initial mode: rect, free, window or last")
	dir := flag.String("dir", "", "save directory (default: $XDG_PICTURES_DIR/Screenshots)")
	noSave := flag.Bool("no-save", false, "don't save to disk")
	noClip := flag.Bool("no-clipboard", false, "don't copy to the clipboard")
	noNotify := flag.Bool("no-notify", false, "don't show a notification")
	flag.Parse()

	path, explicit := *cfgPath, *cfgPath != ""
	if !explicit {
		path = configPath()
	}
	cfg, err := loadConfig(path, explicit)
	if err == nil {
		if *mode != "" {
			if _, ok := modeNames[*mode]; !ok && *mode != "last" {
				err = fmt.Errorf("unknown mode %q", *mode)
			}
			cfg.mode = *mode
		}
		if *dir != "" {
			cfg.dir = *dir
		}
		cfg.save = cfg.save && !*noSave
		cfg.clipboard = cfg.clipboard && !*noClip
		cfg.notify = cfg.notify && !*noNotify
	}
	if err == nil {
		err = run(cfg)
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "frost:", err)
	exec.Command("notify-send", "-a", "frost", "-u", "critical", "frost", err.Error()).Run()
	os.Exit(1)
}

func run(cfg *config) error {
	o, err := configuredRecOptions(cfg)
	if err != nil {
		return err
	}
	if reply, err := controlSend("stop"); err == nil {
		fmt.Println(reply)
		return nil
	}
	a, err := pick(cfg, false)
	if err != nil {
		return err
	}
	if a.result == nil {
		return nil
	}
	if a.video {
		return record(a, cfg, o, a.result.target())
	}
	return deliver(a.result, cfg)
}

func (a *app) matchOutputs(outputs []swayOutput) (shown, direct []*screen, err error) {
	for _, s := range a.screens {
		for _, o := range outputs {
			if o.Name == s.name {
				s.info = o
				shown = append(shown, s)
				if o.Transform == "" || o.Transform == "normal" {
					direct = append(direct, s)
				}
			}
		}
	}
	if len(shown) == 0 {
		return nil, nil, fmt.Errorf("no outputs matched between sway and wayland")
	}
	a.screens = shown
	return shown, direct, nil
}

func pick(cfg *config, video bool) (*app, error) {
	colAccent = cfg.accent
	outputs, err := swayOutputs()
	if err != nil {
		return nil, err
	}
	var windows []swayNode
	var wg sync.WaitGroup
	wg.Go(func() { windows, _ = swayWindows() })

	a, err := connect()
	if err != nil {
		return nil, err
	}
	a.cfg = cfg
	a.setVideo(video)

	shown, direct, err := a.matchOutputs(outputs)
	if err != nil {
		return nil, err
	}

	if err := a.captureAll(direct); err != nil {
		return nil, err
	}
	for _, s := range shown {
		if s.cap == nil {
			if s.cap, err = grab(s.name); err != nil {
				return nil, err
			}
		}
	}

	wg.Wait()
	for _, s := range shown {
		o := s.info
		for _, w := range windows {
			local := rect{w.Rect.X - o.Rect.X, w.Rect.Y - o.Rect.Y, w.Rect.W, w.Rect.H}.
				intersect(rect{0, 0, o.Rect.W, o.Rect.H})
			if !local.empty() {
				s.winLogical = append(s.winLogical, local)
				s.winNodes = append(s.winNodes, w)
			}
		}
	}

	a.mode = loadMode()
	if m, ok := modeNames[cfg.mode]; ok {
		a.mode = m
	}

	if err := a.run(); err != nil {
		return nil, err
	}
	a.hide()
	saveMode(a.mode)
	return a, nil
}

func toBuffer(s *screen, r rect) rect {
	x0, y0 := roundInt(float64(r.X)*s.sx), roundInt(float64(r.Y)*s.sy)
	x1, y1 := roundInt(float64(r.X+r.W)*s.sx), roundInt(float64(r.Y+r.H)*s.sy)
	return rect{x0, y0, x1 - x0, y1 - y0}.intersect(rect{0, 0, s.bw, s.bh})
}

func stateFile() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "frost", "mode")
}

func loadMode() action {
	b, err := os.ReadFile(stateFile())
	if err != nil {
		return actRect
	}
	if m, ok := modeNames[strings.TrimSpace(string(b))]; ok {
		return m
	}
	return actRect
}

func saveMode(m action) {
	for name, v := range modeNames {
		if v == m {
			os.MkdirAll(filepath.Dir(stateFile()), 0o755)
			os.WriteFile(stateFile(), []byte(name+"\n"), 0o644)
		}
	}
}

func xcursorTheme() string {
	if t := os.Getenv("XCURSOR_THEME"); t != "" {
		return t
	}
	return "default"
}

func xcursorSize() int {
	if n, err := strconv.Atoi(os.Getenv("XCURSOR_SIZE")); err == nil && n > 0 {
		return n
	}
	return 24
}
