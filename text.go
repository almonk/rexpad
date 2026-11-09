package main

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"github.com/go-text/typesetting/di"
	"github.com/go-text/typesetting/language"
	"github.com/go-text/typesetting/shaping"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// A textRun is a string shaped onto one line: each glyph with its pen
// position from the start of the run, in device pixels.
type textRun struct {
	glyphs []placedGlyph
	width  fixed.Int26_6
}

type placedGlyph struct {
	id   sfnt.GlyphIndex
	x, y fixed.Int26_6
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
	run := fs.shape(s, bold, px, snap)
	fs.runs[key] = run
	return run
}

// shape lays s out with the font's own features: kerning, ligatures, and
// contextual alternates such as Monaspace's texture healing.
func (fs *fontSet) shape(s string, bold bool, px float64, snap bool) textRun {
	if s == "" {
		return textRun{}
	}
	tf := fs.typeface(bold)
	runes := []rune(s)
	out := fs.shaper.Shape(shaping.Input{
		Text:      runes,
		RunEnd:    len(runes),
		Direction: di.DirectionLTR,
		Face:      tf.shaping,
		Size:      fixed.Int26_6(math.Round(px * 64)),
		// Latin has every feature list text uses. Other scripts still get
		// their glyphs, as they did before shaping.
		Script:   language.Latin,
		Language: language.NewLanguage("en"),
	})
	track := fixed.Int26_6(math.Round(tf.tracking * px * 64))
	var run textRun
	for _, g := range out.Glyphs {
		x, advance := run.width+g.XOffset, g.XAdvance+track
		if snap {
			x, advance = fixed.I(x.Round()), fixed.I(advance.Round())
		}
		run.glyphs = append(run.glyphs, placedGlyph{id: sfnt.GlyphIndex(g.GlyphID), x: x, y: -g.YOffset})
		run.width += advance
	}
	return run
}

// drawRun draws run with its pen starting at origin, in device pixels.
func (r *renderer) drawRun(img *image.RGBA, run textRun, origin fixed.Point26_6, bold bool, col color.RGBA) {
	tf := r.fonts.typeface(bold)
	ppem := fixed.Int26_6(math.Round(r.fonts.size * r.scale * 64))
	src := image.NewUniform(col)
	var buf sfnt.Buffer
	for _, g := range run.glyphs {
		segs, err := tf.outlines.LoadGlyph(&buf, g.id, ppem, nil)
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
	ras := vector.NewRasterizer(box.Dx(), box.Dy())
	ras.DrawOp = draw.Src
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
	mask := image.NewAlpha(image.Rect(0, 0, box.Dx(), box.Dy()))
	ras.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	if curve := r.fonts.edge; curve != nil {
		for i, a := range mask.Pix {
			mask.Pix[i] = curve[a]
		}
	}
	draw.DrawMask(img, box, src, image.Point{}, mask, image.Point{}, draw.Over)
}
