package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

func (r *result) crop() *image.NRGBA {
	s := r.s
	src, stride := s.base, s.bw
	if r.full {
		src, stride = s.cap.Pix, s.cap.W
		r.r = rect{0, 0, s.cap.W, s.cap.H}
	}
	img := image.NewNRGBA(image.Rect(0, 0, r.r.W, r.r.H))
	var cov []float64
	if r.poly != nil {
		cov = coverage(r.poly, r.r)
	}
	for y := 0; y < r.r.H; y++ {
		src := src[((r.r.Y+y)*stride+r.r.X)*4:]
		dst := img.Pix[y*img.Stride:]
		for x := 0; x < r.r.W; x++ {
			a := uint8(255)
			if cov != nil {
				a = uint8(math.Round(cov[y*r.r.W+x] * 255))
				if a == 0 {
					continue
				}
			}
			dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = src[x*4+2], src[x*4+1], src[x*4], a
		}
	}
	return img
}

func coverage(poly []pt, box rect) []float64 {
	const sub = 4
	cov := make([]float64, box.W*box.H)
	var xs []float64
	for y := 0; y < box.H; y++ {
		row := cov[y*box.W : (y+1)*box.W]
		for k := 0; k < sub; k++ {
			sy := float64(box.Y+y) + (float64(k)+0.5)/sub
			xs = xs[:0]
			for i := range poly {
				a, b := poly[i], poly[(i+1)%len(poly)]
				if (a.Y <= sy) != (b.Y <= sy) {
					xs = append(xs, a.X+(sy-a.Y)/(b.Y-a.Y)*(b.X-a.X)-float64(box.X))
				}
			}
			sort.Float64s(xs)
			for i := 0; i+1 < len(xs); i += 2 {
				x0, x1 := math.Max(xs[i], 0), math.Min(xs[i+1], float64(box.W))
				for px := int(x0); px < box.W && float64(px) < x1; px++ {
					l, r := math.Max(x0, float64(px)), math.Min(x1, float64(px+1))
					if r > l {
						row[px] += (r - l) / sub
					}
				}
			}
		}
	}
	return cov
}

func deliver(r *result, cfg *config) error {
	img := r.crop()
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return err
	}

	copied := false
	if cfg.clipboard {
		cmd := exec.Command("wl-copy", "--type", "image/png")
		cmd.Stdin = bytes.NewReader(buf.Bytes())
		err := cmd.Run()
		if err != nil && !cfg.save {
			return fmt.Errorf("wl-copy: %w", err)
		}
		copied = err == nil
	}

	var path string
	if cfg.save {
		var err error
		if path, err = save(buf.Bytes(), cfg.dir); err != nil {
			return err
		}
	}
	if cfg.notify && (copied || path != "") {
		notify(path, img.Bounds().Dx(), img.Bounds().Dy(), copied)
	}
	if path != "" {
		fmt.Println(path)
	}
	return nil
}

func save(data []byte, dir string) (string, error) {
	if dir == "" {
		pics := os.Getenv("XDG_PICTURES_DIR")
		if pics == "" {
			if out, err := exec.Command("xdg-user-dir", "PICTURES").Output(); err == nil {
				pics = strings.TrimSpace(string(out))
			}
		}
		if pics == "" {
			home, _ := os.UserHomeDir()
			pics = filepath.Join(home, "Pictures")
		}
		dir = filepath.Join(pics, "Screenshots")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	stamp := time.Now().Format("2006-01-02_15-04-05")
	path := filepath.Join(dir, stamp+".png")
	for i := 2; ; i++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			path = filepath.Join(dir, fmt.Sprintf("%s_%d.png", stamp, i))
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return path, err
	}
}

func notify(path string, w, h int, copied bool) {
	title := "Screenshot copied"
	if !copied {
		title = "Screenshot saved"
	}
	body := fmt.Sprintf("%d × %d", w, h)
	if path != "" {
		body += " · " + filepath.Base(path)
	}
	script := `r=$(notify-send -a frost -i "$1" -A default=Open "$2" "$3") && [ "$r" = default ] && [ -n "$1" ] && exec xdg-open "$1"`
	cmd := exec.Command("sh", "-c", script, "sh", path, title, body)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Start()
}
