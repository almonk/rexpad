package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
	"unicode/utf8"

	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Layout constants in points, measured from the reference design (a 2x
// screenshot of a Things-style project list). Every vertical position is
// relative to the center line of the row it belongs to.
const (
	// No padding of its own: the margins are just enough that the active
	// row's highlight (activeInset past the content) meets the pane's edges.
	marginLeft  = activeInset
	marginRight = activeInset
	marginTop   = itemPitch/2 - 2 // center line of the first row
	marginBot   = 0.0

	iconRadius     = 7.75 // group progress ring, outer
	iconRingWidth  = 1.65
	iconPieRadius  = 5.0
	iconCenterX    = 8.0 // from the content's left edge
	textX          = 21.0
	titleX         = 20.5 // bold titles sit half a point left of item text
	baselineOffset = 5.0  // baseline below a row's center line

	dividerOffset = 15.0 // top of the divider below the header center
	firstItemGap  = 38.0 // header center to first item center
	itemPitch     = 28.0
	lineH         = 18.0 // between a wrapped item's lines
	groupGap      = 57.0 // last item center to next header center
	emptyGroupGap = 48.0 // header center to next header center when a group has no items

	boxSize   = 12.0
	boxRadius = 3.0
	boxStroke = 1.0
	boxCenter = 8.5 // checkbox center from the content's left edge

	activeInset = 6.0 // active row highlight extends this far left of the content edge
)

type palette struct {
	bg, title, text, doneText, divider, box, accent, activeBg, check color.RGBA
}

var lightPalette = palette{
	bg:       rgb(255, 255, 255),
	title:    rgb(37, 39, 43),
	text:     rgb(18, 20, 23),
	doneText: rgb(150, 153, 158),
	divider:  rgb(230, 232, 236),
	box:      rgb(193, 195, 198),
	accent:   rgb(49, 95, 189),
	activeBg: rgb(232, 238, 249),
	check:    rgb(255, 255, 255),
}

var darkPalette = palette{
	bg:       rgb(30, 31, 34),
	title:    rgb(236, 237, 239),
	text:     rgb(226, 228, 231),
	doneText: rgb(128, 131, 137),
	divider:  rgb(58, 60, 64),
	box:      rgb(96, 99, 105),
	accent:   rgb(84, 136, 238),
	activeBg: rgb(37, 49, 72),
	check:    rgb(255, 255, 255),
}

func rgb(r, g, b uint8) color.RGBA { return color.RGBA{r, g, b, 255} }

// themedPalette derives the design's colors from a terminal theme: text in
// the foreground color, grays mixed from foreground toward background in the
// same proportions as the reference, and the theme's blue as the accent.
func themedPalette(fg, bg, accent color.RGBA) palette {
	doneT, dividerT, boxT := grayMix(bg)
	check := rgb(255, 255, 255)
	if luminance(accent) > 0.45 {
		check = bg
	}
	return palette{
		bg:       bg,
		title:    fg,
		text:     fg,
		doneText: mix(fg, bg, doneT),
		divider:  mix(fg, bg, dividerT),
		box:      mix(fg, bg, boxT),
		accent:   accent,
		activeBg: mix(accent, bg, 0.84),
		check:    check,
	}
}

// grayMix is how far from the foreground toward the background each gray
// sits: done text, dividers, checkboxes.
func grayMix(bg color.RGBA) (doneT, dividerT, boxT float64) {
	if luminance(bg) < 0.18 {
		// Dim grays read darker on a dark background; keep them visible.
		return 0.48, 0.84, 0.62
	}
	// Measured on the light reference.
	return 0.56, 0.895, 0.74
}

// seeThrough is a themed palette for drawing with no background: each gray
// is the foreground at the opacity that gives the same gray over the
// background, and the active row is the accent at its share, so they blend
// with whatever the pane really shows underneath.
func (p palette) seeThrough() palette {
	doneT, dividerT, boxT := grayMix(p.bg)
	q := p
	q.doneText = premul(p.text, 1-doneT)
	q.divider = premul(p.text, 1-dividerT)
	q.box = premul(p.text, 1-boxT)
	q.activeBg = premul(p.accent, 0.16)
	return q
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	l := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t)) }
	return color.RGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), 255}
}

