# frost

Freeze-frame screenshots and screen recording for sway.

Press a key and frost freezes the screen, so tooltips and menus that vanish
when you move the mouse stay in the shot. Then pick a rectangle, a freeform
shape, a window or the whole screen. The result is copied to the clipboard and
saved to `~/Pictures/Screenshots`.

## Install

On Arch Linux, install [`frost-sway`](https://aur.archlinux.org/packages/frost-sway)
from the AUR:

```sh
paru -S frost-sway
```

Then add `bindsym Print exec frost` to your sway config.

### From source

You need Go, a C compiler, `libxkbcommon`, `wl-clipboard` and `libnotify`.
A Nerd Font is used for the toolbar icons. Recording needs `ffmpeg`, and audio
needs PulseAudio or `pipewire-pulse`.

```sh
git clone git@github.com:alessiodam/frost.git
cd frost
go build -o frost .
install -m755 frost ~/.local/bin/
```

Add this to your sway config:

```
bindsym Print exec ~/.local/bin/frost
```

## Usage

| Key                   | Action                        |
|-----------------------|-------------------------------|
| `r` `1`               | Rectangle: drag a box         |
| `f` `2`               | Freeform: draw a shape        |
| `w` `3`               | Window: click a window        |
| `s` `4` `Enter`       | Whole screen                  |
| `p` `6`               | Shot tab: take a screenshot   |
| `v` `5`               | Video tab: record the screen  |
| `Esc`, right-click    | Cancel                        |

You can also click the buttons in the toolbar.

Keys, the default mode, the save folder and more can be changed in
`~/.config/frost/config`. See [`config.example`](config.example).

## Recording

Press the key bound to `frost`, choose the Video tab, then pick what to record.
Press the same key again to stop. A screenshot can't be taken with that key
during a recording, because it stops the recording instead. `frost record` opens
the picker with Video already selected.
The video goes to `~/Videos/Screencasts`, and its path is printed and copied to
the clipboard. Freeform shapes are recorded with black outside the shape.

Skip the picker by naming a target:

```sh
frost record -output DP-1
frost record -all
frost record -region "100,100 1280x720"
frost record -window focused
frost record -window app_id=firefox
frost record -window title='^Docs'
frost record -window con_id=42
frost record -window pid=1234
```

`-all` and regions can span monitors, including rotated and mixed-scale ones.
Window recording follows the window itself, even when it is covered, moved or
on another workspace. If it is resized, the video keeps its first size and fits
the window inside. Closing the window ends the recording.

Control a running recording:

```sh
frost record stop
frost record pause
frost record resume
frost record toggle-pause
frost record cancel
frost record status
```

`status` prints JSON for status bars. `SIGINT`, `SIGTERM` and `SIGHUP` also stop
and save. The recording is written to a `.part.mkv` next to the final file, so a
crash leaves a playable file behind.

A badge with the time, pause and stop buttons sits outside the recorded area,
with an outline around regions. If there is no room outside, as with a full
single monitor, the badge only appears during the countdown. `-indicator on`
forces it into the video and `-indicator off` removes it.

### Options

| Option                | Values                                                     |
|-----------------------|------------------------------------------------------------|
| `-format`             | `mp4` `mkv` `webm` `gif` `webp` `apng`                     |
| `-codec`              | `auto` `h264` `hevc` `av1` `vp9`                           |
| `-hw`                 | `auto` `off` `vaapi` `nvenc`, falls back when `auto`       |
| `-vaapi-device`       | render node, for example `/dev/dri/renderD128`             |
| `-quality`            | `low` `medium` `high` `lossless`                           |
| `-crf`, `-bitrate`    | explicit rate control, for example `-bitrate 8M`           |
| `-preset`             | software encoder preset                                    |
| `-fps`                | 1 to 240, default 60, or 15 for animated formats           |
| `-scale`              | resize factor, for example `0.5`                           |
| `-audio`              | `none` `desktop` `mic` `both`, or source names with commas |
| `-audio-tracks`       | keep each source on its own track instead of mixing        |
| `-audio-offset`       | shift audio to correct sync, for example `40ms`            |
| `-audio-bitrate`      | default `192k`                                             |
| `-delay`              | countdown, for example `3s`                                |
| `-duration`           | stop after this long                                       |
| `-cursor`             | `-cursor=false` hides it where the compositor allows       |
| `-o`, `-dir`          | output file, its extension sets the format, or folder      |
| `-vf`, `-ffmpeg-args` | extra ffmpeg filters and output arguments                  |
| `-no-clipboard`, `-no-notify` | skip delivery steps                                |

Every option can also go in the `[record]` section of the config, with `-` written
as `_`. List audio sources with `pactl list short sources`.

## Tests

`go test .` covers the pure logic. `test/run.sh` starts a private headless sway
and PipeWire under `$XDG_RUNTIME_DIR/frost-test`, runs the recording end-to-end
tests there and removes everything afterwards. Your session, sound devices,
home folder and notifications aren't touched. Pass test names to run a subset.

## Limitations

- Only works on wlroots compositors. Window mode only works on sway.
- A screenshot selection can't span multiple monitors.
- Window recordings don't include the cursor, because sway's window capture
  doesn't draw it.
- Audio is per source. Recording only one app's sound isn't supported.

## License

[GPLv3](LICENSE)
