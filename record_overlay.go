package main

import (
	"fmt"
	"image"
	"image/color"
	"time"

	"github.com/rajveermalviya/go-wayland/wayland/client"
	"github.com/rajveermalviya/go-wayland/wayland/stable/viewporter"
	"golang.org/x/image/font"

	"github.com/alessiodam/frost/protocol/layershell"
)

type layerBox struct {
	a          *app
	surf       *client.Surface
	layer      *layershell.ZwlrLayerSurfaceV1
	vp         *viewporter.Viewport
	bufs       [2]*shmBuffer
	busy       [2]bool
	bw, bh     int
	lw, lh     int
	img        *image.RGBA
	configured bool
	dirty      bool
	draw       func(img *image.RGBA)
}

func (a *app) newLayerBox(sc *screen, box rect, interactive bool, draw func(img *image.RGBA)) (*layerBox, error) {
	b := &layerBox{a: a, lw: box.W, lh: box.H, draw: draw}
	b.bw, b.bh = roundInt(float64(box.W)*sc.info.Scale), roundInt(float64(box.H)*sc.info.Scale)
	b.img = image.NewRGBA(image.Rect(0, 0, b.bw, b.bh))
	var err error
	if b.surf, err = a.compositor.CreateSurface(); err != nil {
		return nil, err
	}
	if b.vp, err = a.viewporter.GetViewport(b.surf); err != nil {
		return nil, err
	}
	b.vp.SetDestination(int32(box.W), int32(box.H))
	if !interactive {
		region, err := a.compositor.CreateRegion()
		if err != nil {
			return nil, err
		}
		b.surf.SetInputRegion(region)
		region.Destroy()
	}
	if b.layer, err = a.layerShell.GetLayerSurface(b.surf, sc.out, layerOverlay, "frost-recording"); err != nil {
		return nil, err
	}
	const anchorTopLeft = 1 | 4
	b.layer.SetAnchor(anchorTopLeft)
	b.layer.SetSize(uint32(box.W), uint32(box.H))
	b.layer.SetMargin(int32(box.Y), 0, 0, int32(box.X))
	b.layer.SetExclusiveZone(-1)
	b.layer.SetKeyboardInteractivity(0)
	b.layer.SetConfigureHandler(func(e layershell.ZwlrLayerSurfaceV1ConfigureEvent) {
		b.layer.AckConfigure(e.Serial)
		b.configured = true
		b.redraw()
	})
	for i := range b.bufs {
		if b.bufs[i], err = a.newShmBuffer(b.bw, b.bh, b.bw*4, fmtARGB8888); err != nil {
			return nil, err
		}
		b.bufs[i].wl.SetReleaseHandler(func(client.BufferReleaseEvent) {
			b.busy[i] = false
			if b.dirty {
				b.redraw()
			}
		})
	}
	return b, b.surf.Commit()
}

func (b *layerBox) redraw() {
	b.dirty = true
	if !b.configured || b.layer == nil {
		return
	}
	i := 0
	if b.busy[0] {
		i = 1
	}
	if b.busy[i] {
		return
	}
	b.dirty = false
	clear(b.img.Pix)
	b.draw(b.img)
	mem := b.bufs[i].mem
	for p := 0; p+4 <= len(mem); p += 4 {
		mem[p], mem[p+1], mem[p+2], mem[p+3] = b.img.Pix[p+2], b.img.Pix[p+1], b.img.Pix[p], b.img.Pix[p+3]
	}
	b.surf.Attach(b.bufs[i].wl, 0, 0)
	b.surf.DamageBuffer(0, 0, int32(b.bw), int32(b.bh))
	b.surf.Commit()
	b.busy[i] = true
}

func (b *layerBox) destroy() {
	if b.layer == nil {
		return
	}
	b.layer.Destroy()
	b.layer = nil
	b.vp.Destroy()
	b.surf.Destroy()
	for _, buf := range b.bufs {
		if buf != nil {
			buf.destroy()
		}
	}
}

const (
	badgeHeight  = 28
	badgeGap     = 12
	badgeButton  = 24
	outlineWidth = 2
)