// luminance is relative luminance in 0..1 (sRGB, linearized).
func luminance(c color.RGBA) float64 {
	lin := func(v uint8) float64 {
		f := float64(v) / 255
		if f <= 0.04045 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

type renderer struct {
	fonts      *fontSet
	scale      float64
	pal        palette
	marks      map[int][]span // find matches to highlight, by row index
	empty      string         // a message over the list, such as when nothing matches
	emptyAlpha float64        // how far it has faded in
	// geo, when it has a row for each of layout's rows, says where each row
	// is and how open it is, for collapsing; geoH is the content height then.
	geo  []rowGeo
	geoH float64
	// glyphRas and glyphMask are reused for every glyph drawn.
	glyphRas  *vector.Rasterizer
	glyphMask image.Alpha
	// ring is the current match's outline, drawn over the rows where it is
	// on its way to; nil draws none.
	ring *ringGeo
	// spins, by item ID, is how far each item has slid over for its
	// working dots while that animates. dots draws the dots too, at
	// dotsPhase seconds in; the viewer leaves them out of the sheet and
	// shows them as their own image, so they can animate cheaply.
	spins     map[int]float64
	dots      bool
	dotsPhase float64
	// icons, by groupKeys, is how far each group's ring is in while it
	// animates; iconKeys is groupKeys of the list being drawn.
	icons    map[string]float64
	iconKeys map[*Group]string
	// clear leaves the background transparent, so the pane's own shows
	// through. Its palette should be seeThrough, and tints are see-through
	// accent instead of mixed colors.
	clear bool
}

// premul is c (opaque) at opacity a, premultiplied as image.RGBA wants.
func premul(c color.RGBA, a float64) color.RGBA {
	a = math.Max(0, math.Min(1, a))
	f := func(v uint8) uint8 { return uint8(math.Round(float64(v) * a)) }
	return color.RGBA{f(c.R), f(c.G), f(c.B), uint8(math.Round(255 * a))}
}

// tint is the accent laid over the background at opacity a: a mixed color
// on a painted background, see-through accent on a clear one. The two look
// the same over the same background.
func (r *renderer) tint(a float64) color.RGBA {
	if r.clear {
		return premul(r.pal.accent, a)
	}
	return mix(r.pal.accent, r.pal.bg, 1-a)
}

func reversed(pts [][2]float64) [][2]float64 {
	out := make([][2]float64, len(pts))
	for i, p := range pts {
		out[len(pts)-1-i] = p
	}
	return out
}

// fillRing fills outer with inner cut out of it: the inner contour runs the
// other way round, which makes a hole, so nothing paints the background.
func (r *renderer) fillRing(img *image.RGBA, outer, inner [][2]float64, col color.RGBA) {
	p := r.path(img)
	p.poly(outer)
	p.poly(reversed(inner))
	p.fill(col)
}

// layout positions every row so the viewer can scroll to the active item.
type rowKind int

const (
	rowHeader rowKind = iota
	rowItem
)

type row struct {
	kind   rowKind
	center float64 // the first line's center, in points from the top of the content
	extra  float64 // how much taller than one line it is: its wrapped lines
	group  *Group
	item   *Item
}

func layout(l *List) (rows []row, height float64) { return layoutVis(l, nil) }

// layoutVis lays out every row, but only the rows in vis take up room (all
// of them when vis is nil).
func layoutVis(l *List, vis []bool) (rows []row, height float64) {
	if vis == nil {
		return layoutOpen(l, nil)
	}
	open := make([]float64, len(vis))
	for i, v := range vis {
		if v {
			open[i] = 1
		}
	}
	return layoutOpen(l, open)
}

// layoutOpen is the layout with each row taking up its share of room: open
// is 1 for a row that shows, 0 for one folded away, and in between while it
// folds (all 1 when open is nil). Rows stack like an accordion, so folding
// rows shrink into their own slots and never overlap their neighbors. With
// only 0s and 1s it's exactly the layout of the rows that show.
func layoutOpen(l *List, open []float64) (rows []row, height float64) {
	w := func(i int) float64 {
		if open == nil {
			return 1
		}
		if i < len(open) {
			return math.Max(0, math.Min(1, open[i]))
		}
		return 0
	}
	y := marginTop // center of the next group's header
	bottom := 0.0  // the lowest edge of anything showing
	for gi := range l.Groups {
		g := &l.Groups[gi]
		head := 1.0
		if g.Title != "" {
			head = w(len(rows))
			// A folding header rises a little into the gap above it.
			rows = append(rows, row{kind: rowHeader, center: y - (1-head)*itemPitch/2, group: g})
			if head > 0 {
				bottom = math.Max(bottom, rows[len(rows)-1].center+(itemPitch/2)*head+marginBot)
			}
		}
		// Items stack from the top of the first item's slot.
		cursor := y + firstItemGap - itemPitch/2
		if g.Title == "" {
			cursor = y - itemPitch/2 // an untitled group's items start where its header would be
		}
		sum, room := 0.0, 0.0 // how many items show, and the room they take
		for ii := range g.Items {
			wi := w(len(rows))
			it := &g.Items[ii]
			ex := it.extra()
			h := itemPitch + ex // a wrapped item is taller by its extra lines
			// Folding, the item stays centered in its shrinking slot.
			rows = append(rows, row{kind: rowItem, center: cursor + wi*h/2 - ex/2, extra: ex, group: g, item: it})
			if wi > 0 {
				bottom = math.Max(bottom, cursor+wi*h+marginBot)
			}
			cursor += wi * h
			sum += wi
			room += wi * h
		}
		// How far the next group starts: the same gaps as the plain layout
		// at whole steps, blended in between.
		some := math.Min(1, sum)
		if g.Title != "" {
			full := firstItemGap - itemPitch + groupGap
			y += head * (emptyGroupGap + some*(full-emptyGroupGap) + room)
		} else {
			y += some*(groupGap-itemPitch) + room
		}
	}
	return rows, bottom
}

// boxHitRight is the right edge of the strip where a click toggles an
// item: the checkbox column, out to halfway between the box and the text.
const boxHitRight = marginLeft + (boxCenter+boxSize/2+textX)/2

// hitItem is the item whose checkbox column holds the point (x, y), in
// content points, or nil. geo, when it has a row for each row, is where
// the rows are drawn while they fold; a click cannot hit a row that is not fully open.
func hitItem(l *List, geo []rowGeo, x, y float64) *Item {
	if x < 0 || x >= boxHitRight {
		return nil
	}
	rows, _ := layout(l)
	if len(geo) != len(rows) {
		geo = nil // Render draws the resting layout then, so the click hits it too
	}
	for i, rw := range rows {
		if rw.kind != rowItem {
			continue
		}
		center := rw.center
		if geo != nil {
			if geo[i].open < 1 {
				continue
			}
			center = geo[i].center
		}
		if y >= center-itemPitch/2 && y < center+itemPitch/2+rw.extra {
			return rw.item
		}
	}
	return nil
}

// followOffset is the scroll offset, in points, that keeps the first active
// item (or else the first open one) in view with some context above it, and
// the largest offset the content allows in a view viewH points tall.
func followOffset(l *List, viewH float64) (offset, maxOffset float64) {
	rows, contentH := layout(l)
	maxOffset = math.Max(0, contentH-viewH)
	if maxOffset == 0 {
		return 0, 0
	}
	focus := -1.0
	for _, rw := range rows {
		if rw.kind == rowItem && rw.item.Status == StatusActive {
			focus = rw.center
			break
		}
	}
	if focus < 0 {
		for _, rw := range rows {
			if rw.kind == rowItem && rw.item.Status != StatusDone {
				focus = rw.center
				break
			}
		}
	}
	if focus < 0 {
		return 0, maxOffset
	}
	return math.Max(0, math.Min(focus-viewH/3, maxOffset)), maxOffset
}

// Render draws the list into an image of exactly widthPx x heightPx device
// pixels, scrolled down by offset points (clamped to the content).
func (r *renderer) Render(l *List, widthPx, heightPx int, offset float64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, widthPx, heightPx))
	if !r.clear {
		draw.Draw(img, img.Bounds(), image.NewUniform(r.pal.bg), image.Point{}, draw.Src)
	}

	s := r.scale
	r.wrapList(l, float64(widthPx)/s-marginRight)
	rows, contentH := layout(l)
	if r.icons != nil {
		r.iconKeys = groupKeys(l)
	}
	geo := r.geo
	if len(geo) != len(rows) {
		geo = nil
	} else {
		contentH = r.geoH
	}
	if len(rows) == 0 {
		r.text(img, "No tasks yet", marginLeft+textX, marginTop+baselineOffset, false, r.pal.doneText, 0)
		return img
	}
	viewH := float64(heightPx) / s
	offset = math.Max(0, math.Min(offset, contentH-viewH))

	left := marginLeft
	right := float64(widthPx)/s - marginRight
	for i, rw := range rows {
		center, open := rw.center, 1.0
		if geo != nil {
			center, open = geo[i].center, geo[i].open
		}
		cy := center - offset
		if open <= 0 || cy < -itemPitch-rw.extra || cy > viewH+itemPitch {
			continue
		}
		if open >= 1 {
			r.row(img, i, rw, left, right, cy)
			continue
		}
		// A row folding open or shut: draw it on its own layer, then show
		// the middle of it, as tall as it is open, faded to match.
		top, bot := cy-itemPitch/2, cy+itemPitch/2+rw.extra
		if rw.kind == rowHeader {
			top, bot = cy-14, cy+dividerOffset+2
		}
		band := image.Rect(0, int(math.Floor(top*s)), widthPx, int(math.Ceil(bot*s)))
		if band.Intersect(img.Bounds()).Empty() {
			continue
		}
		layer := image.NewRGBA(band)
		r.row(layer, i, rw, left, right, cy)
		mid, half := (top+bot)/2, (bot-top)/2*open
		clip := image.Rect(0, int(math.Round((mid-half)*s)), widthPx, int(math.Round((mid+half)*s))).Intersect(img.Bounds())
		fade := image.NewUniform(color.Alpha{uint8(math.Round(255 * open * open))})
		draw.DrawMask(img, clip, layer, clip.Min, fade, image.Point{}, draw.Over)
	}
	if g := r.ring; g != nil && g.alpha > 0 {
		r.outline(img, left, right, g.y-offset, g.extra, g.grow, g.alpha)
	}
	if r.empty != "" && r.emptyAlpha > 0 {
		col := premul(r.pal.doneText, r.emptyAlpha)
		r.text(img, r.empty, marginLeft+textX, marginTop+baselineOffset-offset, false, col, 0)
	}
	return img
}

