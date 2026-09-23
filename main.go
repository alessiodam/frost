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
		fmt.Fprintln(os.Stderr, "frost:", err)
		exec.Command("notify-send", "-a", "frost", "-u", "critical", "frost", err.Error()).Run()
		os.Exit(1)
	}
}

func run(cfg *config) error {
	colAccent = cfg.accent
	outputs, err := swayOutputs()
	if err != nil {
		return err
	}
	var windows []rect
	var wg sync.WaitGroup
	wg.Go(func() { windows, _ = swayWindows() })

	a, err := connect()
	if err != nil {
		return err
	}
	a.cfg = cfg

	var shown, direct []*screen
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
		return fmt.Errorf("no outputs matched between sway and wayland")
	}
	a.screens = shown

	if err := a.captureAll(direct); err != nil {
		return err
	}
	for _, s := range shown {
		if s.cap == nil {
			if s.cap, err = grab(s.name); err != nil {
				return err
			}
		}
	}

	wg.Wait()
	for _, s := range shown {
		o := s.info
		for _, w := range windows {
			local := rect{w.X - o.Rect.X, w.Y - o.Rect.Y, w.W, w.H}.
				intersect(rect{0, 0, o.Rect.W, o.Rect.H})
			if !local.empty() {
				s.winLogical = append(s.winLogical, local)
			}
		}
	}

	a.mode = loadMode()
	if m, ok := modeNames[cfg.mode]; ok {
		a.mode = m
	}

	if err := a.run(); err != nil {
		return err
	}
	a.hide()
	saveMode(a.mode)
	if a.result == nil {
		return nil
	}

	return deliver(a.result, cfg)
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
