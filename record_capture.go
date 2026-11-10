package main

import (
	"fmt"
	"image/color"
	"math"
	"net"
	"reflect"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/rajveermalviya/go-wayland/wayland/client"

	"github.com/alessiodam/frost/protocol/imagecapture"
)

var transformNames = map[string]uint32{
	"": 0, "normal": 0, "90": 1, "180": 2, "270": 3,
	"flipped": 4, "flipped-90": 5, "flipped-180": 6, "flipped-270": 7,
}

func displayPoint(t uint32, u, v, w, h int) (int, int) {
	switch t {
	case 1:
		return h - 1 - v, u
	case 2:
		return w - 1 - u, h - 1 - v
	case 3:
		return v, w - 1 - u
	case 4:
		return w - 1 - u, v
	case 5:
		return h - 1 - v, w - 1 - u
	case 6:
		return u, h - 1 - v
	case 7:
		return v, u
	}
	return u, v
}

func displaySize(t uint32, bw, bh int) (int, int) {
	if t&1 != 0 {
		return bh, bw
	}
	return bw, bh
}

func fitRect(sw, sh, cw, ch int) rect {
	scale := math.Min(float64(cw)/float64(sw), float64(ch)/float64(sh))
	w, h := min(roundInt(float64(sw)*scale), cw), min(roundInt(float64(sh)*scale), ch)
	return rect{(cw - w) / 2, (ch - h) / 2, w, h}
}

func parallelRows(h int, row func(y int)) {
	workers := min(runtime.GOMAXPROCS(0), h)
	if workers <= 1 {
		for y := 0; y < h; y++ {
			row(y)
		}
		return
	}
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			for y := h * i / workers; y < h*(i+1)/workers; y++ {
				row(y)
			}
		})
	}
	wg.Wait()
}

type capSource struct {
	r         *recorder
	name      string
	dst       rect
	fit       bool
	transform uint32
	yInvert   bool

	buf      *shmBuffer
	frameBuf *shmBuffer
	retired  []*shmBuffer
	w, h     int
	stride   int
	format   uint32
	haveInfo bool
	hasFrame bool
	inflight bool
	nextAt   time.Time
	fails    int
	bgra     []byte
	lastFit  rect

	request func() error
}

func (s *capSource) setInfo(w, h, stride int, format uint32) error {
	s.w, s.h, s.stride, s.format, s.haveInfo = w, h, stride, format, true
	if s.buf != nil && (s.buf.w != w || s.buf.h != h || s.buf.stride != stride || s.buf.format != format) {
		s.retired = append(s.retired, s.buf)
		s.buf = nil
	}
	if s.buf == nil {
		var err error
		if s.buf, err = s.r.a.newShmBuffer(w, h, stride, format); err != nil {
			return err
		}
	}
	return s.r.onSourceInfo()
}

func (s *capSource) releaseRetired() {
	for _, b := range s.retired {
		b.destroy()
	}
	s.retired = nil
}

func (s *capSource) onReady() {
	s.inflight, s.fails = false, 0
	if s.frameBuf == s.buf {
		s.r.mu.Lock()
		if s.r.frame != nil {
			s.blit(s.r.frame)
			s.r.applyMask()
			s.hasFrame = true
		}
		s.r.mu.Unlock()
	}
	s.releaseRetired()
	s.r.wake()
}

func (s *capSource) onFailed(err error) {
	s.inflight = false
	s.releaseRetired()
	s.fails++
	if s.fails > 120 {
		s.r.requestStop(err)
	}
}

func (s *capSource) blit(c *canvas) {
	dw, dh := displaySize(s.transform, s.w, s.h)
	dst := s.dst
	if s.fit {
		dst = fitRect(dw, dh, c.w, c.h)
		if dst != s.lastFit {
			c.fill(rect{0, 0, c.w, c.h}, color.RGBA{A: 0xff})
			s.lastFit = dst
		}
	}
	if s.transform == 0 && dst.W == s.w && dst.H == s.h {
		parallelRows(s.h, func(y int) {
			sy := y
			if s.yInvert {
				sy = s.h - 1 - y
			}
			o := ((dst.Y+y)*c.w + dst.X) * 4
			convertRow(c.pix[o:o+s.w*4], formatRow(s.buf.mem, sy, s.w, s.stride, s.format), s.format)
		})
		return
	}
	if len(s.bgra) != s.w*s.h*4 {
		s.bgra = make([]byte, s.w*s.h*4)
	}
	parallelRows(s.h, func(y int) {
		sy := y
		if s.yInvert {
			sy = s.h - 1 - y
		}
		convertRow(s.bgra[y*s.w*4:(y+1)*s.w*4], formatRow(s.buf.mem, sy, s.w, s.stride, s.format), s.format)
	})
	parallelRows(dst.H, func(y int) {
		v := min(y*dh/dst.H, dh-1)
		row := c.pix[((dst.Y+y)*c.w+dst.X)*4:]
		for x := 0; x < dst.W; x++ {
			bx, by := displayPoint(s.transform, min(x*dw/dst.W, dw-1), v, dw, dh)
			o := (by*s.w + bx) * 4
			copy(row[x*4:x*4+4], s.bgra[o:o+4])
		}
	})
}

