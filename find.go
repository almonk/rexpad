package main

import (
	"fmt"
	"image"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Find: the viewer draws the list as an image, so the terminal's own find
// can't see its text. "/" (or Ctrl-F) opens a find bar at the bottom of the
// pane instead. The query is fuzzy, and while it's set the list collapses to
// the matching items; n / N step through them.

// span is a highlighted byte range of one row's text. current marks the
// spans of the match that n / N are on.
type span struct {
	start, end int
	current    bool
}

// rowMatch is one matching row.
type rowMatch struct {
	row    int      // index into layout(l)'s rows
	spans  [][2]int // the matched characters, as byte ranges
	itemID int      // the item it is, or 0 for a group header
	title  string   // the header's title, when itemID is 0
}

type findState struct {
	open    bool      // the bar is showing
	input   bool      // typing into it
	edit    textField // the bar's text field
	query   string    // what the list is filtered by: the field's text
	vis     []bool    // which of layout's rows show; nil shows all but collapsed groups' items
	matches []rowMatch
	cur     int // index into matches
}

// --- matching -----------------------------------------------------------

type runePos struct {
	r   rune
	off int // byte offset in the string
}

// matchTerm finds term's characters in order in rs, returning their rune
// indices. A contiguous run wins; otherwise it's the shortest window that
// holds them all. Case is ignored unless the term has an uppercase letter.
func matchTerm(s, term string) (idx []int, rs []runePos, ok bool) {
	sensitive := strings.ToLower(term) != term
	fold := func(r rune) rune {
		if sensitive {
			return r
		}
		return unicode.ToLower(r)
	}
	for off, r := range s {
		rs = append(rs, runePos{fold(r), off})
	}
	var q []rune
	for _, r := range term {
		q = append(q, fold(r))
	}
	n := len(q)
	// A plain substring reads best, so try that first.
	for i := 0; i+n <= len(rs); i++ {
		k := 0
		for k < n && rs[i+k].r == q[k] {
			k++
		}
		if k == n {
			for j := i; j < i+n; j++ {
				idx = append(idx, j)
			}
			return idx, rs, true
		}
	}
	// Otherwise the first place the whole term fits, tightened from the end
	// back to the latest start, then read forward again.
	end, j := -1, 0
	for i := range rs {
		if rs[i].r == q[j] {
			if j++; j == n {
				end = i
				break
			}
		}
	}
	if end < 0 {
		return nil, rs, false
	}
	start := end
	for i, j := end, n-1; i >= 0; i-- {
		if rs[i].r == q[j] {
			if j--; j < 0 {
				start = i
				break
			}
		}
	}
	for i, j := start, 0; i <= end && j < n; i++ {
		if rs[i].r == q[j] {
			idx = append(idx, i)
			j++
		}
	}
	return idx, rs, true
}

// fuzzyMatch reports whether every space-separated term of query matches s,
// and the matched characters as merged byte ranges.
func fuzzyMatch(s, query string) ([][2]int, bool) {
	terms := strings.Fields(query)
	if len(terms) == 0 || !utf8.ValidString(query) {
		return nil, false
	}
	var hit []bool
	var rs []runePos
	for _, t := range terms {
		idx, r, ok := matchTerm(s, t)
		if !ok {
			return nil, false
		}
		rs = r
		if hit == nil {
			hit = make([]bool, len(rs))
		}
		for _, i := range idx {
			hit[i] = true
		}
	}
	var out [][2]int
	for i := 0; i < len(rs); i++ {
		if !hit[i] {
			continue
		}
		j := i
		for j+1 < len(rs) && hit[j+1] {
			j++
		}
		end := len(s)
		if j+1 < len(rs) {
			end = rs[j+1].off
		}
		out = append(out, [2]int{rs[i].off, end})
		i = j
	}
	return out, true
}

// rowText is the text a row shows: a group's title or an item's text.
func rowText(rw row) string {
	if rw.kind == rowHeader {
		return rw.group.Title
	}
	return rw.item.Text
}

// filterRows matches query against every row of l's layout. An item shows
// when it matches; a group's header shows when its title or any of its items
// match, so a matching item keeps its group for context.
func filterRows(l *List, query string) (vis []bool, matches []rowMatch) {
	rows, _ := layout(l)
	vis = make([]bool, len(rows))
	spans := make([][][2]int, len(rows))
	hasItems := map[*Group]bool{}
	for i, rw := range rows {
		spans[i], vis[i] = fuzzyMatch(rowText(rw), query)
		if vis[i] && rw.kind == rowItem {
			hasItems[rw.group] = true
		}
	}
	for i, rw := range rows {
		if vis[i] {
			m := rowMatch{row: i, spans: spans[i]}
			if rw.kind == rowItem {
				m.itemID = rw.item.ID
			} else {
				m.title = rw.group.Title
			}
			matches = append(matches, m)
		}
		if rw.kind == rowHeader && hasItems[rw.group] {
			vis[i] = true
		}
	}
	return vis, matches
}

// --- state --------------------------------------------------------------

// setQuery filters the list for q and starts at the first match. A blank
// query shows everything.
func (f *findState) setQuery(l *List, q string) {
	f.query, f.cur = q, 0
	f.vis, f.matches = nil, nil
	if strings.TrimSpace(q) != "" && utf8.ValidString(q) {
		f.vis, f.matches = filterRows(l, q)
	}
}

// refresh re-runs the query after the list changed, staying on the same
// row when it still matches.
func (f *findState) refresh(l *List) {
	was, had := f.current()
	cur := f.cur
	f.setQuery(l, f.query)
	f.cur = max(0, min(cur, len(f.matches)-1))
	for i, m := range f.matches {
		if had && m.itemID == was.itemID && m.title == was.title {
			f.cur = i
		}
	}
}

func (f *findState) step(d int) {
	if n := len(f.matches); n > 0 {
		f.cur = ((f.cur+d)%n + n) % n
	}
}

// current is the match n / N are on, if any.
func (f *findState) current() (rowMatch, bool) {
	if f.cur < len(f.matches) {
		return f.matches[f.cur], true
	}
	return rowMatch{}, false
}

// foldedMatch is the ID of the current match's item when the item's group
// is collapsed, so the item hides once find closes; otherwise 0.
func (f *findState) foldedMatch(l *List) int {
	m, ok := f.current()
	if !ok || m.itemID == 0 {
		return 0
	}
	if g := l.groupOf(m.itemID); g == nil || !g.folded() {
		return 0
	}
	return m.itemID
}

// spans groups the matches by row for the renderer.
func (f *findState) spans() map[int][]span {
	if f == nil || len(f.matches) == 0 {
		return nil
	}
	out := map[int][]span{}
	for i, m := range f.matches {
		for _, s := range m.spans {
			out[m.row] = append(out[m.row], span{s[0], s[1], i == f.cur})
		}
	}
	return out
}

// --- collapsing ---------------------------------------------------------

// rowGeo is where a row is drawn while rows collapse and expand: its center
// in points, and how open it is (0 folded away, 1 fully shown).
type rowGeo struct{ center, open float64 }

// collapseTau sets the pace of the fold; a change settles in about 4 tau.
const collapseTau = 60 * time.Millisecond

// geoFor is the resting geometry of l's rows with only vis showing (when vis
// is nil, all but the items of collapsed groups), and the content height.
func geoFor(l *List, vis []bool) ([]rowGeo, float64) {
	rows, h := layoutVis(l, vis)
	g := make([]rowGeo, len(rows))
	for i, rw := range rows {
		g[i] = rowGeo{rw.center, rw.open}
	}
	return g, h
}

// easeGeo moves every row of g a frame of dt toward to and reports whether
// it has arrived. Only how open each row is eases; where the rows sit
// follows from that, accordion style.
func easeGeo(l *List, g, to []rowGeo, dt time.Duration) bool {
	k := 1 - math.Exp(-float64(dt)/float64(collapseTau))
	open := make([]float64, len(g))
	done := true
	for i := range g {
		o := g[i].open + (to[i].open-g[i].open)*k
		if math.Abs(to[i].open-o) < 0.02 { // a row 2% open is a faint pixel: done
			o = to[i].open
		}
		open[i] = o
		done = done && o == to[i].open
	}
	if done {
		copy(g, to)
		return true
	}
	rows, _ := layoutOpen(l, open)
	for i := range g {
		g[i] = rowGeo{rows[i].center, open[i]}
	}
	return false
}

// ringGeo is where the current match's outline is: the center line of the
// row it's around (in content points), how opaque it is, and how far out
// past the row it reaches while it settles in.
type ringGeo struct{ y, alpha, grow, extra float64 } // extra: the row's wrapped lines

// ringGrow is how far out the outline starts when it appears, like a macOS
// focus ring closing in on its control.
const ringGrow = 5.0

// ringTau paces the outline: quicker than the fold, so it snaps to the
// next match and settles in briskly (about 4 tau, ~140 ms).
const ringTau = 35 * time.Millisecond

// easeRing moves the outline a frame of dt toward to.
func easeRing(g, to ringGeo, dt time.Duration, scale float64) ringGeo {
	return easeRingBy(g, to, dt, scale, ringTau)
}

func easeRingBy(g, to ringGeo, dt time.Duration, scale float64, tau time.Duration) ringGeo {
	k := 1 - math.Exp(-float64(dt)/float64(tau))
	near := func(v, t, eps float64) float64 {
		v += (t - v) * k
		if math.Abs(t-v) < eps {
			return t
		}
		return v
	}
	return ringGeo{
		y:     near(g.y, to.y, 0.5/scale),
		alpha: near(g.alpha, to.alpha, 0.01),
		grow:  near(g.grow, to.grow, 0.5/scale),
		extra: near(g.extra, to.extra, 0.5/scale),
	}
}

// arrivalGeo is where nl's rows start after the list changed from old,
// whose rows were at oldGeo (nil: at rest): a row that was there
// keeps how open it was, and a new one starts folded shut, so it unfolds
// into place. Items are the same by ID, or else by group and text, so
// rewriting a plan with set doesn't replay it; groups are the same by title.
// It returns nil when old had no rows: a first list just appears.
func arrivalGeo(old *List, oldGeo []rowGeo, nl *List) []rowGeo {
	oldRows, _ := layout(old)
	if len(oldRows) == 0 {
		return nil
	}
	if len(oldGeo) != len(oldRows) {
		oldGeo, _ = geoFor(old, nil)
	}
	byID := map[int]float64{}
	byText := map[string][]float64{}
	headers := map[string][]float64{}
	for i, rw := range oldRows {
		o := oldGeo[i].open
		if rw.kind == rowHeader {
			headers[rw.group.Title] = append(headers[rw.group.Title], o)
			continue
		}
		byID[rw.item.ID] = o
		k := rw.group.Title + "\x00" + rw.item.Text
		byText[k] = append(byText[k], o)
	}
	take := func(m map[string][]float64, k string) (float64, bool) {
		if v := m[k]; len(v) > 0 {
			m[k] = v[1:]
			return v[0], true
		}
		return 0, false
	}
	rows, _ := layout(nl)
	open := make([]float64, len(rows))
	for i, rw := range rows {
		if rw.kind == rowHeader {
			open[i], _ = take(headers, rw.group.Title)
			continue
		}
		if o, ok := byID[rw.item.ID]; ok {
			open[i] = o
			k := rw.group.Title + "\x00" + rw.item.Text
			take(byText, k) // so a later item with the same text still counts as new
			continue
		}
		open[i], _ = take(byText, rw.group.Title+"\x00"+rw.item.Text)
	}
	placed, _ := layoutOpen(nl, open)
	g := make([]rowGeo, len(rows))
	for i := range rows {
		g[i] = rowGeo{placed[i].center, open[i]}
	}
	return g
}

// geoHeight is the content height while rows are moving: down to the
// bottom of the lowest row that's still showing.
func geoHeight(g []rowGeo) float64 {
	h := 0.0
	for _, r := range g {
		if r.open > 0 {
			h = math.Max(h, r.center+itemPitch/2*r.open+marginBot)
		}
	}
	return h
}

// status is the count shown at the right of the bar.
func (f *findState) status() string {
	switch {
	case strings.TrimSpace(f.query) == "" || len(f.matches) == 0:
		return "" // the list says when nothing matches
	}
	return fmt.Sprintf("%d of %d", f.cur+1, len(f.matches))
}

// findBarPt is the find bar's target height in points; it's rounded to whole rows.
const findBarPt = 30.0

// findBar draws what's drawn of the bar: the magnifier just left of the
// field, which starts at fieldX, and the match count on the right, both
// centered on the line cy points down. It has no background; the field
// itself is terminal text in the cells beside the magnifier. It returns
// where the count starts, which is where the field has to end.
func (r *renderer) findBar(f *findState, widthPx, heightPx int, cy, fieldX float64) (*image.RGBA, float64) {
	img := image.NewRGBA(image.Rect(0, 0, widthPx, heightPx))
	s := r.scale
	right := float64(widthPx)/s - marginRight
	r.magnifier(img, fieldX-(textX-iconCenterX), cy)
	end := right
	if status := f.status(); status != "" {
		w := r.measure(status, false) / s
		r.text(img, status, right-w, cy+baselineOffset, false, r.pal.doneText, 0)
		end = right - w - 12
	}
	return img, end
}

// magnifier draws a small lens: a ring with a handle toward the lower right.
func (r *renderer) magnifier(img *image.RGBA, cx, cy float64) {
	col := r.pal.box
	lx, ly, rad := cx-1, cy-1, 4.6
	// A ring with a real hole, wound the other way, so the bar stays transparent.
	ring := r.path(img)
	ring.poly(arc(nil, lx, ly, rad, 0, 2*math.Pi))
	ring.poly(arc(nil, lx, ly, rad-1.4, 2*math.Pi, 0))
	ring.fill(col)
	a := math.Pi / 4
	x0, y0 := lx+(rad-0.3)*math.Cos(a), ly+(rad-0.3)*math.Sin(a)
	x1, y1 := x0+3.6*math.Cos(a), y0+3.6*math.Sin(a)
	nx, ny := -math.Sin(a)*0.85, math.Cos(a)*0.85
	p := r.path(img)
	p.poly([][2]float64{{x0 - nx, y0 - ny}, {x1 - nx, y1 - ny}, {x1 + nx, y1 + ny}, {x0 + nx, y0 + ny}})
	p.fill(col)
}