// row draws one row of the list with its center line at cy.
func (r *renderer) row(img *image.RGBA, i int, rw row, left, right, cy float64) {
	switch rw.kind {
	case rowHeader:
		// A group with nothing done has no ring, and its title moves into
		// the ring's place; the ring grows in as progress starts.
		// It grows rightward from its left edge, pushing the title over, so
		// the two never overlap.
		show := r.iconShow(rw.group)
		edge := left + iconCenterX - iconRadius
		if show > 0 {
			r.groupIcon(img, edge+iconRadius*show, cy, rw.group.progress(), show)
		}
		tx := edge + 0.25 + show*(left+titleX-edge-0.25)
		r.textMarked(img, rw.group.Title, tx, cy+baselineOffset, true, r.pal.title, right-tx, r.marks[i])
		r.fillRect(img, left, cy+dividerOffset, right, cy+dividerOffset+1, r.pal.divider)
	case rowItem:
		r.item(img, rw.item, left, right, cy, r.marks[i])
	}
}

func (r *renderer) item(img *image.RGBA, it *Item, left, right, cy float64, marks []span) {
	ex := it.extra()
	if it.Status == StatusActive {
		r.fillRoundRect(img, left-activeInset, cy-itemPitch/2+2, right+activeInset, cy+itemPitch/2-2+ex, 6, r.pal.activeBg)
	}

	bx, by := left+boxCenter-boxSize/2, cy-boxSize/2
	box := roundRect(bx, by, bx+boxSize, by+boxSize, boxRadius)
	hole := roundRect(bx+boxStroke, by+boxStroke, bx+boxSize-boxStroke, by+boxSize-boxStroke, boxRadius-boxStroke)
	switch it.Status {
	case StatusDone:
		r.fillRoundRect(img, bx, by, bx+boxSize, by+boxSize, boxRadius, r.pal.accent)
		r.checkmark(img, bx, by)
	case StatusActive:
		r.fillRing(img, box, hole, r.pal.accent)
	default:
		r.fillRing(img, box, hole, r.pal.box)
	}

	col := r.pal.text
	if it.Status == StatusDone {
		col = r.pal.doneText
	}
	tx := left + textX
	// The wrapped lines, each with the find highlights that fall in it.
	lines, starts := itemLines(it)
	for i, line := range lines {
		a := starts[i]
		var lm []span
		for _, m := range marks {
			s, e := max(m.start, a), min(m.end, a+len(line))
			if s < e {
				lm = append(lm, span{s - a, e - a, m.current})
			}
		}
		r.textMarked(img, line, tx, cy+baselineOffset+float64(i)*lineH, false, col, 0, lm)
	}
	// The working dots follow the last line.
	if show := r.spinShow(it); show > 0 && r.dots {
		r.workingDots(img, r.dotsX(it, right, show), cy+ex, r.dotsPhase, show)
	}
}

