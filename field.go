package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// textField is a one-line text field that edits like a macOS one: a caret
// and a selection, Option for words, Command for the whole line, Shift to
// select, Command-A, Command-Z, and the Emacs-style Control keys Cocoa
// fields take (your Rex may also send Control-A and -E for Command-Left
// and -Right).
type textField struct {
	text        string
	cur, anchor int // byte offsets; the selection runs between them
	undo, redo  []fieldSnap
	last        editKind // the last edit, so a run of typing undoes at once
	scrollCol   int      // how many cells the text is scrolled left
}

type fieldSnap struct {
	text        string
	cur, anchor int
}

type editKind int

const (
	editNone editKind = iota
	editType
	editDelete
)

// unit is how far a move or delete reaches.
type unit int

const (
	byChar unit = iota
	byWord
	byLine
)

func (f *textField) sel() (int, int) { return min(f.cur, f.anchor), max(f.cur, f.anchor) }
func (f *textField) hasSel() bool    { return f.cur != f.anchor }
func (f *textField) selected() string {
	a, b := f.sel()
	return f.text[a:b]
}

// fieldResult says what a key did to the field.
type fieldResult struct {
	handled bool   // the field used the key
	edited  bool   // the text changed
	copy    string // text to put on the clipboard
}

// handle applies one key press.
func (f *textField) handle(k keyPress) fieldResult {
	shift := k.mods&modShift != 0
	alt, ctrl, super := k.mods&modAlt != 0, k.mods&modCtrl != 0, k.mods&modSuper != 0
	reach := byChar
	switch {
	case super:
		reach = byLine
	case alt:
		reach = byWord
	}
	moved := fieldResult{handled: true}
	before := f.text
	edited := func() fieldResult { return fieldResult{handled: true, edited: f.text != before} }

	switch {
	case k.code == keyLeft:
		f.moveBy(-1, reach, shift)
	case k.code == keyRight:
		f.moveBy(1, reach, shift)
	case k.code == keyHome:
		f.moveTo(0, shift)
	case k.code == keyEnd:
		f.moveTo(len(f.text), shift)
	case ctrl && !alt && !super:
		switch k.code {
		case 'a':
			f.moveTo(0, shift)
		case 'e':
			f.moveTo(len(f.text), shift)
		case 'b':
			f.moveBy(-1, byChar, shift)
		case 'f':
			f.moveBy(1, byChar, shift)
		case 'h':
			f.deleteBy(-1, byChar)
			return edited()
		case 'd':
			f.deleteBy(1, byChar)
			return edited()
		case 'w':
			f.deleteBy(-1, byWord)
			return edited()
		case 'u':
			f.deleteBy(-1, byLine)
			return edited()
		case 'k':
			f.deleteBy(1, byLine)
			return edited()
		default:
			return fieldResult{}
		}
	case alt && !super && (k.code == 'b' || k.code == 'f'):
		// What a terminal with Option as Meta sends for Option-Left and -Right.
		f.moveBy(map[rune]int{'b': -1, 'f': 1}[k.code], byWord, shift)
	case k.code == keyBackspace:
		f.deleteBy(-1, reach)
		return edited()
	case k.code == keyDelete:
		f.deleteBy(1, reach)
		return edited()
	case super:
		switch k.code {
		case 'a':
			f.anchor, f.cur = 0, len(f.text)
		case 'z':
			if shift {
				f.redoEdit()
			} else {
				f.undoEdit()
			}
			return edited()
		case 'c':
			return fieldResult{handled: true, copy: f.selected()}
		case 'x':
			cut := f.selected()
			if cut != "" {
				f.insert("", editDelete)
			}
			r := edited()
			r.copy = cut
			return r
		default:
			return fieldResult{}
		}
	case k.code >= 0x20 && !alt:
		f.insert(string(k.code), editType)
		return edited()
	default:
		return fieldResult{}
	}
	f.last = editNone // moving ends a run of typing
	return moved
}

// paste inserts text, flattened onto one line.
func (f *textField) paste(s string) {
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(s)
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if s != "" {
		f.insert(s, editNone)
	}
}

// moveBy moves the caret a unit left (dir < 0) or right. Without Shift, a
// selection collapses to the side the move heads toward first.
func (f *textField) moveBy(dir int, u unit, extend bool) {
	from := f.cur
	if !extend && f.hasSel() {
		a, b := f.sel()
		from = b
		if dir < 0 {
			from = a
		}
		if u == byChar {
			f.cur, f.anchor = from, from
			return
		}
	}
	f.moveTo(f.boundary(from, dir, u), extend)
}

func (f *textField) moveTo(i int, extend bool) {
	f.cur = i
	if !extend {
		f.anchor = i
	}
}

// deleteBy deletes the selection, or else a unit before (dir < 0) or after the caret.
func (f *textField) deleteBy(dir int, u unit) {
	if f.hasSel() {
		f.insert("", editDelete)
		return
	}
	to := f.boundary(f.cur, dir, u)
	if to == f.cur {
		return
	}
	f.snapshot(editDelete, u == byChar)
	a, b := min(to, f.cur), max(to, f.cur)
	f.text = f.text[:a] + f.text[b:]
	f.cur, f.anchor = a, a
}

