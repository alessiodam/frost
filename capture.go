package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
)

type rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"width"`
	H int `json:"height"`
}

func (r rect) empty() bool            { return r.W <= 0 || r.H <= 0 }
func (r rect) contains(x, y int) bool { return x >= r.X && y >= r.Y && x < r.X+r.W && y < r.Y+r.H }

func (r rect) intersect(o rect) rect {
	x0, y0 := max(r.X, o.X), max(r.Y, o.Y)
	x1, y1 := min(r.X+r.W, o.X+o.W), min(r.Y+r.H, o.Y+o.H)
	if x1 <= x0 || y1 <= y0 {
		return rect{}
	}
	return rect{x0, y0, x1 - x0, y1 - y0}
}

func (r rect) union(o rect) rect {
	if r.empty() {
		return o
	}
	if o.empty() {
		return r
	}
	x0, y0 := min(r.X, o.X), min(r.Y, o.Y)
	x1, y1 := max(r.X+r.W, o.X+o.W), max(r.Y+r.H, o.Y+o.H)
	return rect{x0, y0, x1 - x0, y1 - y0}
}

func (r rect) inset(d int) rect { return rect{r.X - d, r.Y - d, r.W + 2*d, r.H + 2*d} }

type swayOutput struct {
	Name      string  `json:"name"`
	Active    bool    `json:"active"`
	Scale     float64 `json:"scale"`
	Transform string  `json:"transform"`
	Rect      rect    `json:"rect"`
}

func swayOutputs() ([]swayOutput, error) {
	out, err := exec.Command("swaymsg", "-r", "-t", "get_outputs").Output()
	if err != nil {
		return nil, fmt.Errorf("swaymsg get_outputs: %w", err)
	}
	var all []swayOutput
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("swaymsg get_outputs: %w", err)
	}
	var active []swayOutput
	for _, o := range all {
		if o.Active {
			active = append(active, o)
		}
	}
	return active, nil
}

type swayNode struct {
	Type           string     `json:"type"`
	Pid            int        `json:"pid"`
	Visible        bool       `json:"visible"`
	FullscreenMode int        `json:"fullscreen_mode"`
	Rect           rect       `json:"rect"`
	Nodes          []swayNode `json:"nodes"`
	FloatingNodes  []swayNode `json:"floating_nodes"`
}

func swayWindows() ([]rect, error) {
	out, err := exec.Command("swaymsg", "-r", "-t", "get_tree").Output()
	if err != nil {
		return nil, fmt.Errorf("swaymsg get_tree: %w", err)
	}
	var root swayNode
	if err := json.Unmarshal(out, &root); err != nil {
		return nil, fmt.Errorf("swaymsg get_tree: %w", err)
	}
	var full, floating, tiled []rect
	var walk func(n swayNode, isFloating bool)
	walk = func(n swayNode, isFloating bool) {
		if n.Pid != 0 && n.Visible {
			switch {
			case n.FullscreenMode != 0:
				full = append(full, n.Rect)
			case isFloating:
				floating = append(floating, n.Rect)
			default:
				tiled = append(tiled, n.Rect)
			}
		}
		for _, c := range n.Nodes {
			walk(c, isFloating)
		}
		for _, c := range n.FloatingNodes {
			walk(c, true)
		}
	}
	walk(root, false)
	return append(append(full, floating...), tiled...), nil
}

type frame struct {
	W, H int
	Pix  []byte
}

func grab(output string) (*frame, error) {
	cmd := exec.Command("grim", "-t", "ppm", "-o", output, "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("grim -o %s: %w: %s", output, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return decodePPM(raw)
}

func decodePPM(raw []byte) (*frame, error) {
	r := bufio.NewReader(bytes.NewReader(raw))
	var fields [4]int
	magic, err := ppmToken(r)
	if err != nil || magic != "P6" {
		return nil, fmt.Errorf("not a binary PPM")
	}
	for i := 1; i < 4; i++ {
		tok, err := ppmToken(r)
		if err != nil {
			return nil, fmt.Errorf("PPM header: %w", err)
		}
		if fields[i], err = strconv.Atoi(tok); err != nil {
			return nil, fmt.Errorf("PPM header: %w", err)
		}
	}
	w, h, maxval := fields[1], fields[2], fields[3]
	if maxval != 255 {
		return nil, fmt.Errorf("PPM maxval %d unsupported", maxval)
	}
	rgb := make([]byte, w*h*3)
	if _, err := io.ReadFull(r, rgb); err != nil {
		return nil, fmt.Errorf("PPM pixels: %w", err)
	}
	pix := make([]byte, w*h*4)
	for i, j := 0, 0; i < len(rgb); i, j = i+3, j+4 {
		pix[j], pix[j+1], pix[j+2], pix[j+3] = rgb[i+2], rgb[i+1], rgb[i], 0xff
	}
	return &frame{w, h, pix}, nil
}

func ppmToken(r *bufio.Reader) (string, error) {
	var tok []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		switch {
		case c == '#' && len(tok) == 0:
			if _, err := r.ReadString('\n'); err != nil {
				return "", err
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if len(tok) > 0 {
				return string(tok), nil
			}
		default:
			tok = append(tok, c)
		}
	}
}

func roundInt(f float64) int { return int(math.Round(f)) }
