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
	// The layout's line height and baseline fit text from 10pt to 16pt.
	minFontSize, maxFontSize   = 10.0, 16.0
	minTextGamma, maxTextGamma = 0.5, 3.0
	// Apple's SF Pro tracking at 14pt is -0.154pt; the static fonts don't
	// apply it on their own. A font the user picks gets none.
	defaultTracking = -0.011 // em
)

// typeface is one font file, parsed once for drawing outlines and once for
// shaping. Both read the same bytes, so they agree on glyph IDs.
type typeface struct {
	outlines    *sfnt.Font
	shapingFace *gtfont.Face
	tracking    float64 // em
}

type fontSet struct {
	regular, bold typeface
	size          float64     // points
	edge          *[256]uint8 // coverage curve that darkens text edges; nil leaves them as drawn
	warnings      []string    // why a font the user named is not in use

	mu     sync.Mutex
	shaper shaping.HarfbuzzShaper
	runs   map[runKey]textRun
}

// loadTypeface loads the font named in $env, or else the first candidate
// that loads, or else fallback. warning says why the named font was not used.
func loadTypeface(env string, candidates []string, fallback []byte) (tf typeface, warning string, err error) {
	if chosen := os.Getenv(env); chosen != "" {
		tf, err := readTypeface(chosen)
		if err == nil {
			tf.tracking = 0
			return tf, "", nil
		}
		warning = fmt.Sprintf("$%s %s: %v; using the default font", env, chosen, err)
	}
	for _, p := range candidates {
		if tf, err := readTypeface(p); err == nil {
			return tf, warning, nil
		}
	}
	tf, err = parseTypeface(fallback)
	return tf, warning, err
}

func readTypeface(path string) (typeface, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return typeface{}, err
	}
	return parseTypeface(data)
}

// parseTypeface reads a font file, or the first font of a collection (.ttc),
// which is how macOS ships Menlo and Courier.
func parseTypeface(data []byte) (typeface, error) {
	if bytes.HasPrefix(data, []byte("ttcf")) {
		return parseCollection(data)
	}
	outlines, err := sfnt.Parse(data)
	if err != nil {
		return typeface{}, err
	}
	shapingFace, err := gtfont.ParseTTF(bytes.NewReader(data))
	if err != nil {
		return typeface{}, err
	}
	return typeface{outlines: outlines, shapingFace: shapingFace, tracking: defaultTracking}, nil
}

func parseCollection(data []byte) (typeface, error) {
	collection, err := sfnt.ParseCollection(data)
	if err != nil {
		return typeface{}, err
	}
	outlines, err := collection.Font(0)
	if err != nil {
		return typeface{}, err
	}
	shapingFaces, err := gtfont.ParseTTC(bytes.NewReader(data))
	if err != nil {
		return typeface{}, err
	}
	return typeface{outlines: outlines, shapingFace: shapingFaces[0], tracking: defaultTracking}, nil
}

func loadFonts() (*fontSet, error) {
	regular, regularWarning, err := loadTypeface("REXPAD_FONT", regularCandidates, goregular.TTF)
	if err != nil {
		return nil, fmt.Errorf("load regular font: %w", err)
	}
	bold, boldWarning, err := loadTypeface("REXPAD_FONT_BOLD", boldCandidates, gobold.TTF)
	if err != nil {
		return nil, fmt.Errorf("load bold font: %w", err)
	}
	fonts := &fontSet{
		regular: regular,
		bold:    bold,
		size:    boundedSetting("REXPAD_FONT_SIZE", defaultFontSize, minFontSize, maxFontSize),
		edge:    edgeCurve(boundedSetting("REXPAD_TEXT_GAMMA", 1, minTextGamma, maxTextGamma)),
		runs:    map[runKey]textRun{},
	}
	for _, w := range []string{regularWarning, boldWarning} {
		if w != "" {
			fonts.warnings = append(fonts.warnings, w)
		}
	}
	return fonts, nil
}

// boundedSetting is the number in $key, or fallback when it's unset, not a
// number, or outside [min, max].
func boundedSetting(key string, fallback, min, max float64) float64 {
	v, err := strconv.ParseFloat(os.Getenv(key), 64)
	if err != nil || !(v >= min && v <= max) {
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

func printFontWarnings(fonts *fontSet) {
	for _, w := range fonts.warnings {
		fmt.Fprintln(os.Stderr, "rexpad:", w)
	}
}