// --- text ---------------------------------------------------------------

// text draws s with its baseline at (x, y) points, truncating with an
// ellipsis to fit maxW when maxW > 0. It returns the drawn advance in points.
func (r *renderer) text(img *image.RGBA, s string, x, y float64, bold bool, col color.RGBA, maxW float64) (float64, bool) {
	return r.textMarked(img, s, x, y, bold, col, maxW, nil)
}

// textMarked is text with find matches highlighted behind it. marks are
// byte ranges of s; a match cut off by the ellipsis highlights the ellipsis.
// (The current match is marked by an outline around its row, not here.)
func (r *renderer) textMarked(img *image.RGBA, s string, x, y float64, bold bool, col color.RGBA, maxW float64, marks []span) (float64, bool) {
	s, kept, truncated := r.fit(s, bold, maxW)
	run := r.layout(s, bold)

	// Highlights, mapped onto the drawn string, filled as one path so
	// neighbors that touch don't darken where they overlap.
	hl := r.path(img)
	for _, m := range marks {
		a, b := min(m.start, kept), min(m.end, kept)
		if truncated && m.end > kept {
			b = len(s) // runs into the ellipsis
		}
		if a >= b {
			continue
		}
		x0 := x + r.xAt(s, run, a)/r.scale - 1
		x1 := x + r.xAt(s, run, b)/r.scale + 1
		cy := y - baselineOffset
		hl.poly(roundRect(x0, cy-9, x1, cy+9, 3))
	}
	hl.fill(r.tint(0.38))

	origin := fixed.Point26_6{X: fixed.Int26_6(x * r.scale * 64), Y: fixed.Int26_6(math.Round(y*r.scale) * 64)}
	r.drawRun(img, run, origin, col)
	return float64(run.width) / 64 / r.scale, truncated
}

