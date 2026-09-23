package main

import (
	"errors"
	"fmt"
	"image"
	"math"
	"strings"

	"github.com/rajveermalviya/go-wayland/wayland/client"
	"github.com/rajveermalviya/go-wayland/wayland/cursor"
	"github.com/rajveermalviya/go-wayland/wayland/stable/viewporter"
	"golang.org/x/image/font"
	"golang.org/x/sys/unix"

	"github.com/alessiodam/frost/protocol/layershell"
	"github.com/alessiodam/frost/protocol/screencopy"
)

const (
	btnLeft  = 0x110
	btnRight = 0x111
)

type buffer struct {
	wl      *client.Buffer
	pix     []byte
	busy    bool
	drawn   bool
	lastDim bool
	lastExt rect
}

type screen struct {
	app  *app
	out  *client.Output
	name string
	info swayOutput

	surf  *client.Surface
	layer *layershell.ZwlrLayerSurfaceV1
	vp    *viewporter.Viewport

	lw, lh int
	bw, bh int
	sx, sy float64
	cap    *frame
	base   []byte
	dim    []byte
	bufs   []*buffer

	configured, framePending, dirty bool

	face       font.Face
	tb         *toolbar
	tbAt       image.Point
	hover      action
	winLogical []rect
	windows    []rect
	label      struct {
		text string
		img  *image.RGBA
	}
}

type result struct {
	s    *screen
	r    rect
	poly []pt
	full bool
}

type app struct {
	display    *client.Display
	ctx        *client.Context
	compositor *client.Compositor
	shm        *client.Shm
	seat       *client.Seat
	layerShell *layershell.ZwlrLayerShellV1
	viewporter *viewporter.Viewporter
	screencopy *screencopy.ZwlrScreencopyManagerV1
	pointer    *client.Pointer
	keyboard   *client.Keyboard
	xkb        *xkb
	cfg        *config

	screens []*screen

	cursorTheme *cursor.Theme
	cursorSurf  *client.Surface
	cursorScale int
	cursorName  string
	enterSerial uint32

	cur    *screen
	pos    pt
	mode   action
	hoverW int

	dragging  bool
	dragFrom  *screen
	dragStart pt
	path      []pt

	result *result
	quit   bool
	err    error
}

func connect() (*app, error) {
	display, err := client.Connect("")
	if err != nil {
		return nil, fmt.Errorf("connect to wayland: %w", err)
	}
	a := &app{display: display, ctx: display.Context(), hoverW: -1, xkb: newXKB()}
	display.SetErrorHandler(func(e client.DisplayErrorEvent) {
		a.err = fmt.Errorf("wayland error: object %d code %d: %s", e.ObjectId.ID(), e.Code, e.Message)
		a.quit = true
	})

	reg, err := display.GetRegistry()
	if err != nil {
		return nil, err
	}
	var outputs []*client.Output
	reg.SetGlobalHandler(func(e client.RegistryGlobalEvent) {
		bind := func(p client.Proxy, v uint32) {
			if err := reg.Bind(e.Name, e.Interface, min(e.Version, v), p); err != nil {
				a.err = err
			}
		}
		switch e.Interface {
		case "wl_compositor":
			a.compositor = client.NewCompositor(a.ctx)
			bind(a.compositor, 4)
		case "wl_shm":
			a.shm = client.NewShm(a.ctx)
			bind(a.shm, 1)
		case "wl_seat":
			if a.seat == nil {
				a.seat = client.NewSeat(a.ctx)
				bind(a.seat, 5)
			}
		case "wl_output":
			o := client.NewOutput(a.ctx)
			bind(o, 4)
			outputs = append(outputs, o)
		case "zwlr_layer_shell_v1":
			a.layerShell = layershell.NewZwlrLayerShellV1(a.ctx)
			bind(a.layerShell, 4)
		case "zwlr_screencopy_manager_v1":
			a.screencopy = screencopy.NewZwlrScreencopyManagerV1(a.ctx)
			bind(a.screencopy, 3)
		case "wp_viewporter":
			a.viewporter = viewporter.NewViewporter(a.ctx)
			bind(a.viewporter, 1)
		}
	})
	if err := a.roundtrip(); err != nil {
		return nil, err
	}
	switch {
	case a.compositor == nil || a.shm == nil:
		return nil, errors.New("compositor lacks wl_compositor/wl_shm")
	case a.layerShell == nil:
		return nil, errors.New("compositor lacks wlr-layer-shell")
	case a.viewporter == nil:
		return nil, errors.New("compositor lacks wp_viewporter")
	}

	for _, o := range outputs {
		s := &screen{app: a, out: o, hover: actNone}
		o.SetNameHandler(func(e client.OutputNameEvent) { s.name = e.Name })
		a.screens = append(a.screens, s)
	}
	if a.seat != nil {
		a.seat.SetCapabilitiesHandler(a.onSeatCaps)
	}

	if err := a.roundtrip(); err != nil {
		return nil, err
	}
	return a, a.err
}

