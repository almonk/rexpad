package main

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"unicode/utf8"

	"github.com/go-text/typesetting/di"
	"github.com/go-text/typesetting/language"
	"github.com/go-text/typesetting/shaping"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// A textRun is a string shaped onto one line in one typeface: each glyph with
// its pen position from the start of the run, in device pixels.
type textRun struct {
	face   *typeface
	glyphs []placedGlyph
	width  fixed.Int26_6
}

type placedGlyph struct {
	id      sfnt.GlyphIndex
	x, y    fixed.Int26_6
	cluster int // index of the first rune this glyph draws
}

type runKey struct {
	s    string
	bold bool
	px   float64
	snap bool
}

// Shaping is the slow part of drawing text, and most strings are drawn
// again on every frame, so runs are kept. The cap only bounds a long session.
const maxRuns = 4096

// layout shapes s at the renderer's scale.
func (r *renderer) layout(s string, bold bool) textRun {
	return r.fonts.run(s, bold, r.fonts.size*r.scale, r.snap())
}

// measure is how wide s is drawn, in device pixels.
func (r *renderer) measure(s string, bold bool) float64 {
	return float64(r.layout(s, bold).width) / 64
}

// xAt is where the text at byte offset i of s starts in run, s's layout, in
// device pixels.
func (r *renderer) xAt(s string, run textRun, i int) float64 {
	n := utf8.RuneCountInString(s[:i])
	for _, g := range run.glyphs {
		if g.cluster >= n {
			return float64(g.x) / 64
		}
	}
	return float64(run.width) / 64
}

// snap puts every glyph on a whole pixel. Below 2x a glyph that starts
// between pixels comes out blurred, and each copy of a letter blurs
// differently. At 2x and up the design's fractional spacing reads better.
func (r *renderer) snap() bool { return r.scale < 2 }

func (fs *fontSet) run(s string, bold bool, px float64, snap bool) textRun {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	key := runKey{s, bold, px, snap}
	if run, ok := fs.runs[key]; ok {
		return run
	}
	if len(fs.runs) >= maxRuns {
		clear(fs.runs)
	}
	run := fs.shape(s, fs.typeface(bold), px, snap)
	fs.runs[key] = run
	return run
}

// shape lays s out with the font's own features: kerning, ligatures, and
// contextual alternates such as Monaspace's texture healing. When snapping,
// each advance rounds to whole pixels, so a monospace font keeps an even
// grid like a terminal's, though lines can come out a little wider.
func (fs *fontSet) shape(s string, tf *typeface, px float64, snap bool) textRun {
	run := textRun{face: tf}
	if s == "" {
		return run
	}
	runes := []rune(s)
	out := fs.shaper.ShapeNoExtents(shaping.Input{
		Text:      runes,
		RunEnd:    len(runes),
		Direction: di.DirectionLTR,
		Face:      tf.shapingFace,
		Size:      fixed.Int26_6(math.Round(px * 64)),
		// Latin has every feature list text uses. Other scripts draw the
		// glyphs the font has, as they did before shaping.
		Script:   language.Latin,
		Language: language.NewLanguage("en"),
	})
	track := fixed.Int26_6(math.Round(tf.tracking * px * 64))
	run.glyphs = make([]placedGlyph, 0, len(out.Glyphs))
	for _, g := range out.Glyphs {
		x, advance := run.width+g.XOffset, g.Advance+track
		if snap {
			x, advance = fixed.I(x.Round()), fixed.I(advance.Round())
		}
		run.glyphs = append(run.glyphs, placedGlyph{id: sfnt.GlyphIndex(g.GlyphID), x: x, y: -g.YOffset, cluster: g.ClusterIndex})
		run.width += advance
	}
	return run
}

// drawRun draws run with its pen starting at origin, in device pixels.
func (r *renderer) drawRun(img *image.RGBA, run textRun, origin fixed.Point26_6, col color.RGBA) {
	if r.snap() {
		origin.X = fixed.I(origin.X.Round())
	}
	ppem := fixed.Int26_6(math.Round(r.fonts.size * r.scale * 64))
	src := image.NewUniform(col)
	var buf sfnt.Buffer
	for _, g := range run.glyphs {
		segs, err := run.face.outlines.LoadGlyph(&buf, g.id, ppem, nil)
		if err != nil || len(segs) == 0 {
			continue // a space has no outline
		}
		r.fillGlyph(img, segs, origin.Add(fixed.Point26_6{X: g.x, Y: g.y}), src)
	}
}

func (r *renderer) fillGlyph(img *image.RGBA, segs sfnt.Segments, at fixed.Point26_6, src image.Image) {
	b := segs.Bounds().Add(at)
	box := image.Rect(b.Min.X.Floor(), b.Min.Y.Floor(), b.Max.X.Ceil(), b.Max.Y.Ceil())
	if box.Empty() {
		return
	}
	px := func(p fixed.Point26_6) (float32, float32) {
		return float32(at.X+p.X)/64 - float32(box.Min.X), float32(at.Y+p.Y)/64 - float32(box.Min.Y)
	}
	ras, mask := r.glyphBuffers(box.Dx(), box.Dy())
	for _, seg := range segs {
		switch seg.Op {
		case sfnt.SegmentOpMoveTo:
			ras.MoveTo(px(seg.Args[0]))
		case sfnt.SegmentOpLineTo:
			ras.LineTo(px(seg.Args[0]))
		case sfnt.SegmentOpQuadTo:
			ax, ay := px(seg.Args[0])
			bx, by := px(seg.Args[1])
			ras.QuadTo(ax, ay, bx, by)
		case sfnt.SegmentOpCubeTo:
			ax, ay := px(seg.Args[0])
			bx, by := px(seg.Args[1])
			cx, cy := px(seg.Args[2])
			ras.CubeTo(ax, ay, bx, by, cx, cy)
		}
	}
	ras.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	if curve := r.fonts.edge; curve != nil {
		for i, a := range mask.Pix {
			mask.Pix[i] = curve[a]
		}
	}
	draw.DrawMask(img, box, src, image.Point{}, mask, image.Point{}, draw.Over)
}

// glyphBuffers are the rasterizer and mask for one w×h glyph. They're reused
// from glyph to glyph, so a frame doesn't allocate for every letter.
func (r *renderer) glyphBuffers(w, h int) (*vector.Rasterizer, *image.Alpha) {
	if r.glyphRas == nil {
		r.glyphRas = vector.NewRasterizer(w, h)
	} else {
		r.glyphRas.Reset(w, h)
	}
	r.glyphRas.DrawOp = draw.Src
	if cap(r.glyphMask.Pix) < w*h {
		r.glyphMask.Pix = make([]uint8, w*h)
	}
	r.glyphMask = image.Alpha{Pix: r.glyphMask.Pix[:w*h], Stride: w, Rect: image.Rect(0, 0, w, h)}
	return r.glyphRas, &r.glyphMask
}
