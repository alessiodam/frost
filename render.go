package main

import (
	"image"
	"image/color"
	"math"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	colBg     = color.RGBA{0x00, 0x00, 0x00, 0xff}
	colHover  = color.RGBA{0x16, 0x16, 0x16, 0xff}
	colLine   = color.RGBA{0x2a, 0x2a, 0x2a, 0xff}
	colDim    = color.RGBA{0x5a, 0x5a, 0x5a, 0xff}
	colFg     = color.RGBA{0xa8, 0xa8, 0xa8, 0xff}
	colBright = color.RGBA{0xe6, 0xe6, 0xe6, 0xff}
	colAccent = color.RGBA{0xe5, 0xb8, 0x4a, 0xff}
)

const dimFactor = 0.45

type pt struct{ X, Y float64 }

func loadFace(px float64) font.Face {
	var data []byte
	if out, err := exec.Command("fc-match", "-f", "%{file}", "JetBrainsMono Nerd Font:medium").Output(); err == nil {
		data, _ = os.ReadFile(strings.TrimSpace(string(out)))
	}
	f, err := opentype.Parse(data)
	if err != nil {
		f, _ = opentype.Parse(gomono.TTF)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err)
	}
	return face
}

func textWidth(face font.Face, s string) int { return font.MeasureString(face, s).Ceil() }

func drawText(dst *image.RGBA, face font.Face, s string, x, baseline int, c color.Color) int {
	d := font.Drawer{Dst: dst, Src: image.NewUniform(c), Face: face, Dot: fixed.P(x, baseline)}
	d.DrawString(s)
	return d.Dot.X.Ceil()
}

func fillRoundRect(dst *image.RGBA, r rect, radius, bw float64, fill, border color.RGBA) {
	x0, y0 := float64(r.X), float64(r.Y)
	x1, y1 := x0+float64(r.W), y0+float64(r.H)
	for y := r.Y; y < r.Y+r.H; y++ {
		for x := r.X; x < r.X+r.W; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5

			qx := math.Max(math.Max(x0+radius-px, px-(x1-radius)), 0)
			qy := math.Max(math.Max(y0+radius-py, py-(y1-radius)), 0)
			var d float64
			if qx > 0 || qy > 0 {
				d = math.Hypot(qx, qy) - radius
			} else {
				d = -math.Min(math.Min(px-x0, x1-px), math.Min(py-y0, y1-py))
			}
			cov := clamp01(0.5 - d)
			if cov == 0 {
				continue
			}
			c := fill
			if bw > 0 {
				c = mix(border, fill, clamp01(-d-bw+0.5))
			}
			blendPixel(dst, x, y, c, cov)
		}
	}
}

func blendPixel(dst *image.RGBA, x, y int, c color.RGBA, cov float64) {
	i := dst.PixOffset(x, y)
	a := float64(c.A) / 255 * cov
	p := dst.Pix[i : i+4 : i+4]
	p[0] = uint8(float64(c.R)*a + float64(p[0])*(1-a))
	p[1] = uint8(float64(c.G)*a + float64(p[1])*(1-a))
	p[2] = uint8(float64(c.B)*a + float64(p[2])*(1-a))
	p[3] = uint8(255*a + float64(p[3])*(1-a))
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	l := func(x, y uint8) uint8 { return uint8(float64(x)*(1-t) + float64(y)*t) }
	return color.RGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), l(a.A, b.A)}
}

func clamp01(f float64) float64 { return math.Max(0, math.Min(1, f)) }

type action int

const (
	actRect action = iota
	actFree
	actWindow
	actScreen
	actCancel
	actNone = action(-1)
)

type tbButton struct {
	act         action
	icon, label string
	key         string
	r           rect
}

type toolbar struct {
	face    font.Face
	scale   float64
	buttons []tbButton
	w, h    int
	cache   map[[2]action]*image.RGBA
}

