package main

/*
#cgo pkg-config: xkbcommon
#include <stdlib.h>
#include <xkbcommon/xkbcommon.h>
*/
import "C"

import (
	"bytes"
	"errors"
	"strings"
	"unicode"
	"unsafe"

	"golang.org/x/sys/unix"
)

type xkb struct {
	ctx    *C.struct_xkb_context
	keymap *C.struct_xkb_keymap
	state  *C.struct_xkb_state
}

func newXKB() *xkb {
	return &xkb{ctx: C.xkb_context_new(C.XKB_CONTEXT_NO_FLAGS)}
}

func (x *xkb) setKeymap(fd int, size uint32) error {
	defer unix.Close(fd)
	data, err := unix.Mmap(fd, 0, int(size), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		return err
	}
	text := C.CString(string(bytes.TrimRight(data, "\x00")))
	unix.Munmap(data)
	defer C.free(unsafe.Pointer(text))

	km := C.xkb_keymap_new_from_string(x.ctx, text, C.XKB_KEYMAP_FORMAT_TEXT_V1, C.XKB_KEYMAP_COMPILE_NO_FLAGS)
	if km == nil {
		return errors.New("compositor sent an invalid keymap")
	}
	if x.state != nil {
		C.xkb_state_unref(x.state)
		C.xkb_keymap_unref(x.keymap)
	}
	x.keymap, x.state = km, C.xkb_state_new(km)
	return nil
}

func (x *xkb) updateMods(depressed, latched, locked, group uint32) {
	if x.state != nil {
		C.xkb_state_update_mask(x.state, C.xkb_mod_mask_t(depressed), C.xkb_mod_mask_t(latched),
			C.xkb_mod_mask_t(locked), 0, 0, C.xkb_layout_index_t(group))
	}
}

func (x *xkb) syms(evdev uint32) []uint32 {
	if x.state == nil {
		return nil
	}
	kc := C.xkb_keycode_t(evdev + 8)
	out := []uint32{uint32(C.xkb_state_key_get_one_sym(x.state, kc))}
	layout := C.xkb_state_key_get_layout(x.state, kc)
	levels := C.xkb_keymap_num_levels_for_key(x.keymap, kc, layout)
	for lvl := C.xkb_level_index_t(0); lvl < levels; lvl++ {
		var syms *C.xkb_keysym_t
		n := C.xkb_keymap_key_get_syms_by_level(x.keymap, kc, layout, lvl, &syms)
		if n > 0 {
			for _, s := range unsafe.Slice(syms, int(n)) {
				out = append(out, uint32(s))
			}
		}
	}
	return out
}

func keysymFromName(name string) uint32 {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	return uint32(C.xkb_keysym_from_name(cs, C.XKB_KEYSYM_CASE_INSENSITIVE))
}

func keysymLower(sym uint32) uint32 {
	return uint32(C.xkb_keysym_to_lower(C.xkb_keysym_t(sym)))
}

var keysymLabels = map[string]string{
	"Escape": "Esc", "Return": "Enter", "KP_Enter": "Enter", "space": "Space",
	"BackSpace": "Bksp", "Tab": "Tab", "Delete": "Del",
}

func keysymLabel(sym uint32) string {
	if r := rune(C.xkb_keysym_to_utf32(C.xkb_keysym_t(sym))); r > ' ' && unicode.IsPrint(r) {
		return strings.ToUpper(string(r))
	}
	buf := make([]C.char, 64)
	n := C.xkb_keysym_get_name(C.xkb_keysym_t(sym), &buf[0], C.size_t(len(buf)))
	if n <= 0 {
		return "?"
	}
	name := C.GoString(&buf[0])
	if l, ok := keysymLabels[name]; ok {
		return l
	}
	return name
}