// insert replaces the selection with s.
func (f *textField) insert(s string, kind editKind) {
	f.snapshot(kind, kind == editType && !f.hasSel())
	a, b := f.sel()
	f.text = f.text[:a] + s + f.text[b:]
	f.cur = a + len(s)
	f.anchor = f.cur
}

// snapshot saves the state for undo, unless this edit continues a run of
// the same kind (typing, or deleting a character at a time).
func (f *textField) snapshot(kind editKind, joinable bool) {
	if !(joinable && kind != editNone && kind == f.last) {
		f.undo = append(f.undo, fieldSnap{f.text, f.cur, f.anchor})
	}
	f.redo = nil
	// Typing over a selection starts a run; a word or line delete doesn't.
	f.last = kind
	if kind == editDelete && !joinable {
		f.last = editNone
	}
}

func (f *textField) undoEdit() {
	if len(f.undo) == 0 {
		return
	}
	f.redo = append(f.redo, fieldSnap{f.text, f.cur, f.anchor})
	s := f.undo[len(f.undo)-1]
	f.undo = f.undo[:len(f.undo)-1]
	f.text, f.cur, f.anchor, f.last = s.text, s.cur, s.anchor, editNone
}

func (f *textField) redoEdit() {
	if len(f.redo) == 0 {
		return
	}
	f.undo = append(f.undo, fieldSnap{f.text, f.cur, f.anchor})
	s := f.redo[len(f.redo)-1]
	f.redo = f.redo[:len(f.redo)-1]
	f.text, f.cur, f.anchor, f.last = s.text, s.cur, s.anchor, editNone
}

// boundary is the offset a unit from i, left for dir < 0.
func (f *textField) boundary(i, dir int, u unit) int {
	s := f.text
	switch u {
	case byLine:
		if dir < 0 {
			return 0
		}
		return len(s)
	case byWord:
		if dir < 0 {
			return prevWord(s, i)
		}
		return nextWord(s, i)
	}
	if dir < 0 {
		if i <= 0 {
			return 0
		}
		_, n := utf8.DecodeLastRuneInString(s[:i])
		return i - n
	}
	if i >= len(s) {
		return len(s)
	}
	_, n := utf8.DecodeRuneInString(s[i:])
	return i + n
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '\''
}

// prevWord is the start of the word before i, skipping any gap first, as
// Option-Left does.
func prevWord(s string, i int) int {
	for i > 0 {
		r, n := utf8.DecodeLastRuneInString(s[:i])
		if isWordRune(r) {
			break
		}
		i -= n
	}
	for i > 0 {
		r, n := utf8.DecodeLastRuneInString(s[:i])
		if !isWordRune(r) {
			break
		}
		i -= n
	}
	return i
}

// nextWord is the end of the word after i, as Option-Right goes.
func nextWord(s string, i int) int {
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if isWordRune(r) {
			break
		}
		i += n
	}
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if !isWordRune(r) {
			break
		}
		i += n
	}
	return i
}

// The field is real terminal text, so the real cursor can sit in it, with
// whatever shape, blink, and shaders the terminal gives it. These lay it
// out in cells.

// fieldRun is a stretch of the field's visible text drawn one way.
type fieldRun struct {
	text        string
	selected    bool
	placeholder bool
}

// cellWidth is how many terminal cells r takes: 2 for East Asian wide and
// emoji, 0 for combining marks, else 1.
func cellWidth(r rune) int {
	switch {
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d:
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1f64f, r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

func cells(s string) int {
	n := 0
	for _, r := range s {
		n += cellWidth(r)
	}
	return n
}

// layout fits the field into width cells, scrolling as little as keeps the
// caret in view, and returns what to print and the caret's cell. The
// selection shows only while typing; an empty field typing shows "Find".
func (f *textField) layout(width int, input bool) (runs []fieldRun, caret int) {
	if width < 1 {
		return nil, 0
	}
	if f.text == "" {
		f.scrollCol = 0
		if input {
			p := "Find"
			if width < len(p) {
				p = p[:width]
			}
			runs = []fieldRun{{text: p, placeholder: true}}
		}
		return runs, 0
	}
	at := cells(f.text[:f.cur])
	if at-f.scrollCol >= width {
		f.scrollCol = at - width + 1
	}
	if at < f.scrollCol {
		f.scrollCol = at
	}
	f.scrollCol = max(0, min(f.scrollCol, cells(f.text)-width+1))
	a, b := f.sel()
	col := 0
	for i, r := range f.text {
		w := cellWidth(r)
		if col >= f.scrollCol && col+w <= f.scrollCol+width {
			sel := input && i >= a && i < b
			if n := len(runs); n > 0 && runs[n-1].selected == sel {
				runs[n-1].text += string(r)
			} else {
				runs = append(runs, fieldRun{text: string(r), selected: sel})
			}
		}
		col += w
	}
	return runs, at - f.scrollCol
}