func newToolbar(face font.Face, scale float64, cfg *config) *toolbar {
	s := func(v float64) int { return roundInt(v * scale) }
	t := &toolbar{face: face, scale: scale, cache: map[[2]action]*image.RGBA{}}
	defs := []tbButton{
		{act: actRect, icon: "\U000F0489", label: "Rectangle"},
		{act: actFree, icon: "\U000F0F03", label: "Freeform"},
		{act: actWindow, icon: "\U000F05B6", label: "Window"},
		{act: actScreen, icon: "\U000F0379", label: "Screen"},
		{act: actCancel, icon: "\U000F0156"},
	}
	for i := range defs {
		defs[i].key = cfg.hint(defs[i].act)
	}
	pad, bh, gap, sep := s(5), s(30), s(2), s(9)
	x := pad
	for i, b := range defs {
		if b.act == actCancel {
			x += sep
		}
		w := s(12) + textWidth(face, b.icon)
		if b.label != "" {
			w += s(8) + textWidth(face, b.label)
		}
		if b.act != actCancel && b.key != "" {
			w += s(8) + textWidth(face, b.key)
		}
		w += s(12)
		b.r = rect{x, pad, w, bh}
		defs[i] = b
		x += w + gap
	}
	t.buttons = defs
	t.w, t.h = x-gap+pad, bh+2*pad
	return t
}

func (t *toolbar) hit(x, y int) action {
	for _, b := range t.buttons {
		if b.r.contains(x, y) {
			return b.act
		}
	}
	return actNone
}

func (t *toolbar) image(active, hover action) *image.RGBA {
	key := [2]action{active, hover}
	if img, ok := t.cache[key]; ok {
		return img
	}
	s := func(v float64) int { return roundInt(v * t.scale) }
	img := image.NewRGBA(image.Rect(0, 0, t.w, t.h))
	fillRoundRect(img, rect{0, 0, t.w, t.h}, float64(s(9)), math.Max(1, t.scale), colBg, colLine)

	m := t.face.Metrics()
	for i, b := range t.buttons {
		if b.act == actCancel && i > 0 {
			prev := t.buttons[i-1].r
			sx := (prev.X + prev.W + b.r.X) / 2
			for y := b.r.Y + s(7); y < b.r.Y+b.r.H-s(7); y++ {
				img.SetRGBA(sx, y, colLine)
			}
		}
		fg, keyCol := colFg, colDim
		if b.act == hover {
			fillRoundRect(img, b.r, float64(s(6)), 0, colHover, colHover)
			fg = colBright
		}
		if b.act == active {
			fg = colAccent
			ul := rect{b.r.X + s(10), b.r.Y + b.r.H - s(2), b.r.W - s(20), s(2)}
			fillRoundRect(img, ul, float64(s(1)), 0, colAccent, colAccent)
		}
		if b.act == actCancel && b.act == hover {
			fg = color.RGBA{0xd0, 0x70, 0x88, 0xff}
		}
		baseline := b.r.Y + (b.r.H+(m.Ascent-m.Descent).Ceil())/2
		x := b.r.X + s(12)
		x = drawText(img, t.face, b.icon, x, baseline, fg)
		if b.label != "" {
			x = drawText(img, t.face, b.label, x+s(8), baseline, fg)
		}
		if b.act != actCancel && b.key != "" {
			drawText(img, t.face, b.key, x+s(8), baseline, keyCol)
		}
	}
	t.cache[key] = img
	return img
}

func labelImage(face font.Face, scale float64, text string) *image.RGBA {
	s := func(v float64) int { return roundInt(v * scale) }
	m := face.Metrics()
	w := textWidth(face, text) + 2*s(8)
	h := (m.Ascent + m.Descent).Ceil() + 2*s(4)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fillRoundRect(img, rect{0, 0, w, h}, float64(s(5)), 0, color.RGBA{0, 0, 0, 0xe0}, colBg)
	drawText(img, face, text, s(8), s(4)+m.Ascent.Ceil(), colBright)
	return img
}

type scene struct {
	dim     bool
	hole    rect
	outline bool
	path    []pt
	label   *image.RGBA
	labelAt image.Point
	tb      *image.RGBA
	tbAt    image.Point
}