// fit is s as drawn in maxW points (no limit when maxW <= 0): cut with an
// ellipsis if it's too long. kept is how many bytes of s come before the
// ellipsis.
func (r *renderer) fit(s string, bold bool, maxW float64) (shown string, kept int, truncated bool) {
	s = strings.ReplaceAll(s, "\t", " ") // fonts have no tab glyph
	run := r.layout(s, bold)
	if maxW <= 0 || float64(run.width)/64 <= maxW*r.scale {
		return s, len(s), false
	}
	s = s[:r.longestCandidate(s, run, maxW*r.scale)]
	const ell = "…"
	for len(s) > 0 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
		if r.measure(trimRight(s)+ell, bold) <= maxW*r.scale {
			break
		}
	}
	s = trimRight(s)
	return s + ell, len(s), true
}

// longestCandidate is where fit starts cutting s, in bytes. Once a rune
// starts past limit, every longer prefix is too wide, so fit only needs to
// try the prefixes up to that rune instead of shaping one per rune from the
// end. It keeps that rune too, because fit drops a rune before each try.
func (r *renderer) longestCandidate(s string, run textRun, limit float64) int {
	for _, g := range run.glyphs {
		if float64(g.x)/64 < limit {
			continue
		}
		cut := runeOffset(s, g.cluster)
		_, size := utf8.DecodeRuneInString(s[cut:])
		return cut + size
	}
	return len(s)
}

// runeOffset is the byte offset of rune n in s.
func runeOffset(s string, n int) int {
	for i := range s {
		if n == 0 {
			return i
		}
		n--
	}
	return len(s)
}