func placeBadge(outputs []swayOutput, area rect, w, h, gap int) (int, rect, bool) {
	fits := func(o swayOutput, b rect) bool {
		return b.X >= o.Rect.X && b.Y >= o.Rect.Y && b.X+b.W <= o.Rect.X+o.Rect.W && b.Y+b.H <= o.Rect.Y+o.Rect.H &&
			b.intersect(area.inset(outlineWidth)).empty()
	}
	home := -1
	for i, o := range outputs {
		if !o.Rect.intersect(area).empty() {
			home = i
			break
		}
	}
	if !area.empty() && home >= 0 {
		o := outputs[home]
		cx := area.X + (area.W-w)/2
		clampX := func(x int) int { return max(o.Rect.X+gap, min(x, o.Rect.X+o.Rect.W-w-gap)) }
		cy := area.Y + (area.H-h)/2
		for _, b := range []rect{
			{clampX(cx), area.Y + area.H + gap, w, h},
			{clampX(cx), area.Y - h - gap, w, h},
			{area.X + area.W + gap, cy, w, h},
			{area.X - w - gap, cy, w, h},
		} {
			if fits(o, b) {
				return home, b, true
			}
		}
	}
	for i, o := range outputs {
		b := rect{o.Rect.X + (o.Rect.W-w)/2, o.Rect.Y + gap, w, h}
		if fits(o, b) || area.empty() {
			return i, b, true
		}
	}
	if len(outputs) == 0 {
		return -1, rect{}, false
	}
	i := max(home, 0)
	o := outputs[i]
	return i, rect{o.Rect.X + (o.Rect.W-w)/2, o.Rect.Y + gap, w, h}, false
}

type badge struct {
	r       *recorder
	sc      *screen
	face    font.Face
	scale   float64
	x, y    float64
	pauseAt rect
	stopAt  rect
	outside bool
}

func (r *recorder) setupIndicator() {
	if r.o.indicator == "off" {
		return
	}
	var outs []swayOutput
	for _, sc := range r.a.screens {
		outs = append(outs, sc.info)
	}
	area := r.target.area
	if r.target.window != nil {
		area = rect{}
	}
	scale := 1.0
	if len(r.a.screens) > 0 {
		scale = r.a.screens[0].info.Scale
	}
	face := loadFace(13 * scale)
	textW := textWidth(face, "PAUSED 00:00:00")
	width := 12 + 10 + 8 + int(float64(textW)/scale) + 8 + badgeButton + 4 + badgeButton + 6

	if r.target.window == nil {
		r.setupOutlines()
	}
	i, box, outside := placeBadge(outs, area, width, badgeHeight, badgeGap)
	if i < 0 || !outside && r.o.indicator != "on" && r.o.delay == 0 && r.target.window == nil {
		return
	}
	sc := r.a.screens[i]
	if sc.info.Scale != scale {
		scale = sc.info.Scale
		face = loadFace(13 * scale)
	}
	bd := &badge{r: r, sc: sc, face: face, scale: scale, outside: outside}
	local := rect{box.X - sc.info.Rect.X, box.Y - sc.info.Rect.Y, box.W, box.H}
	layer, err := r.a.newLayerBox(sc, local, true, bd.paint)
	if err != nil {
		return
	}
	r.badge = layer
	r.badgeInfo = bd
	r.attachPointer(bd)
}

func (r *recorder) showBadgeWhileRecording() bool {
	if r.badgeInfo == nil {
		return false
	}
	return r.badgeInfo.outside || r.o.indicator == "on" || r.target.window != nil
}

func (r *recorder) setupOutlines() {
	area := r.target.area
	for _, sc := range r.a.screens {
		o := sc.info.Rect
		part := o.intersect(area)
		if part.empty() {
			continue
		}
		box := part.inset(outlineWidth).intersect(o)
		if box == part {
			continue
		}
		local := rect{box.X - o.X, box.Y - o.Y, box.W, box.H}
		scale := sc.info.Scale
		hole := rect{
			roundInt(float64(part.X-box.X) * scale), roundInt(float64(part.Y-box.Y) * scale),
			roundInt(float64(part.W) * scale), roundInt(float64(part.H) * scale),
		}
		layer, err := r.a.newLayerBox(sc, local, false, func(img *image.RGBA) { paintOutline(img, hole) })
		if err == nil {
			r.outlines = append(r.outlines, layer)
		}
	}
}

func paintOutline(img *image.RGBA, hole rect) {
	b := img.Bounds()
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			if !hole.contains(x, y) {
				img.SetRGBA(x, y, colRec)
			}
		}
	}
}

