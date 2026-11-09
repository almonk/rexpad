package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Items wrap onto as many lines as they need. A list remembers the width it
// was wrapped for, so layout (which has no width of its own) can read each
// item's lines, and wrapping again for the same width is free.

// wrapWidth is how wide an item's lines can be in a row whose content ends
// at right: always leaving room for the working dots after the last line,
// so an item doesn't rewrap when it becomes active.
func wrapWidth(right float64) float64 {
	return right - (marginLeft + textX) - (dotsGap + dotsW)
}

// wrapList wraps every item in l for rows whose content ends at right, in
// points, and reports whether that changed anything.
func (r *renderer) wrapList(l *List, right float64) bool {
	key := fmt.Sprint(right, r.scale)
	if l.wrapKey == key {
		return false
	}
	l.wrapKey = key
	for gi := range l.Groups {
		for ii := range l.Groups[gi].Items {
			it := &l.Groups[gi].Items[ii]
			it.lines = r.wrapText(it.Text, wrapWidth(right))
		}
	}
	return true
}

// wrapText breaks s into lines no wider than maxW points, at spaces where
// it can and mid-word where a word is too long for a line. It returns where
// each line starts, in bytes.
func (r *renderer) wrapText(s string, maxW float64) []int {
	s = strings.ReplaceAll(s, "\t", " ")
	limit := maxW * r.scale
	starts := []int{0}
	if maxW <= 0 || r.measure(s, false) <= limit {
		return starts
	}
	start, brk := 0, -1 // the line's start, and the last place it could break
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		end := i + size
		if i > start && r.measure(strings.TrimRight(s[start:end], " "), false) > limit {
			next := i // mid-word, if the line has no space to break at
			if brk > start {
				next = brk
			}
			for next < len(s) && s[next] == ' ' {
				next++ // a new line doesn't start with spaces
			}
			if next >= len(s) {
				break
			}
			starts = append(starts, next)
			start, brk = next, -1
			if next > i {
				i = next
			}
			continue
		}
		if s[i] == ' ' {
			brk = end
		}
		i = end
	}
	return starts
}

// itemLines is the text of each of it's wrapped lines, with their offsets.
func itemLines(it *Item) (lines []string, starts []int) {
	s := strings.ReplaceAll(it.Text, "\t", " ")
	starts = it.lines
	if len(starts) == 0 {
		starts = []int{0}
	}
	for i, a := range starts {
		b := len(s)
		if i+1 < len(starts) {
			b = starts[i+1]
		}
		lines = append(lines, strings.TrimRight(s[a:b], " "))
	}
	return lines, starts
}
