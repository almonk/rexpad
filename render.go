package main

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Layout constants in points, measured from the reference design (a 2x
// screenshot of a Things-style project list). Every vertical position is
// relative to the center line of the row it belongs to.
const (
	fontSize = 14.0
	// Apple's SF Pro tracking at 14pt is -0.154pt; the static fonts don't
	// apply it on their own.
	tracking = -0.011 // em

	marginLeft  = 16.0
	marginRight = 16.0
	marginTop   = 22.0 // center line of the first group's header
	marginBot   = 16.0

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
	groupGap      = 57.0 // last item center to next header center
	emptyGroupGap = 48.0 // header center to next header center when a group has no items

	boxSize   = 12.0
	boxRadius = 3.0
	boxStroke = 1.0
	boxCenter = 8.5 // checkbox center from the content's left edge

	noteW       = 7.8
	noteH       = 11.0
	noteGap     = 6.0 // text frame end to note icon
	noteTrail   = 8.5 // note icon to the right edge
	noteFold    = 4.0
	noteStroke  = 1.0
	noteCorner  = 1.5
	activeInset = 6.0 // active row highlight extends this far left of the content edge
)

type palette struct {
	bg, title, text, doneText, divider, box, note, accent, activeBg, check color.RGBA
}

var lightPalette = palette{
	bg:       rgb(255, 255, 255),
	title:    rgb(37, 39, 43),
	text:     rgb(18, 20, 23),
	doneText: rgb(150, 153, 158),
	divider:  rgb(230, 232, 236),
	box:      rgb(193, 195, 198),
	note:     rgb(183, 186, 190),
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
	note:     rgb(108, 111, 117),
	accent:   rgb(84, 136, 238),
	activeBg: rgb(37, 49, 72),
	check:    rgb(255, 255, 255),
}

func rgb(r, g, b uint8) color.RGBA { return color.RGBA{r, g, b, 255} }

// themedPalette derives the design's colors from a terminal theme: text in
// the foreground color, grays mixed from foreground toward background in the
// same proportions as the reference, and the theme's blue as the accent.
func themedPalette(fg, bg, accent color.RGBA) palette {
	dark := luminance(bg) < 0.18
	// Fractions of the way from fg to bg, measured on the light reference.
	doneT, dividerT, boxT, noteT := 0.56, 0.895, 0.74, 0.70
	if dark {
		// Dim grays read darker on a dark background; keep them visible.
		doneT, dividerT, boxT, noteT = 0.48, 0.84, 0.62, 0.58
	}
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
		note:     mix(fg, bg, noteT),
		accent:   accent,
		activeBg: mix(accent, bg, 0.84),
		check:    check,
	}
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
	fonts *fontSet
	scale float64
	pal   palette
}

// layout positions every row so the viewer can scroll to the active item.
type rowKind int

const (
	rowHeader rowKind = iota
	rowItem
)

type row struct {
	kind   rowKind
	center float64 // points from the top of the content
	group  *Group
	item   *Item
}