func cursorFlag(on bool) int32 {
	if on {
		return 1
	}
	return 0
}

func (r *recorder) outputSource(sc *screen, local rect) *capSource {
	s := &capSource{r: r, name: sc.name, transform: transformNames[sc.info.Transform]}
	s.request = func() error {
		fr, err := r.a.screencopy.CaptureOutputRegion(cursorFlag(r.o.cursor), sc.out,
			int32(local.X), int32(local.Y), int32(local.W), int32(local.H))
		if err != nil {
			return err
		}
		var info struct {
			w, h, stride int
			format       uint32
			have         bool
		}
		fr.SetBufferHandler(func(e screencopyBufferEvent) {
			if supportedFormat(e.Format) && !info.have {
				info.w, info.h, info.stride, info.format, info.have = int(e.Width), int(e.Height), int(e.Stride), e.Format, true
			}
		})
		fr.SetBufferDoneHandler(func(screencopyBufferDoneEvent) {
			if !info.have {
				r.requestStop(fmt.Errorf("screencopy of %s: no supported shm format", sc.name))
				return
			}
			if err := s.setInfo(info.w, info.h, info.stride, info.format); err != nil {
				r.requestStop(err)
				return
			}
			s.frameBuf = s.buf
			fr.CopyWithDamage(s.buf.wl)
		})
		fr.SetFlagsHandler(func(e screencopyFlagsEvent) { s.yInvert = e.Flags&1 != 0 })
		fr.SetReadyHandler(func(screencopyReadyEvent) {
			s.onReady()
			fr.Destroy()
		})
		fr.SetFailedHandler(func(screencopyFailedEvent) {
			s.onFailed(fmt.Errorf("screencopy of %s failed", sc.name))
			fr.Destroy()
		})
		return nil
	}
	return s
}

type toplevelList struct {
	client.BaseProxy
	found func(id uint32)
}

func (l *toplevelList) Dispatch(opcode uint32, _ int, data []byte) {
	if opcode == 0 {
		l.found(client.Uint32(data))
	}
}

func contextField[T any](ctx *client.Context, name string) T {
	f := reflect.ValueOf(ctx).Elem().FieldByName(name)
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(T)
}

func waylandFD(ctx *client.Context) (int, error) {
	raw, err := contextField[*net.UnixConn](ctx, "conn").SyscallConn()
	if err != nil {
		return 0, err
	}
	fd := -1
	if err := raw.Control(func(v uintptr) { fd = int(v) }); err != nil {
		return 0, err
	}
	return fd, nil
}

func (a *app) findToplevel(identifier string) (*imagecapture.ExtForeignToplevelHandleV1, error) {
	objects := contextField[map[uint32]client.Proxy](a.ctx, "objects")
	found := map[string]*imagecapture.ExtForeignToplevelHandleV1{}
	list := &toplevelList{}
	list.found = func(id uint32) {
		h := &imagecapture.ExtForeignToplevelHandleV1{}
		h.SetContext(a.ctx)
		h.SetID(id)
		h.SetIdentifierHandler(func(e imagecapture.ExtForeignToplevelHandleV1IdentifierEvent) { found[e.Identifier] = h })
		objects[id] = h
	}
	a.ctx.Register(list)
	if err := a.registry.Bind(a.listGlobal, "ext_foreign_toplevel_list_v1", 1, list); err != nil {
		return nil, err
	}
	if err := a.roundtrip(); err != nil {
		return nil, err
	}
	if err := a.roundtrip(); err != nil {
		return nil, err
	}
	h, ok := found[identifier]
	if !ok {
		return nil, fmt.Errorf("window %s is gone", identifier)
	}
	return h, nil
}

