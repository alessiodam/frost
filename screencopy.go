package main

import (
	"fmt"

	"github.com/rajveermalviya/go-wayland/wayland/client"
	"golang.org/x/sys/unix"

	"github.com/alessiodam/frost/protocol/screencopy"
)

const (
	fmtARGB8888    = 0
	fmtXRGB8888    = 1
	fmtABGR8888    = 0x34324241
	fmtXBGR8888    = 0x34324258
	fmtXRGB2101010 = 0x30335258
	fmtARGB2101010 = 0x30335241
	fmtXBGR2101010 = 0x30334258
	fmtABGR2101010 = 0x30334241
	fmtRGB888      = 0x34324752
	fmtBGR888      = 0x34324742
)

type shot struct {
	s      *screen
	fr     *screencopy.ZwlrScreencopyFrameV1
	info   screencopy.ZwlrScreencopyFrameV1BufferEvent
	have   bool
	yInv   bool
	buf    *shmBuffer
	done   bool
	result *frame
	err    error
}

func (a *app) captureAll(screens []*screen) error {
	if a.screencopy == nil {
		return fmt.Errorf("compositor lacks wlr-screencopy")
	}
	shots := make([]*shot, len(screens))
	for i, s := range screens {
		fr, err := a.screencopy.CaptureOutput(0, s.out)
		if err != nil {
			return err
		}
		sh := &shot{s: s, fr: fr}
		shots[i] = sh
		fr.SetBufferHandler(func(e screencopy.ZwlrScreencopyFrameV1BufferEvent) {
			if supportedFormat(e.Format) && !sh.have {
				sh.info, sh.have = e, true
			}
		})
		fr.SetBufferDoneHandler(func(screencopy.ZwlrScreencopyFrameV1BufferDoneEvent) {
			if sh.err = a.startCopy(sh); sh.err != nil {
				sh.done = true
			}
		})
		fr.SetFlagsHandler(func(e screencopy.ZwlrScreencopyFrameV1FlagsEvent) { sh.yInv = e.Flags&1 != 0 })
		fr.SetReadyHandler(func(screencopy.ZwlrScreencopyFrameV1ReadyEvent) {
			sh.result = sh.convert()
			sh.done = true
		})
		fr.SetFailedHandler(func(screencopy.ZwlrScreencopyFrameV1FailedEvent) {
			sh.err, sh.done = fmt.Errorf("screencopy of %s failed", s.name), true
		})
	}
	for {
		pending := false
		for _, sh := range shots {
			pending = pending || !sh.done
		}
		if !pending {
			break
		}
		if err := a.dispatch(); err != nil {
			return err
		}
	}
	for _, sh := range shots {
		sh.release()
		if sh.err != nil {
			return sh.err
		}
		sh.s.cap = sh.result
	}
	return nil
}

func (a *app) startCopy(sh *shot) error {
	if !sh.have {
		return fmt.Errorf("screencopy of %s: no supported shm format", sh.s.name)
	}
	var err error
	if sh.buf, err = a.newShmBuffer(int(sh.info.Width), int(sh.info.Height), int(sh.info.Stride), sh.info.Format); err != nil {
		return err
	}
	return sh.fr.Copy(sh.buf.wl)
}

func supportedFormat(format uint32) bool {
	switch format {
	case fmtARGB8888, fmtXRGB8888, fmtABGR8888, fmtXBGR8888,
		fmtXRGB2101010, fmtARGB2101010, fmtXBGR2101010, fmtABGR2101010,
		fmtRGB888, fmtBGR888:
		return true
	}
	return false
}

func formatBytes(format uint32) int {
	if format == fmtRGB888 || format == fmtBGR888 {
		return 3
	}
	return 4
}

func formatRow(mem []byte, y, w, stride int, format uint32) []byte {
	return mem[y*stride : y*stride+w*formatBytes(format)]
}

type shmBuffer struct {
	wl           *client.Buffer
	pool         *client.ShmPool
	mem          []byte
	w, h, stride int
	format       uint32
}

func (a *app) newShmBuffer(w, h, stride int, format uint32) (*shmBuffer, error) {
	size := stride * h
	fd, err := unix.MemfdCreate("frost-capture", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		return nil, err
	}
	b := &shmBuffer{w: w, h: h, stride: stride, format: format}
	if b.mem, err = unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED); err != nil {
		return nil, err
	}
	if b.pool, err = a.shm.CreatePool(fd, int32(size)); err != nil {
		b.destroy()
		return nil, err
	}
	if b.wl, err = b.pool.CreateBuffer(0, int32(w), int32(h), int32(stride), format); err != nil {
		b.destroy()
		return nil, err
	}
	return b, nil
}

func (b *shmBuffer) destroy() {
	if b.wl != nil {
		b.wl.Destroy()
	}
	if b.pool != nil {
		b.pool.Destroy()
	}
	if b.mem != nil {
		unix.Munmap(b.mem)
	}
}

func (sh *shot) convert() *frame {
	w, h, stride := int(sh.info.Width), int(sh.info.Height), int(sh.info.Stride)
	f := &frame{W: w, H: h, Pix: make([]byte, w*h*4)}
	for y := 0; y < h; y++ {
		sy := y
		if sh.yInv {
			sy = h - 1 - y
		}
		convertRow(f.Pix[y*w*4:(y+1)*w*4], formatRow(sh.buf.mem, sy, w, stride, sh.info.Format), sh.info.Format)
	}
	return f
}

func convertRow(dst, src []byte, format uint32) {
	switch format {
	case fmtARGB8888, fmtXRGB8888:
		copy(dst, src)
		for i := 3; i < len(dst); i += 4 {
			dst[i] = 0xff
		}
	case fmtABGR8888, fmtXBGR8888:
		for i := 0; i < len(dst); i += 4 {
			dst[i], dst[i+1], dst[i+2], dst[i+3] = src[i+2], src[i+1], src[i], 0xff
		}
	case fmtRGB888:
		for i, j := 0, 0; i < len(dst); i, j = i+4, j+3 {
			dst[i], dst[i+1], dst[i+2], dst[i+3] = src[j], src[j+1], src[j+2], 0xff
		}
	case fmtBGR888:
		for i, j := 0, 0; i < len(dst); i, j = i+4, j+3 {
			dst[i], dst[i+1], dst[i+2], dst[i+3] = src[j+2], src[j+1], src[j], 0xff
		}
	default:
		bgr := format == fmtXBGR2101010 || format == fmtABGR2101010
		for i := 0; i < len(dst); i += 4 {
			v := uint32(src[i]) | uint32(src[i+1])<<8 | uint32(src[i+2])<<16 | uint32(src[i+3])<<24
			lo, mid, hi := byte(v>>2), byte(v>>12), byte(v>>22)
			if bgr {
				dst[i], dst[i+1], dst[i+2] = hi, mid, lo
			} else {
				dst[i], dst[i+1], dst[i+2] = lo, mid, hi
			}
			dst[i+3] = 0xff
		}
	}
}

func (sh *shot) release() {
	if sh.buf != nil {
		sh.buf.destroy()
	}
	sh.fr.Destroy()
}