func layout(l *List) (rows []row, height float64) {
	y := marginTop // center of the next group's header
	for gi := range l.Groups {
		g := &l.Groups[gi]
		if g.Title != "" {
			rows = append(rows, row{kind: rowHeader, center: y, group: g})
			if len(g.Items) == 0 {
				y += emptyGroupGap
				continue
			}
			y += firstItemGap
		}
		// An untitled group's items start where its header would be.
		for ii := range g.Items {
			rows = append(rows, row{kind: rowItem, center: y, group: g, item: &g.Items[ii]})
			y += itemPitch
		}
		y += groupGap - itemPitch
	}
	if len(rows) == 0 {
		return nil, 0
	}
	return rows, rows[len(rows)-1].center + itemPitch/2 + marginBot
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
	draw.Draw(img, img.Bounds(), image.NewUniform(r.pal.bg), image.Point{}, draw.Src)

	s := r.scale
	rows, contentH := layout(l)
	if len(rows) == 0 {
		r.text(img, "No tasks yet", marginLeft+textX, marginTop+baselineOffset, false, r.pal.doneText, 0)
		return img
	}
	viewH := float64(heightPx) / s
	offset = math.Max(0, math.Min(offset, contentH-viewH))

	left := marginLeft
	right := float64(widthPx)/s - marginRight
	for _, rw := range rows {
		cy := rw.center - offset
		if cy < -itemPitch || cy > viewH+itemPitch {
			continue
		}
		switch rw.kind {
		case rowHeader:
			r.groupIcon(img, left+iconCenterX, cy, rw.group.progress())
			r.text(img, rw.group.Title, left+titleX, cy+baselineOffset, true, r.pal.title, right-(left+titleX))
			r.fillRect(img, left, cy+dividerOffset, right, cy+dividerOffset+1, r.pal.divider)
		case rowItem:
			r.item(img, rw.item, left, right, cy)
		}
	}
	return img
}

func (r *renderer) item(img *image.RGBA, it *Item, left, right, cy float64) {
	bg := r.pal.bg
	if it.Status == StatusActive {
		bg = r.pal.activeBg
		r.fillRoundRect(img, left-activeInset, cy-itemPitch/2+2, right+activeInset, cy+itemPitch/2-2, 6, bg)
	}

	bx, by := left+boxCenter-boxSize/2, cy-boxSize/2
	switch it.Status {
	case StatusDone:
		r.fillRoundRect(img, bx, by, bx+boxSize, by+boxSize, boxRadius, r.pal.accent)
		r.checkmark(img, bx, by)
	case StatusActive:
		r.fillRoundRect(img, bx, by, bx+boxSize, by+boxSize, boxRadius, r.pal.accent)
		r.fillRoundRect(img, bx+boxStroke, by+boxStroke, bx+boxSize-boxStroke, by+boxSize-boxStroke, boxRadius-boxStroke, bg)
	default:
		r.fillRoundRect(img, bx, by, bx+boxSize, by+boxSize, boxRadius, r.pal.box)
		r.fillRoundRect(img, bx+boxStroke, by+boxStroke, bx+boxSize-boxStroke, by+boxSize-boxStroke, boxRadius-boxStroke, bg)
	}

	col := r.pal.text
	if it.Status == StatusDone {
		col = r.pal.doneText
	}
	tx := left + textX
	maxW := right - tx
	if it.Note != "" {
		maxW -= noteGap + noteW + noteTrail
	}
	w, truncated := r.text(img, it.Text, tx, cy+baselineOffset, false, col, maxW)
	if it.Note != "" {
		nx := tx + w + noteGap
		if truncated {
			nx = tx + maxW + noteGap
		}
		r.noteIcon(img, nx, by+1/r.scale, bg) // one device pixel below the box top reads as centered
	}
}

// --- text ---------------------------------------------------------------

// text draws s with its baseline at (x, y) points, truncating with an
// ellipsis to fit maxW when maxW > 0. It returns the drawn advance in points.
func (r *renderer) text(img *image.RGBA, s string, x, y float64, bold bool, col color.RGBA, maxW float64) (float64, bool) {
	s = strings.ReplaceAll(s, "\t", " ") // fonts have no tab glyph
	face := r.fonts.face(bold, fontSize*r.scale)
	track := tracking * fontSize * r.scale
	truncated := false
	if maxW > 0 && r.measure(face, s, track) > maxW*r.scale {
		truncated = true
		ell := "…"
		for len(s) > 0 {
			_, size := utf8.DecodeLastRuneInString(s)
			s = s[:len(s)-size]
			if r.measure(face, trimRight(s)+ell, track) <= maxW*r.scale {
				break
			}
		}
		s = trimRight(s) + ell
	}
	src := image.NewUniform(col)
	dot := fixed.Point26_6{X: fixed.Int26_6(x * r.scale * 64), Y: fixed.Int26_6(math.Round(y*r.scale) * 64)}
	start := dot.X
	prev := rune(-1)
	for _, c := range s {
		if prev >= 0 {
			dot.X += face.Kern(prev, c)
		}
		dr, mask, mp, adv, ok := face.Glyph(dot, c)
		if ok {
			draw.DrawMask(img, dr, src, image.Point{}, mask, mp, draw.Over)
		}
		dot.X += adv + fixed.Int26_6(track*64)
		prev = c
	}
	return float64(dot.X-start) / 64 / r.scale, truncated
}

