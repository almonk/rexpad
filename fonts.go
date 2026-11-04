package main

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
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

type fontSet struct {
	regular, bold *opentype.Font
	mu            sync.Mutex
	faces         map[faceKey]font.Face
}

type faceKey struct {
	bold bool
	size float64
}

func loadFont(env string, candidates []string, fallback []byte) (*opentype.Font, error) {
	if p := os.Getenv(env); p != "" {
		candidates = append([]string{p}, candidates...)
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if f, err := opentype.Parse(data); err == nil {
			return f, nil
		}
	}
	return opentype.Parse(fallback)
}

func loadFonts() (*fontSet, error) {
	regular, err := loadFont("REXPAD_FONT", regularCandidates, goregular.TTF)
	if err != nil {
		return nil, fmt.Errorf("load regular font: %w", err)
	}
	bold, err := loadFont("REXPAD_FONT_BOLD", boldCandidates, gobold.TTF)
	if err != nil {
		return nil, fmt.Errorf("load bold font: %w", err)
	}
	return &fontSet{regular: regular, bold: bold, faces: map[faceKey]font.Face{}}, nil
}

// face returns a face at size device pixels.
func (fs *fontSet) face(bold bool, size float64) font.Face {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	k := faceKey{bold, size}
	if f, ok := fs.faces[k]; ok {
		return f
	}
	src := fs.regular
	if bold {
		src = fs.bold
	}
	f, err := opentype.NewFace(src, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		panic(err)
	}
	fs.faces[k] = f
	return f
}
