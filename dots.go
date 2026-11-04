package main

import (
	"image"
	"math"
)

// The working dots: Rex's three-dot spinner (TabStatusIndicatorView in
// rex-app), shown after the active item's text. Each dot rests dim and
// lights up in turn.
const (
	dotDiameter = 3.5
	dotSpacing  = 2.0
	dotsW       = 3*dotDiameter + 2*dotSpacing
	dotsGap     = 6.0 // text to the dots
	dotResting  = 0.3
	dotLit      = 0.85
	dotCycle    = 1.2 // seconds, first dot lighting to the cycle repeating
	dotStagger  = 0.2 // seconds each dot lights after the one before
	dotsFPS     = 15  // Rex runs them at 8 to 15 frames a second
)

// dotOpacity is dot i's opacity at phase seconds into the animation: up to
// lit over the first quarter of its cycle, back down over the second, then
// resting, each move eased in and out.
func dotOpacity(phase float64, i int) float64 {
	t := math.Mod((phase-float64(i)*dotStagger)/dotCycle, 1)
	if t < 0 {
		t++
	}
	ease := func(u float64) float64 { return u * u * (3 - 2*u) }
	switch {
	case t < 0.25:
		return dotResting + (dotLit-dotResting)*ease(t/0.25)
	case t < 0.5:
		return dotLit + (dotResting-dotLit)*ease((t-0.25)/0.25)
	}
	return dotResting
}

// workingDots draws the three dots with the first one's left edge at x and
// their centers on cy, at phase seconds in, faded by alpha.
func (r *renderer) workingDots(img *image.RGBA, x, cy, phase, alpha float64) {
	for i := 0; i < 3; i++ {
		a := dotOpacity(phase, i) * alpha
		col := premul(r.pal.text, a)
		if !r.clear {
			col = mix(r.pal.text, r.pal.bg, 1-a)
		}
		cx := x + dotDiameter/2 + float64(i)*(dotDiameter+dotSpacing)
		r.fillCircle(img, cx, cy, dotDiameter/2, col)
	}
}

// dotsX is where its working dots start, in points: just after its text,
// as drawn in a row whose content ends at right.
func (r *renderer) dotsX(it *Item, right, show float64) float64 {
	tx := marginLeft + textX
	return tx + r.textWidth(it.Text, false, right-tx-show*(dotsGap+dotsW)) + dotsGap
}

// spinShow is how far its working dots are in: from r.spins while they
// fade, else 1 for an active item and 0 for others.
func (r *renderer) spinShow(it *Item) float64 {
	if r.spins != nil {
		if v, ok := r.spins[it.ID]; ok {
			return v
		}
	}
	if it.Status == StatusActive {
		return 1
	}
	return 0
}