func (r *recorder) attachPointer(bd *badge) {
	a := r.a
	get := func() {
		if a.pointer != nil || a.seatCaps&1 == 0 {
			return
		}
		a.pointer, _ = a.seat.GetPointer()
		a.pointer.SetEnterHandler(func(e client.PointerEnterEvent) {
			a.enterSerial, a.cursorName = e.Serial, ""
			a.setCursor("default")
			bd.x, bd.y = e.SurfaceX, e.SurfaceY
		})
		a.pointer.SetMotionHandler(func(e client.PointerMotionEvent) { bd.x, bd.y = e.SurfaceX, e.SurfaceY })
		a.pointer.SetButtonHandler(func(e client.PointerButtonEvent) {
			if e.Button == btnLeft && e.State == 1 {
				bd.click()
			}
		})
	}
	a.seat.SetCapabilitiesHandler(func(e client.SeatCapabilitiesEvent) {
		a.seatCaps = e.Capabilities
		get()
	})
	get()
}

func (bd *badge) click() {
	r := bd.r
	x, y := int(bd.x*bd.scale), int(bd.y*bd.scale)
	switch {
	case bd.stopAt.contains(x, y):
		if r.currentState() == stateCountdown {
			r.cancelled.Store(true)
		}
		r.requestStop(nil)
	case bd.pauseAt.contains(x, y):
		r.togglePause("toggle")
	}
}

func formatElapsed(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
}

func (bd *badge) paint(img *image.RGBA) {
	r := bd.r
	s := func(v float64) int { return roundInt(v * bd.scale) }
	b := img.Bounds()
	fillRoundRect(img, rect{0, 0, b.Dx(), b.Dy()}, float64(s(8)), max(1, bd.scale), color.RGBA{0, 0, 0, 0xee}, colLine)

	state := r.currentState()
	dot := colRec
	text := "REC " + formatElapsed(r.clock.elapsed())
	switch state {
	case statePaused:
		dot = r.cfg.accent
		text = "PAUSED " + formatElapsed(r.clock.elapsed())
	case stateCountdown:
		dot = r.cfg.accent
		text = fmt.Sprintf("Starting in %d", int(time.Until(r.countUntil).Seconds())+1)
	case stateFinalizing:
		text = "Saving"
	}
	cy := b.Dy() / 2
	fillRoundRect(img, rect{s(12), cy - s(5), s(10), s(10)}, float64(s(5)), 0, dot, dot)
	m := bd.face.Metrics()
	baseline := (b.Dy() + (m.Ascent - m.Descent).Ceil()) / 2
	drawText(img, bd.face, text, s(30), baseline, colBright)

	stop := rect{b.Dx() - s(6) - s(badgeButton), s(2), s(badgeButton), b.Dy() - s(4)}
	pause := rect{stop.X - s(4) - s(badgeButton), stop.Y, stop.W, stop.H}
	bd.stopAt, bd.pauseAt = stop, rect{}
	fillRoundRect(img, rect{stop.X + stop.W/2 - s(5), stop.Y + stop.H/2 - s(5), s(10), s(10)}, 1, 0, colBright, colBright)
	if state == stateRecording {
		bd.pauseAt = pause
		fillRoundRect(img, rect{pause.X + pause.W/2 - s(5), pause.Y + pause.H/2 - s(5), s(3), s(10)}, 0, 0, colBright, colBright)
		fillRoundRect(img, rect{pause.X + pause.W/2 + s(2), pause.Y + pause.H/2 - s(5), s(3), s(10)}, 0, 0, colBright, colBright)
	}
	if state == statePaused {
		bd.pauseAt = pause
		h := s(10)
		for y := 0; y < h; y++ {
			span := min(y, h-1-y) + 1
			for x := 0; x < span; x++ {
				img.SetRGBA(pause.X+pause.W/2-s(4)+x, pause.Y+pause.H/2-h/2+y, colBright)
			}
		}
	}
}

func (r *recorder) updateBadge() {
	if r.badge == nil || r.badgeInfo == nil {
		return
	}
	state := r.currentState()
	var key string
	if state == stateCountdown {
		key = fmt.Sprintf("%s %d", state, int(time.Until(r.countUntil).Seconds()))
	} else {
		key = fmt.Sprintf("%s %d", state, int(r.clock.elapsed().Seconds()))
	}
	if key != r.drawn {
		r.drawn = key
		r.badge.redraw()
	}
}

func (r *recorder) destroyBadge() {
	if r.badge != nil {
		r.badge.destroy()
		r.badge = nil
		r.badgeInfo = nil
	}
}

func (r *recorder) destroyIndicator() {
	if r.a.pointer != nil {
		r.a.pointer.Release()
		r.a.pointer = nil
	}
	if r.badge != nil {
		r.badge.destroy()
		r.badge = nil
		r.badgeInfo = nil
	}
	for _, o := range r.outlines {
		o.destroy()
	}
	r.outlines = nil
}