func (a *app) roundtrip() error {
	cb, err := a.display.Sync()
	if err != nil {
		return err
	}
	done := false
	cb.SetDoneHandler(func(client.CallbackDoneEvent) { done = true })
	for !done {
		if err := a.dispatch(); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) dispatch() error {
	err := a.ctx.Dispatch()
	if err != nil && strings.Contains(err.Error(), "unable to read msg") {
		return err
	}
	return nil
}

func (a *app) run() error {
	for _, s := range a.screens {
		if err := s.show(); err != nil {
			return err
		}
	}
	for !a.quit {
		if err := a.dispatch(); err != nil {
			return err
		}
	}
	return a.err
}

func (a *app) hide() {
	if a.pointer != nil {
		a.pointer.Release()
	}
	if a.keyboard != nil {
		a.keyboard.Release()
	}
	for _, s := range a.screens {
		if s.layer != nil {
			s.layer.Destroy()
			s.surf.Destroy()
		}
	}
	a.roundtrip()
}

func (s *screen) show() error {
	a := s.app
	var err error
	if s.surf, err = a.compositor.CreateSurface(); err != nil {
		return err
	}
	if s.vp, err = a.viewporter.GetViewport(s.surf); err != nil {
		return err
	}
	const layerOverlay = 3
	if s.layer, err = a.layerShell.GetLayerSurface(s.surf, s.out, layerOverlay, "frost"); err != nil {
		return err
	}
	const anchorAll = 1 | 2 | 4 | 8
	s.layer.SetAnchor(anchorAll)
	s.layer.SetExclusiveZone(-1)
	s.layer.SetKeyboardInteractivity(1)
	s.layer.SetConfigureHandler(s.onConfigure)
	s.layer.SetClosedHandler(func(layershell.ZwlrLayerSurfaceV1ClosedEvent) { a.quit = true })
	return s.surf.Commit()
}

func (s *screen) onConfigure(e layershell.ZwlrLayerSurfaceV1ConfigureEvent) {
	s.layer.AckConfigure(e.Serial)
	if s.configured {
		s.redraw()
		return
	}
	s.lw, s.lh = int(e.Width), int(e.Height)

	s.bw = min(roundInt(float64(s.lw)*s.info.Scale), s.cap.W)
	s.bh = min(roundInt(float64(s.lh)*s.info.Scale), s.cap.H)
	s.sx, s.sy = float64(s.bw)/float64(s.lw), float64(s.bh)/float64(s.lh)

	s.base = make([]byte, s.bw*s.bh*4)
	for y := 0; y < s.bh; y++ {
		copy(s.base[y*s.bw*4:(y+1)*s.bw*4], s.cap.Pix[y*s.cap.W*4:])
	}
	s.dim = dimmed(s.base)
	for _, w := range s.winLogical {
		s.windows = append(s.windows, toBuffer(s, w))
	}

	s.face = loadFace(13 * s.info.Scale)
	s.tb = newToolbar(s.face, s.info.Scale, s.app.cfg)

	s.tbAt = image.Pt((s.bw-s.tb.w)/2, roundInt(48*s.info.Scale))

	if err := s.allocBuffers(2); err != nil {
		s.app.err, s.app.quit = err, true
		return
	}
	s.vp.SetDestination(int32(s.lw), int32(s.lh))
	s.configured = true
	s.redraw()
}

func (s *screen) allocBuffers(n int) error {
	size := s.bw * s.bh * 4
	fd, err := unix.MemfdCreate("frost", unix.MFD_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, int64(size*n)); err != nil {
		return err
	}
	mem, err := unix.Mmap(fd, 0, size*n, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return err
	}
	pool, err := s.app.shm.CreatePool(fd, int32(size*n))
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		wb, err := pool.CreateBuffer(int32(i*size), int32(s.bw), int32(s.bh), int32(s.bw*4), uint32(client.ShmFormatXrgb8888))
		if err != nil {
			return err
		}
		b := &buffer{wl: wb, pix: mem[i*size : (i+1)*size]}
		wb.SetReleaseHandler(func(client.BufferReleaseEvent) {
			b.busy = false
			if s.dirty && !s.framePending {
				s.draw()
			}
		})
		s.bufs = append(s.bufs, b)
	}
	return nil
}

