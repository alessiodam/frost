# frost

Freeze-frame screenshots for sway.

Press a key and frost freezes the screen, so tooltips and menus that vanish
when you move the mouse stay in the shot. Then pick a rectangle, a freeform
shape, a window or the whole screen. The result is copied to the clipboard and
saved to `~/Pictures/Screenshots`.

## Install

You need Go, a C compiler, `libxkbcommon`, `wl-clipboard` and `libnotify`.
A Nerd Font is used for the toolbar icons.

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
| `Esc`, right-click    | Cancel                        |

You can also click the buttons in the toolbar.

Keys, the default mode, the save folder and more can be changed in
`~/.config/frost/config`. See [`config.example`](config.example).

## Limitations

- Only works on wlroots compositors. Window mode only works on sway.
- A selection can't span multiple monitors.

## License

[GPLv3](LICENSE)