func (sc *scene) extent(t int) rect {
	var e rect
	if !sc.hole.empty() {
		e = e.union(sc.hole.inset(t))
	}
	if len(sc.path) > 0 {
		x0, y0, x1, y1 := sc.path[0].X, sc.path[0].Y, sc.path[0].X, sc.path[0].Y
		for _, p := range sc.path {
			x0, y0 = math.Min(x0, p.X), math.Min(y0, p.Y)
			x1, y1 = math.Max(x1, p.X), math.Max(y1, p.Y)
		}
		e = e.union(rect{int(x0), int(y0), int(x1-x0) + 1, int(y1-y0) + 1}.inset(t + 1))
	}
	if sc.label != nil {
		b := sc.label.Bounds()
		e = e.union(rect{sc.labelAt.X, sc.labelAt.Y, b.Dx(), b.Dy()})
	}
	if sc.tb != nil {
		b := sc.tb.Bounds()
		e = e.union(rect{sc.tbAt.X, sc.tbAt.Y, b.Dx(), b.Dy()})
	}
	return e
}

type canvas struct {
	pix  []byte
	w, h int
}

func (c canvas) copyFrom(src []byte, r rect) {
	for y := r.Y; y < r.Y+r.H; y++ {
		o := (y*c.w + r.X) * 4
		copy(c.pix[o:o+r.W*4], src[o:o+r.W*4])
	}
}

func (c canvas) fill(r rect, col color.RGBA) {
	for y := r.Y; y < r.Y+r.H; y++ {
		row := c.pix[(y*c.w+r.X)*4 : (y*c.w+r.X+r.W)*4]
		for i := 0; i < len(row); i += 4 {
			row[i], row[i+1], row[i+2], row[i+3] = col.B, col.G, col.R, 0xff
		}
	}
}

func (c canvas) blit(img *image.RGBA, at image.Point, clip rect) {
	b := img.Bounds()
	r := rect{at.X, at.Y, b.Dx(), b.Dy()}.intersect(clip)
	for y := r.Y; y < r.Y+r.H; y++ {
		for x := r.X; x < r.X+r.W; x++ {
			s := img.Pix[img.PixOffset(x-at.X, y-at.Y):]
			a := uint32(s[3])
			if a == 0 {
				continue
			}
			d := c.pix[(y*c.w+x)*4:]
			inv := 255 - a
			d[0] = uint8(uint32(s[2]) + uint32(d[0])*inv/255)
			d[1] = uint8(uint32(s[1]) + uint32(d[1])*inv/255)
			d[2] = uint8(uint32(s[0]) + uint32(d[2])*inv/255)
		}
	}
}

func (c canvas) stroke(path []pt, t int, col color.RGBA, clip rect) {
	stamp := func(x, y float64) {
		r := rect{int(x) - t/2, int(y) - t/2, t, t}.intersect(clip)
		if !r.empty() {
			c.fill(r, col)
		}
	}
	for i := range path {
		if i == 0 {
			stamp(path[0].X, path[0].Y)
			continue
		}
		a, b := path[i-1], path[i]
		n := int(math.Ceil(math.Hypot(b.X-a.X, b.Y-a.Y) * 2))
		for k := 1; k <= n; k++ {
			f := float64(k) / float64(n)
			stamp(a.X+(b.X-a.X)*f, a.Y+(b.Y-a.Y)*f)
		}
	}
}

func (c canvas) strokeRect(r rect, t int, col color.RGBA, clip rect) {
	o := r.inset(t)
	for _, side := range []rect{
		{o.X, o.Y, o.W, t}, {o.X, r.Y + r.H, o.W, t},
		{o.X, r.Y, t, r.H}, {r.X + r.W, r.Y, t, r.H},
	} {
		if s := side.intersect(clip); !s.empty() {
			c.fill(s, col)
		}
	}
}

func dimmed(src []byte) []byte {
	var lut [256]byte
	for i := range lut {
		lut[i] = byte(float64(i) * dimFactor)
	}
	dst := make([]byte, len(src))
	for i := 0; i < len(src); i += 4 {
		dst[i], dst[i+1], dst[i+2], dst[i+3] = lut[src[i]], lut[src[i+1]], lut[src[i+2]], 0xff
	}
	return dst
}