func (r *recorder) windowSource(w *swayNode) (*capSource, error) {
	a := r.a
	h, err := a.findToplevel(w.Toplevel)
	if err != nil {
		return nil, err
	}
	source, err := a.winSources.CreateSource(h)
	if err != nil {
		return nil, err
	}
	session, err := a.imageCopy.CreateSession(source, uint32(cursorFlag(r.o.cursor)))
	if err != nil {
		return nil, err
	}
	s := &capSource{r: r, name: "window", fit: true}
	var pendW, pendH int
	var pendFormat uint32
	var haveFormat, offered bool
	session.SetBufferSizeHandler(func(e imagecapture.ExtImageCopyCaptureSessionV1BufferSizeEvent) {
		pendW, pendH = int(e.Width), int(e.Height)
	})
	session.SetShmFormatHandler(func(e imagecapture.ExtImageCopyCaptureSessionV1ShmFormatEvent) {
		if supportedFormat(e.Format) && (!offered || e.Format == fmtXRGB8888) {
			pendFormat, haveFormat, offered = e.Format, true, true
		}
	})
	session.SetDoneHandler(func(imagecapture.ExtImageCopyCaptureSessionV1DoneEvent) {
		if !haveFormat {
			r.requestStop(fmt.Errorf("window capture: no supported shm format"))
			return
		}
		if err := s.setInfo(pendW, pendH, pendW*formatBytes(pendFormat), pendFormat); err != nil {
			r.requestStop(err)
		}
		offered = false
	})
	session.SetStoppedHandler(func(imagecapture.ExtImageCopyCaptureSessionV1StoppedEvent) {
		r.requestStop(nil)
	})
	s.request = func() error {
		if s.buf == nil {
			return nil
		}
		fr, err := session.CreateFrame()
		if err != nil {
			return err
		}
		s.inflight, s.frameBuf = true, s.buf
		fr.SetTransformHandler(func(e imagecapture.ExtImageCopyCaptureFrameV1TransformEvent) { s.transform = e.Transform })
		fr.SetReadyHandler(func(imagecapture.ExtImageCopyCaptureFrameV1ReadyEvent) {
			s.onReady()
			fr.Destroy()
		})
		fr.SetFailedHandler(func(e imagecapture.ExtImageCopyCaptureFrameV1FailedEvent) {
			fr.Destroy()
			if e.Reason == 1 {
				s.inflight = false
				s.releaseRetired()
				return
			}
			s.onFailed(fmt.Errorf("window capture failed"))
		})
		fr.AttachBuffer(s.buf.wl)
		fr.DamageBuffer(0, 0, int32(s.w), int32(s.h))
		return fr.Capture()
	}
	return s, nil
}

func (r *recorder) buildSources() error {
	t := r.target
	if t.window != nil && t.window.Toplevel != "" && r.a.imageCopy != nil && r.a.winSources != nil && r.a.listGlobal != 0 {
		s, err := r.windowSource(t.window)
		if err != nil {
			return err
		}
		r.sources = []*capSource{s}
		return nil
	}
	if r.a.screencopy == nil {
		return fmt.Errorf("compositor lacks wlr-screencopy")
	}
	maxScale := 1.0
	for _, sc := range r.a.screens {
		if !sc.info.Rect.intersect(t.area).empty() {
			maxScale = math.Max(maxScale, sc.info.Scale)
		}
	}
	r.scale = maxScale
	for _, sc := range r.a.screens {
		part := sc.info.Rect.intersect(t.area)
		if part.empty() {
			continue
		}
		local := rect{part.X - sc.info.Rect.X, part.Y - sc.info.Rect.Y, part.W, part.H}
		s := r.outputSource(sc, local)
		x0, y0 := roundInt(float64(part.X-t.area.X)*maxScale), roundInt(float64(part.Y-t.area.Y)*maxScale)
		x1 := roundInt(float64(part.X+part.W-t.area.X) * maxScale)
		y1 := roundInt(float64(part.Y+part.H-t.area.Y) * maxScale)
		s.dst = rect{x0, y0, x1 - x0, y1 - y0}
		r.sources = append(r.sources, s)
	}
	if len(r.sources) == 0 {
		return fmt.Errorf("%s is not on any output", t.label)
	}
	return nil
}

func (r *recorder) canvasSize() (int, int) {
	if len(r.sources) == 1 {
		s := r.sources[0]
		w, h := displaySize(s.transform, s.w, s.h)
		s.dst = rect{0, 0, w, h}
		return w, h
	}
	return roundInt(float64(r.target.area.W) * r.scale), roundInt(float64(r.target.area.H) * r.scale)
}

func (r *recorder) buildMask(w, h int) {
	if len(r.target.poly) < 3 {
		return
	}
	poly := make([]pt, len(r.target.poly))
	for i, p := range r.target.poly {
		poly[i] = pt{p.X * float64(w), p.Y * float64(h)}
	}
	cov := coverage(poly, rect{0, 0, w, h})
	r.mask = make([]uint8, len(cov))
	for i, c := range cov {
		r.mask[i] = uint8(math.Round(c * 255))
	}
}

func (r *recorder) applyMask() {
	if r.mask == nil {
		return
	}
	c := r.frame
	parallelRows(c.h, func(y int) {
		row := c.pix[y*c.w*4 : (y+1)*c.w*4]
		m := r.mask[y*c.w : (y+1)*c.w]
		for x, a := range m {
			switch a {
			case 255:
			case 0:
				row[x*4], row[x*4+1], row[x*4+2] = 0, 0, 0
			default:
				row[x*4] = uint8(uint32(row[x*4]) * uint32(a) / 255)
				row[x*4+1] = uint8(uint32(row[x*4+1]) * uint32(a) / 255)
				row[x*4+2] = uint8(uint32(row[x*4+2]) * uint32(a) / 255)
			}
		}
	})
}
