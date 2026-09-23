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
)

type shot struct {
	s      *screen
	fr     *screencopy.ZwlrScreencopyFrameV1
	info   screencopy.ZwlrScreencopyFrameV1BufferEvent
	have   bool
	yInv   bool
	mem    []byte
	buf    *client.Buffer
	pool   *client.ShmPool
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
			switch e.Format {
			case fmtARGB8888, fmtXRGB8888, fmtABGR8888, fmtXBGR8888,
				fmtXRGB2101010, fmtARGB2101010, fmtXBGR2101010, fmtABGR2101010:
				if !sh.have {
					sh.info, sh.have = e, true
				}
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
	size := int(sh.info.Stride * sh.info.Height)
	fd, err := unix.MemfdCreate("frost-capture", unix.MFD_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		return err
	}
	if sh.mem, err = unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED); err != nil {
		return err
	}
	if sh.pool, err = a.shm.CreatePool(fd, int32(size)); err != nil {
		return err
	}
	if sh.buf, err = sh.pool.CreateBuffer(0, int32(sh.info.Width), int32(sh.info.Height),
		int32(sh.info.Stride), sh.info.Format); err != nil {
		return err
	}
	return sh.fr.Copy(sh.buf)
}

func (sh *shot) convert() *frame {
	w, h, stride := int(sh.info.Width), int(sh.info.Height), int(sh.info.Stride)
	format := sh.info.Format
	f := &frame{W: w, H: h, Pix: make([]byte, w*h*4)}
	for y := 0; y < h; y++ {
		sy := y
		if sh.yInv {
			sy = h - 1 - y
		}
		src := sh.mem[sy*stride : sy*stride+w*4]
		dst := f.Pix[y*w*4 : (y+1)*w*4]
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
	return f
}

func (sh *shot) release() {
	if sh.buf != nil {
		sh.buf.Destroy()
	}
	if sh.pool != nil {
		sh.pool.Destroy()
	}
	if sh.mem != nil {
		unix.Munmap(sh.mem)
	}
	sh.fr.Destroy()
}