func (s *screen) redraw() {
	s.dirty = true
	if s.configured && !s.framePending {
		s.draw()
	}
}

func (s *screen) draw() {
	var b *buffer
	for _, c := range s.bufs {
		if !c.busy {
			b = c
			break
		}
	}
	if b == nil {
		return
	}
	s.dirty = false

	sc := s.app.sceneFor(s)
	t := s.lineWidth()
	ext := sc.extent(t)
	full := rect{0, 0, s.bw, s.bh}
	clip := full
	if b.drawn && b.lastDim == sc.dim {
		clip = b.lastExt.union(ext).intersect(full)
	}
	s.paint(b, &sc, clip, t)
	b.drawn, b.lastDim, b.lastExt = true, sc.dim, ext

	cb, err := s.surf.Frame()
	if err == nil {
		s.framePending = true
		cb.SetDoneHandler(func(client.CallbackDoneEvent) {
			s.framePending = false
			if s.dirty {
				s.draw()
			}
		})
	}
	s.surf.Attach(b.wl, 0, 0)
	if !clip.empty() {
		s.surf.DamageBuffer(int32(clip.X), int32(clip.Y), int32(clip.W), int32(clip.H))
	}
	s.surf.Commit()
	b.busy = true
}

func (s *screen) lineWidth() int { return max(2, roundInt(1.5*s.info.Scale)) }

func (s *screen) paint(b *buffer, sc *scene, clip rect, t int) {
	if clip.empty() {
		return
	}
	c := canvas{b.pix, s.bw, s.bh}
	if sc.dim {
		c.copyFrom(s.dim, clip)
	} else {
		c.copyFrom(s.base, clip)
	}
	if h := sc.hole.intersect(clip); !h.empty() {
		c.copyFrom(s.base, h)
	}
	if sc.outline && !sc.hole.empty() {
		c.strokeRect(sc.hole, t, colAccent, clip)
	}
	if len(sc.path) > 0 {
		c.stroke(sc.path, t, colAccent, clip)
	}
	if sc.label != nil {
		c.blit(sc.label, sc.labelAt, clip)
	}
	if sc.tb != nil {
		c.blit(sc.tb, sc.tbAt, clip)
	}

	bw := max(2, roundInt(2*s.info.Scale))
	c.strokeRect(rect{bw, bw, s.bw - 2*bw, s.bh - 2*bw}, bw, colAccent, clip)
}

func (s *screen) sizeLabel(r rect) (*image.RGBA, image.Point) {
	text := fmt.Sprintf("%d × %d", r.W, r.H)
	if s.label.text != text {
		s.label.text, s.label.img = text, labelImage(s.face, s.info.Scale, text)
	}
	img := s.label.img
	gap := roundInt(6 * s.info.Scale)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	x, y := r.X+r.W-w, r.Y+r.H+gap+s.lineWidth()
	if y+h > s.bh-gap {
		y = r.Y + r.H - h - gap
		x -= gap
	}
	return img, image.Pt(max(gap, x), max(gap, y))
}

func (a *app) sceneFor(s *screen) scene {
	var sc scene
	switch {
	case a.dragging && s != a.dragFrom:
		sc.dim = true
	case a.dragging && a.mode == actRect:
		sc.dim, sc.outline = true, true
		sc.hole = a.dragRect()
		if !sc.hole.empty() {
			sc.label, sc.labelAt = s.sizeLabel(sc.hole)
		}
	case a.dragging && a.mode == actFree:
		sc.dim = true
		sc.path = a.path
	case a.mode == actWindow && s == a.cur && a.hoverW >= 0 && s.hover == actNone:
		sc.dim, sc.outline = true, true
		sc.hole = s.windows[a.hoverW]
		sc.label, sc.labelAt = s.sizeLabel(sc.hole)
	}
	if !a.dragging {
		sc.tb, sc.tbAt = s.tb.image(a.mode, s.hover), s.tbAt
	}
	return sc
}

