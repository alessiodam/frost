package main

import (
	"bufio"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type config struct {
	mode      string
	dir       string
	save      bool
	clipboard bool
	notify    bool
	accent    color.RGBA
	keys      map[action][]uint32
}

var actionNames = map[string]action{
	"rect": actRect, "free": actFree, "window": actWindow, "screen": actScreen, "cancel": actCancel,
}

var defaultKeys = map[action]string{
	actRect:   "r 1",
	actFree:   "f 2",
	actWindow: "w 3",
	actScreen: "s 4 Return KP_Enter",
	actCancel: "Escape",
}

func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "frost", "config")
}

func loadConfig(path string, mustExist bool) (*config, error) {
	c := &config{mode: "last", save: true, clipboard: true, notify: true, accent: colAccent, keys: map[action][]uint32{}}
	for act, names := range defaultKeys {
		syms, err := parseKeys(names)
		if err != nil {
			return nil, err
		}
		c.keys[act] = syms
	}

	f, err := os.Open(path)
	if os.IsNotExist(err) && !mustExist {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	section := ""
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		fail := func(format string, args ...any) error {
			return fmt.Errorf("%s:%d: %s", path, n, fmt.Sprintf(format, args...))
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section != "general" && section != "keys" {
				return nil, fail("unknown section [%s]", section)
			}
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fail("expected key = value")
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)

		switch section {
		case "keys":
			act, ok := actionNames[key]
			if !ok {
				return nil, fail("unknown action %q (rect, free, window, screen, cancel)", key)
			}
			syms, err := parseKeys(val)
			if err != nil {
				return nil, fail("%v", err)
			}
			c.keys[act] = syms
		case "general":
			if err := c.setGeneral(key, val); err != nil {
				return nil, fail("%v", err)
			}
		default:
			return nil, fail("%q outside of a [general] or [keys] section", key)
		}
	}
	return c, sc.Err()
}

func (c *config) setGeneral(key, val string) error {
	boolean := func(dst *bool) error {
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("%s: expected true or false, got %q", key, val)
		}
		*dst = b
		return nil
	}
	switch key {
	case "mode":
		if _, ok := modeNames[val]; !ok && val != "last" {
			return fmt.Errorf("mode: expected rect, free, window or last, got %q", val)
		}
		c.mode = val
	case "directory":
		if strings.HasPrefix(val, "~/") {
			home, _ := os.UserHomeDir()
			val = filepath.Join(home, val[2:])
		}
		c.dir = os.ExpandEnv(val)
	case "save":
		return boolean(&c.save)
	case "clipboard":
		return boolean(&c.clipboard)
	case "notify":
		return boolean(&c.notify)
	case "accent":
		col, err := parseHex(val)
		if err != nil {
			return fmt.Errorf("accent: %v", err)
		}
		c.accent = col
	default:
		return fmt.Errorf("unknown option %q", key)
	}
	return nil
}

func parseKeys(list string) ([]uint32, error) {
	var syms []uint32
	for _, name := range strings.Fields(list) {
		sym := keysymFromName(name)
		if sym == 0 {
			return nil, fmt.Errorf("unknown key %q (use xkb names like r, Return, Escape, F1)", name)
		}
		syms = append(syms, keysymLower(sym))
	}
	return syms, nil
}

func parseHex(in string) (color.RGBA, error) {
	s := strings.TrimPrefix(in, "#")
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil || len(s) != 6 {
		return color.RGBA{}, fmt.Errorf("expected a colour like #e5b84a, got %q", in)
	}
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}, nil
}

func (c *config) actionFor(syms []uint32) action {
	for _, sym := range syms {
		sym = keysymLower(sym)
		for act, bound := range c.keys {
			for _, b := range bound {
				if b == sym {
					return act
				}
			}
		}
	}
	return actNone
}

func (c *config) hint(act action) string {
	if syms := c.keys[act]; len(syms) > 0 {
		return keysymLabel(syms[0])
	}
	return ""
}
