package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"

	gtfont "github.com/go-text/typesetting/font"
	"github.com/go-text/typesetting/shaping"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/sfnt"
)

// The design uses SF Pro Text. macOS ships it as a variable font that
// x/image can't instance, so prefer the static OTFs from Apple's SF Pro
// download (developer.apple.com/fonts), then other sans fonts, then Go's
// bundled fonts so the viewer always draws something.
var (
	regularCandidates = []string{
		"/Library/Fonts/SF-Pro-Text-Regular.otf",
		os.ExpandEnv("$HOME/Library/Fonts/SF-Pro-Text-Regular.otf"),
		"/usr/share/fonts/TTF/Inter-Regular.ttf",
		"/usr/share/fonts/truetype/inter/Inter-Regular.ttf",
		"/usr/share/fonts/inter/Inter-Regular.otf",
	}
	boldCandidates = []string{
		"/Library/Fonts/SF-Pro-Text-Bold.otf",
		os.ExpandEnv("$HOME/Library/Fonts/SF-Pro-Text-Bold.otf"),
		"/usr/share/fonts/TTF/Inter-Bold.ttf",
		"/usr/share/fonts/truetype/inter/Inter-Bold.ttf",
		"/usr/share/fonts/inter/Inter-Bold.otf",
	}
)

const (
	defaultFontSize = 14.0 // points
	// Apple's SF Pro tracking at 14pt is -0.154pt; the static fonts don't
	// apply it on their own. A font the user picks gets none.
	defaultTracking = -0.011 // em
)

// typeface is one font file, parsed once for drawing outlines and once for
// shaping. Both read the same bytes, so they agree on glyph IDs.
type typeface struct {
	outlines *sfnt.Font
	shaping  *gtfont.Face
	tracking float64 // em
}

type fontSet struct {
	regular, bold typeface
	size          float64     // points
	edge          *[256]uint8 // coverage curve that darkens text edges; nil leaves them as drawn

	mu     sync.Mutex
	shaper shaping.HarfbuzzShaper
	runs   map[runKey]textRun
}

func loadTypeface(env string, candidates []string, fallback []byte) (typeface, error) {
	chosen := os.Getenv(env)
	if chosen != "" {
		candidates = append([]string{chosen}, candidates...)
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		tf, err := parseTypeface(data)
		if err != nil {
			continue
		}
		if p == chosen {
			tf.tracking = 0
		}
		return tf, nil
	}
	return parseTypeface(fallback)
}

func parseTypeface(data []byte) (typeface, error) {
	outlines, err := sfnt.Parse(data)
	if err != nil {
		return typeface{}, err
	}
	shapingFace, err := gtfont.ParseTTF(bytes.NewReader(data))
	if err != nil {
		return typeface{}, err
	}
	return typeface{outlines: outlines, shaping: shapingFace, tracking: defaultTracking}, nil
}

func loadFonts() (*fontSet, error) {
	regular, err := loadTypeface("REXPAD_FONT", regularCandidates, goregular.TTF)
	if err != nil {
		return nil, fmt.Errorf("load regular font: %w", err)
	}
	bold, err := loadTypeface("REXPAD_FONT_BOLD", boldCandidates, gobold.TTF)
	if err != nil {
		return nil, fmt.Errorf("load bold font: %w", err)
	}
	return &fontSet{
		regular: regular,
		bold:    bold,
		size:    positiveSetting("REXPAD_FONT_SIZE", defaultFontSize),
		edge:    edgeCurve(positiveSetting("REXPAD_TEXT_GAMMA", 1)),
		runs:    map[runKey]textRun{},
	}, nil
}

// positiveSetting is the number in $key, or fallback when it's unset or not
// a positive number.
func positiveSetting(key string, fallback float64) float64 {
	v, err := strconv.ParseFloat(os.Getenv(key), 64)
	if err != nil || !(v > 0 && v < math.Inf(1)) {
		return fallback
	}
	return v
}

// edgeCurve raises glyph coverage to 1/gamma. A gamma above 1 thickens the
// anti-aliased edges, close to what macOS's "Improve text contrast" does,
// which matters most on a 1x display.
func edgeCurve(gamma float64) *[256]uint8 {
	if gamma == 1 {
		return nil
	}
	var curve [256]uint8
	for i := range curve {
		curve[i] = uint8(math.Round(255 * math.Pow(float64(i)/255, 1/gamma)))
	}
	return &curve
}

func (fs *fontSet) typeface(bold bool) *typeface {
	if bold {
		return &fs.bold
	}
	return &fs.regular
}