func (a *app) dragRect() rect {
	s := a.dragFrom
	x0, y0 := math.Min(a.dragStart.X, a.pos.X), math.Min(a.dragStart.Y, a.pos.Y)
	x1, y1 := math.Max(a.dragStart.X, a.pos.X), math.Max(a.dragStart.Y, a.pos.Y)
	r := rect{int(math.Floor(x0)), int(math.Floor(y0)), 0, 0}
	r.W, r.H = int(math.Ceil(x1))-r.X, int(math.Ceil(y1))-r.Y
	return r.intersect(rect{0, 0, s.bw, s.bh})
}

func (a *app) redrawAll() {
	for _, s := range a.screens {
		s.redraw()
	}
}

func (a *app) onSeatCaps(e client.SeatCapabilitiesEvent) {
	const capPointer, capKeyboard = 1, 2
	if e.Capabilities&capPointer != 0 && a.pointer == nil {
		a.pointer, _ = a.seat.GetPointer()
		a.pointer.SetEnterHandler(a.onEnter)
		a.pointer.SetLeaveHandler(func(client.PointerLeaveEvent) {
			if a.cur != nil && !a.dragging {
				a.cur.hover = actNone
				a.cur.redraw()
			}
		})
		a.pointer.SetMotionHandler(func(e client.PointerMotionEvent) { a.onMotion(e.SurfaceX, e.SurfaceY) })
		a.pointer.SetButtonHandler(a.onButton)
	}
	if e.Capabilities&capKeyboard != 0 && a.keyboard == nil {
		a.keyboard, _ = a.seat.GetKeyboard()
		a.keyboard.SetKeymapHandler(func(e client.KeyboardKeymapEvent) {
			if err := a.xkb.setKeymap(e.Fd, e.Size); err != nil {
				a.err, a.quit = err, true
			}
		})
		a.keyboard.SetModifiersHandler(func(e client.KeyboardModifiersEvent) {
			a.xkb.updateMods(e.ModsDepressed, e.ModsLatched, e.ModsLocked, e.Group)
		})
		a.keyboard.SetKeyHandler(a.onKey)
	}
}

func (a *app) screenFor(surf *client.Surface) *screen {
	for _, s := range a.screens {
		if s.surf == surf {
			return s
		}
	}
	return nil
}

func (a *app) onEnter(e client.PointerEnterEvent) {
	a.enterSerial = e.Serial
	a.cursorName = ""
	if s := a.screenFor(e.Surface); s != nil && !a.dragging {
		a.cur = s
	}
	a.onMotion(e.SurfaceX, e.SurfaceY)
}

func (a *app) onMotion(x, y float64) {
	s := a.cur
	if s == nil || !s.configured {
		return
	}
	a.pos = pt{math.Max(0, math.Min(x*s.sx, float64(s.bw))), math.Max(0, math.Min(y*s.sy, float64(s.bh)))}

	if a.dragging {
		switch a.mode {
		case actRect:
			s.redraw()
		case actFree:
			last := a.path[len(a.path)-1]
			if math.Hypot(a.pos.X-last.X, a.pos.Y-last.Y) >= 1 {
				a.path = append(a.path, a.pos)
				s.redraw()
			}
		}
		return
	}

	hover := s.tb.hit(int(a.pos.X)-s.tbAt.X, int(a.pos.Y)-s.tbAt.Y)
	if hover != s.hover {
		s.hover = hover
		s.redraw()
	}
	if a.mode == actWindow {
		w := -1
		for i, r := range s.windows {
			if r.contains(int(a.pos.X), int(a.pos.Y)) {
				w = i
				break
			}
		}
		if w != a.hoverW {
			a.hoverW = w
			s.redraw()
		}
	}
	a.updateCursor()
}

