package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseRegion(t *testing.T) {
	r, err := parseRegion("10,-20 300x400")
	if err != nil || r != (rect{10, -20, 300, 400}) {
		t.Fatalf("got %v, %v", r, err)
	}
	for _, bad := range []string{"", "10,20", "10,20 0x5", "a,b cxd"} {
		if _, err := parseRegion(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDisplayPointIsBijective(t *testing.T) {
	for transform := uint32(0); transform < 8; transform++ {
		dw, dh := 4, 3
		bw, bh := dw, dh
		if transform&1 != 0 {
			bw, bh = dh, dw
		}
		seen := map[[2]int]bool{}
		for v := 0; v < dh; v++ {
			for u := 0; u < dw; u++ {
				x, y := displayPoint(transform, u, v, dw, dh)
				if x < 0 || y < 0 || x >= bw || y >= bh {
					t.Fatalf("transform %d: (%d,%d) maps outside buffer to (%d,%d)", transform, u, v, x, y)
				}
				seen[[2]int{x, y}] = true
			}
		}
		if len(seen) != dw*dh {
			t.Errorf("transform %d covers %d of %d pixels", transform, len(seen), dw*dh)
		}
	}
}

func TestFitRect(t *testing.T) {
	cases := []struct {
		sw, sh, cw, ch int
		want           rect
	}{
		{600, 400, 600, 400, rect{0, 0, 600, 400}},
		{300, 400, 600, 400, rect{150, 0, 300, 400}},
		{1200, 400, 600, 400, rect{0, 100, 600, 200}},
	}
	for _, c := range cases {
		if got := fitRect(c.sw, c.sh, c.cw, c.ch); got != c.want {
			t.Errorf("fitRect(%d,%d,%d,%d) = %v, want %v", c.sw, c.sh, c.cw, c.ch, got, c.want)
		}
	}
}

func TestPlaceBadge(t *testing.T) {
	one := []swayOutput{{Name: "A", Rect: rect{0, 0, 1000, 800}}}
	two := append(one, swayOutput{Name: "B", Rect: rect{1000, 0, 1000, 800}})

	_, box, outside := placeBadge(one, rect{100, 100, 400, 300}, 200, 28, 12)
	if !outside || box.Y != 412 {
		t.Errorf("below region: %v %v", box, outside)
	}
	_, box, outside = placeBadge(one, rect{100, 700, 400, 100}, 200, 28, 12)
	if !outside || box.Y != 660 {
		t.Errorf("above region: %v %v", box, outside)
	}
	if _, _, outside = placeBadge(one, rect{0, 0, 1000, 800}, 200, 28, 12); outside {
		t.Error("whole output has no outside spot")
	}
	i, _, outside := placeBadge(two, rect{0, 0, 1000, 800}, 200, 28, 12)
	if !outside || i != 1 {
		t.Errorf("other output: %d %v", i, outside)
	}
	i, _, outside = placeBadge(two, rect{}, 200, 28, 12)
	if !outside || i != 0 {
		t.Errorf("window target: %d %v", i, outside)
	}
}

func TestFindWindow(t *testing.T) {
	root := swayNode{Nodes: []swayNode{
		{Pid: 10, ID: 1, AppID: "foot", Name: "shell"},
		{FloatingNodes: []swayNode{{Pid: 20, ID: 2, AppID: "firefox", Name: "Docs", Focused: true}}},
	}}
	for criteria, want := range map[string]int{
		"focused": 2, "app_id=^foot$": 1, "title=Do.s": 2, "con_id=1": 1, "pid=20": 2,
	} {
		w, err := findWindow(root, criteria)
		if err != nil || w.ID != want {
			t.Errorf("%s: %v %v", criteria, w, err)
		}
	}
	for _, bad := range []string{"app_id=nope", "bogus", "pid=x", "title=("} {
		if _, err := findWindow(root, bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestNormalize(t *testing.T) {
	valid := func(mut func(*recOptions)) *recOptions {
		o := newRecOptions()
		mut(o)
		if err := o.normalize(); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		return o
	}
	if o := valid(func(*recOptions) {}); o.fps != 60 || o.codec != "h264" {
		t.Errorf("defaults: %+v", o)
	}
	if o := valid(func(o *recOptions) { o.out = "/x/a.webm" }); o.format != "webm" || o.codec != "vp9" {
		t.Errorf("webm from extension: %+v", o)
	}
	if o := valid(func(o *recOptions) { o.format = "gif" }); o.fps != 15 {
		t.Errorf("gif fps: %d", o.fps)
	}
	invalid := []func(*recOptions){
		func(o *recOptions) { o.fps = 999 },
		func(o *recOptions) { o.format = "avi" },
		func(o *recOptions) { o.out = "/x/a.avi" },
		func(o *recOptions) { o.codec = "h264"; o.format = "webm" },
		func(o *recOptions) { o.codec = "vp9"; o.format = "mp4" },
		func(o *recOptions) { o.codec = "av1"; o.quality = "lossless" },
		func(o *recOptions) { o.format = "gif"; o.audio = "mic" },
		func(o *recOptions) { o.scale = 0 },
		func(o *recOptions) { o.delay = -time.Second },
		func(o *recOptions) { o.mode = "nope" },
	}
	for i, mut := range invalid {
		o := newRecOptions()
		mut(o)
		if o.normalize() == nil {
			t.Errorf("invalid case %d accepted", i)
		}
	}
}

func TestAudioSources(t *testing.T) {
	cases := map[string]string{
		"none": "", "desktop": "@DEFAULT_MONITOR@", "mic": "@DEFAULT_SOURCE@",
		"both": "@DEFAULT_MONITOR@ @DEFAULT_SOURCE@", "a, b,": "a b",
	}
	for in, want := range cases {
		o := &recOptions{audio: in}
		if got := strings.Join(o.audioSources(), " "); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestEncoderArgs(t *testing.T) {
	o := newRecOptions()
	o.fps = 30
	o.audio = "both"
	if err := o.normalize(); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(o.encoderArgs(softwareEncoder(o), 640, 360, 2, "/tmp/x.mkv"), " ")
	for _, want := range []string{
		"-video_size 640x360", "-framerate 30", "-i pipe:0", "-i pipe:3", "-i pipe:4",
		"amix=inputs=2", "-map [frostaudio]", "-c:v libx264", "-crf 18", "-c:a aac", "-f matroska /tmp/x.mkv",
		"format=yuv420p",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in %s", want, args)
		}
	}
	o.audioTracks = true
	args = strings.Join(o.encoderArgs(softwareEncoder(o), 640, 360, 2, "/tmp/x.mkv"), " ")
	if strings.Contains(args, "amix") || !strings.Contains(args, "-map 1:a -map 2:a") {
		t.Errorf("separate tracks: %s", args)
	}
	lossless := newRecOptions()
	lossless.quality = "lossless"
	lossless.normalize()
	if e := softwareEncoder(lossless); e.pixFmt != "yuv444p" || !strings.Contains(strings.Join(e.args, " "), "-qp 0") {
		t.Errorf("lossless: %+v", e)
	}
	if got := encoderCandidates(lossless); len(got) != 1 || got[0].name != "libx264" {
		t.Errorf("lossless candidates: %+v", got)
	}
}

func TestClockExcludesPauses(t *testing.T) {
	var c recClock
	if c.elapsed() != 0 {
		t.Error("clock runs before start")
	}
	c.start()
	time.Sleep(20 * time.Millisecond)
	c.setPaused(true)
	paused := c.elapsed()
	time.Sleep(20 * time.Millisecond)
	if c.elapsed() != paused || paused < 20*time.Millisecond {
		t.Errorf("paused elapsed moved: %v -> %v", paused, c.elapsed())
	}
	c.setPaused(false)
	time.Sleep(5 * time.Millisecond)
	if c.elapsed() <= paused {
		t.Error("clock did not resume")
	}
}

func TestConvertRow24Bit(t *testing.T) {
	dst := make([]byte, 4)
	convertRow(dst, []byte{1, 2, 3}, fmtBGR888)
	if dst[0] != 3 || dst[1] != 2 || dst[2] != 1 || dst[3] != 0xff {
		t.Errorf("BGR888: %v", dst)
	}
	convertRow(dst, []byte{1, 2, 3}, fmtRGB888)
	if dst[0] != 1 || dst[1] != 2 || dst[2] != 3 {
		t.Errorf("RGB888: %v", dst)
	}
}