func (r *renderer) measure(face font.Face, s string, track float64) float64 {
	var w fixed.Int26_6
	prev := rune(-1)
	for _, c := range s {
		if prev >= 0 {
			w += face.Kern(prev, c)
		}
		adv, _ := face.GlyphAdvance(c)
		w += adv + fixed.Int26_6(track*64)
		prev = c
	}
	return float64(w) / 64
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

func (p *pathBuilder) fill(col color.RGBA) {
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
	ras.Draw(p.img, box, image.NewUniform(col), image.Point{})
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

func (r *renderer) fillCircle(img *image.RGBA, cx, cy, rad float64, col color.RGBA) {
	p := r.path(img)
	p.poly(arc(nil, cx, cy, rad, 0, 2*math.Pi))
	p.fill(col)
}

// groupIcon draws the progress ring with a pie filled clockwise from 12 o'clock.
func (r *renderer) groupIcon(img *image.RGBA, cx, cy, progress float64) {
	r.fillCircle(img, cx, cy, iconRadius, r.pal.accent)
	r.fillCircle(img, cx, cy, iconRadius-iconRingWidth, r.pal.bg)
	switch {
	case progress >= 0.999:
		r.fillCircle(img, cx, cy, iconPieRadius, r.pal.accent)
	case progress > 0.001:
		start := -math.Pi / 2
		pts := [][2]float64{{cx, cy}}
		pts = arc(pts, cx, cy, iconPieRadius, start, start+2*math.Pi*progress)
		p := r.path(img)
		p.poly(pts)
		p.fill(r.pal.accent)
	}
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

// noteIcon draws the dog-eared page shown after items that have notes.
func (r *renderer) noteIcon(img *image.RGBA, x, y float64, bg color.RGBA) {
	x1, y1 := x+noteW, y+noteH
	c := noteCorner
	outer := [][2]float64{{x + noteW - noteFold, y}, {x1, y + noteFold}}
	outer = arc(outer, x1-c, y1-c, c, 0, math.Pi/2)
	outer = arc(outer, x+c, y1-c, c, math.Pi/2, math.Pi)
	outer = arc(outer, x+c, y+c, c, math.Pi, 3*math.Pi/2)
	p := r.path(img)
	p.poly(outer)
	p.fill(r.pal.note)

	st, ic := noteStroke, 0.5
	d := math.Sqrt2 * st
	inner := [][2]float64{{x + noteW - noteFold + st - d, y + st}, {x1 - st, y + noteFold - st + d}}
	inner = arc(inner, x1-st-ic, y1-st-ic, ic, 0, math.Pi/2)
	inner = arc(inner, x+st+ic, y1-st-ic, ic, math.Pi/2, math.Pi)
	inner = arc(inner, x+st+ic, y+st+ic, ic, math.Pi, 3*math.Pi/2)
	p = r.path(img)
	p.poly(inner)
	p.fill(bg)

	// The fold: an L from the top edge down and across to the right edge.
	fx := x + noteW - noteFold - st/2
	fy := y + noteFold + st/2
	r.fillRect(img, fx-st/2, y, fx+st/2, fy+st/2, r.pal.note)
	r.fillRect(img, fx-st/2, fy-st/2, x1, fy+st/2, r.pal.note)
}