// textWidth is how wide fit's text is drawn, in points.
func (r *renderer) textWidth(s string, bold bool, maxW float64) float64 {
	shown, _, _ := r.fit(s, bold, maxW)
	return r.measure(shown, bold) / r.scale
}

func trimRight(s string) string {
	for len(s) > 0 {
		c, size := utf8.DecodeLastRuneInString(s)
		if c != ' ' && c != ',' && c != '.' && c != ';' && c != ':' && c != '-' {
			break
		}
		s = s[:len(s)-size]
	}
	return s
}

// --- shapes -------------------------------------------------------------

// pathBuilder collects polygons in points and fills them at the renderer's
// scale. The rasterizer covers only the polygons' bounding box: one the size
// of the whole image made every 12pt checkbox cost a full-image pass.
type pathBuilder struct {
	r     *renderer
	img   *image.RGBA
	polys [][][2]float32 // in device pixels
}

func (r *renderer) path(img *image.RGBA) *pathBuilder {
	return &pathBuilder{r: r, img: img}
}

func (p *pathBuilder) poly(pts [][2]float64) {
	s := p.r.scale
	px := make([][2]float32, len(pts))
	for i, pt := range pts {
		px[i] = [2]float32{float32(pt[0] * s), float32(pt[1] * s)}
	}
	p.polys = append(p.polys, px)
}

func (p *pathBuilder) fill(col color.RGBA) { p.fillWith(image.NewUniform(col)) }

func (p *pathBuilder) fillWith(src image.Image) {
	if len(p.polys) == 0 {
		return
	}
	minX, minY := float32(math.Inf(1)), float32(math.Inf(1))
	maxX, maxY := float32(math.Inf(-1)), float32(math.Inf(-1))
	for _, poly := range p.polys {
		for _, pt := range poly {
			minX, maxX = min(minX, pt[0]), max(maxX, pt[0])
			minY, maxY = min(minY, pt[1]), max(maxY, pt[1])
		}
	}
	box := image.Rect(
		int(math.Floor(float64(minX))), int(math.Floor(float64(minY))),
		int(math.Ceil(float64(maxX))), int(math.Ceil(float64(maxY))),
	).Intersect(p.img.Bounds())
	if box.Empty() {
		return
	}
	// The rasterizer's origin lands on box.Min in the image.
	ras := vector.NewRasterizer(box.Dx(), box.Dy())
	ox, oy := float32(box.Min.X), float32(box.Min.Y)
	for _, poly := range p.polys {
		ras.MoveTo(poly[0][0]-ox, poly[0][1]-oy)
		for _, pt := range poly[1:] {
			ras.LineTo(pt[0]-ox, pt[1]-oy)
		}
		ras.ClosePath()
	}
	ras.Draw(p.img, box, src, image.Point{})
}

// arc appends points along a clockwise arc (screen coordinates) from a0 to a1 radians.
func arc(pts [][2]float64, cx, cy, rad, a0, a1 float64) [][2]float64 {
	n := int(math.Max(4, math.Ceil(math.Abs(a1-a0)*rad*4)))
	for i := 0; i <= n; i++ {
		a := a0 + (a1-a0)*float64(i)/float64(n)
		pts = append(pts, [2]float64{cx + rad*math.Cos(a), cy + rad*math.Sin(a)})
	}
	return pts
}

func roundRect(x0, y0, x1, y1, rad float64) [][2]float64 {
	rad = math.Max(0, math.Min(rad, math.Min(x1-x0, y1-y0)/2))
	var pts [][2]float64
	pts = arc(pts, x1-rad, y0+rad, rad, -math.Pi/2, 0)
	pts = arc(pts, x1-rad, y1-rad, rad, 0, math.Pi/2)
	pts = arc(pts, x0+rad, y1-rad, rad, math.Pi/2, math.Pi)
	pts = arc(pts, x0+rad, y0+rad, rad, math.Pi, 3*math.Pi/2)
	return pts
}

func (r *renderer) fillRoundRect(img *image.RGBA, x0, y0, x1, y1, rad float64, col color.RGBA) {
	p := r.path(img)
	p.poly(roundRect(x0, y0, x1, y1, rad))
	p.fill(col)
}

func (r *renderer) fillRect(img *image.RGBA, x0, y0, x1, y1 float64, col color.RGBA) {
	p := r.path(img)
	p.poly([][2]float64{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}})
	p.fill(col)
}

