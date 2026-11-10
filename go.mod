module github.com/alessiodam/frost

go 1.26.0

require (
	github.com/rajveermalviya/go-wayland/wayland v0.0.0-20230130181619-0ad78d1310b2
	golang.org/x/image v0.46.0
	golang.org/x/sys v0.48.0
)

require golang.org/x/text v0.42.0 // indirect

replace github.com/rajveermalviya/go-wayland/wayland => github.com/alessiodam/go-wayland/wayland v0.0.0-20260923181718-a36a3be795c9