func (a *app) onButton(e client.PointerButtonEvent) {
	s := a.cur
	if s == nil || !s.configured {
		return
	}
	pressed := e.State == 1
	switch {
	case e.Button == btnRight && pressed:
		if a.dragging {
			a.cancelDrag()
		} else {
			a.quit = true
		}
	case e.Button != btnLeft:
	case pressed && !a.dragging && s.hover != actNone:
		a.do(s.hover)
	case pressed && !a.dragging:
		switch a.mode {
		case actRect, actFree:
			a.dragging, a.dragFrom, a.dragStart = true, s, a.pos
			a.path = []pt{a.pos}
			a.redrawAll()
		case actWindow:
			if a.hoverW >= 0 {
				a.finish(&result{s: s, r: s.windows[a.hoverW]})
			}
		}
	case !pressed && a.dragging:
		a.endDrag()
	}
}

func (a *app) endDrag() {
	s := a.dragFrom
	switch a.mode {
	case actRect:
		if r := a.dragRect(); r.W >= 3 && r.H >= 3 {
			a.finish(&result{s: s, r: r})
			return
		}
	case actFree:
		if bb := polyBounds(a.path).intersect(rect{0, 0, s.bw, s.bh}); len(a.path) >= 3 && bb.W >= 3 && bb.H >= 3 {
			a.finish(&result{s: s, r: bb, poly: a.path})
			return
		}
	}
	a.cancelDrag()
}

func (a *app) cancelDrag() {
	a.dragging, a.dragFrom, a.path = false, nil, nil
	a.redrawAll()
}

func (a *app) onKey(e client.KeyboardKeyEvent) {
	if e.State != 1 {
		return
	}
	switch act := a.cfg.actionFor(a.xkb.syms(e.Key)); {
	case act == actCancel && a.dragging:
		a.cancelDrag()
	case act != actNone:
		a.do(act)
	}
}

func (a *app) do(act action) {
	switch act {
	case actCancel:
		a.quit = true
	case actScreen:
		s := a.cur
		if s == nil {
			s = a.screens[0]
		}
		a.finish(&result{s: s, full: true})
	default:
		if a.dragging {
			a.cancelDrag()
		}
		a.mode = act
		a.hoverW = -1
		if act == actWindow && a.cur != nil {
			a.onMotion(a.pos.X/a.cur.sx, a.pos.Y/a.cur.sy)
		}
		a.updateCursor()
		a.redrawAll()
	}
}

func (a *app) finish(r *result) {
	a.result, a.quit = r, true
}

func (a *app) updateCursor() {
	if a.pointer == nil {
		return
	}
	name := "crosshair"
	if a.cur != nil && a.cur.hover != actNone || a.mode == actWindow {
		name = "default"
	}
	if name == a.cursorName {
		return
	}
	if a.cursorTheme == nil {
		scale := 1
		for _, s := range a.screens {
			scale = max(scale, int(math.Ceil(s.info.Scale)))
		}
		theme, err := cursor.LoadTheme(xcursorTheme(), xcursorSize()*scale, a.shm)
		if err != nil {
			return
		}
		a.cursorTheme, a.cursorScale = theme, scale
		a.cursorSurf, _ = a.compositor.CreateSurface()
	}
	c := a.cursorTheme.GetCursor(name)
	if c == nil || len(c.Images) == 0 {
		return
	}
	img := &c.Images[0]
	buf, err := img.GetBuffer()
	if err != nil {
		return
	}
	a.cursorSurf.Attach(buf, 0, 0)
	a.cursorSurf.SetBufferScale(int32(a.cursorScale))
	a.cursorSurf.DamageBuffer(0, 0, int32(img.Width), int32(img.Height))
	a.cursorSurf.Commit()
	a.pointer.SetCursor(a.enterSerial, a.cursorSurf,
		int32(img.HotspotX)/int32(a.cursorScale), int32(img.HotspotY)/int32(a.cursorScale))
	a.cursorName = name
}

func polyBounds(p []pt) rect {
	x0, y0, x1, y1 := p[0].X, p[0].Y, p[0].X, p[0].Y
	for _, q := range p {
		x0, y0 = math.Min(x0, q.X), math.Min(y0, q.Y)
		x1, y1 = math.Max(x1, q.X), math.Max(y1, q.Y)
	}
	r := rect{int(math.Floor(x0)), int(math.Floor(y0)), 0, 0}
	r.W, r.H = int(math.Ceil(x1))-r.X, int(math.Ceil(y1))-r.Y
	return r
}