// outline rings the row centered at cy in the accent: the find match that
// n / N are on. grow points it out past the row on every side, and alpha
// fades it, for when it settles in.
func (r *renderer) outline(img *image.RGBA, left, right, cy, extra, grow, alpha float64) {
	const w = 1.5
	x0, y0 := left-activeInset-grow, cy-itemPitch/2+2-grow
	x1, y1 := right+activeInset+grow, cy+itemPitch/2-2+extra+grow
	inner := roundRect(x0+w, y0+w, x1-w, y1-w, 6+grow-w)
	for i, j := 0, len(inner)-1; i < j; i, j = i+1, j-1 {
		inner[i], inner[j] = inner[j], inner[i] // the other way round makes a hole
	}
	p := r.path(img)
	p.poly(roundRect(x0, y0, x1, y1, 6+grow))
	p.poly(inner)
	a := r.pal.accent
	p.fillWith(image.NewUniform(color.NRGBA{a.R, a.G, a.B, uint8(math.Round(255 * math.Max(0, math.Min(1, alpha))))}))
}

func (r *renderer) fillCircle(img *image.RGBA, cx, cy, rad float64, col color.RGBA) {
	p := r.path(img)
	p.poly(arc(nil, cx, cy, rad, 0, 2*math.Pi))
	p.fill(col)
}

// groupIcon draws the progress ring with a pie filled clockwise from 12
// o'clock. show (0 to 1) grows and fades it in.
func (r *renderer) groupIcon(img *image.RGBA, cx, cy, progress, show float64) {
	k := show // it grows from nothing
	a := math.Min(1, show*1.5)
	col := premul(r.pal.accent, a)
	if !r.clear && a < 1 {
		col = mix(r.pal.accent, r.pal.bg, 1-a)
	}
	r.fillRing(img, arc(nil, cx, cy, iconRadius*k, 0, 2*math.Pi), arc(nil, cx, cy, (iconRadius-iconRingWidth)*k, 0, 2*math.Pi), col)
	switch {
	case progress >= 0.999:
		r.fillCircle(img, cx, cy, iconPieRadius*k, col)
	case progress > 0.001:
		start := -math.Pi / 2
		pts := [][2]float64{{cx, cy}}
		pts = arc(pts, cx, cy, iconPieRadius*k, start, start+2*math.Pi*progress)
		p := r.path(img)
		p.poly(pts)
		p.fill(col)
	}
}

// groupKeys names each group so a group's ring can be followed across
// reloads: its title, and which of the groups with that title it is.
func groupKeys(l *List) map[*Group]string {
	keys := map[*Group]string{}
	seen := map[string]int{}
	for gi := range l.Groups {
		g := &l.Groups[gi]
		keys[g] = fmt.Sprint(g.Title, "\x00", seen[g.Title])
		seen[g.Title]++
	}
	return keys
}

// iconShow is how far g's ring is in: from r.icons while it animates,
// else 1 once anything in the group is under way and 0 before.
func (r *renderer) iconShow(g *Group) float64 {
	if r.icons != nil {
		if r.iconKeys == nil {
			r.iconKeys = map[*Group]string{}
		}
		if v, ok := r.icons[r.iconKeys[g]]; ok && r.iconKeys[g] != "" {
			return v
		}
	}
	if g.progress() > 0 {
		return 1
	}
	return 0
}

func (r *renderer) checkmark(img *image.RGBA, bx, by float64) {
	// A stroked polyline as a polygon: short leg then long leg.
	pts := [][2]float64{{bx + 3.0, by + 6.2}, {bx + 5.1, by + 8.3}, {bx + 9.0, by + 3.8}}
	const w = 0.75 // half stroke width
	p := r.path(img)
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		dx, dy := b[0]-a[0], b[1]-a[1]
		l := math.Hypot(dx, dy)
		nx, ny := -dy/l*w, dx/l*w
		ex, ey := dx/l*w*0.6, dy/l*w*0.6
		p.poly([][2]float64{
			{a[0] - nx - ex, a[1] - ny - ey}, {b[0] - nx + ex, b[1] - ny + ey},
			{b[0] + nx + ex, b[1] + ny + ey}, {a[0] + nx - ex, a[1] + ny - ey},
		})
	}
	p.fill(r.pal.check)
}
