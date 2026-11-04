package main

import (
	"bufio"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `# Investigate the crash
- [ ] Read the crash log
  From the beta tester report

# Fix
- [x] Reproduce it locally
- [~] Guard the missing session {#7}
- Add a regression test
`

func TestParseMarkdown(t *testing.T) {
	l, err := parseMarkdown(sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Groups) != 2 || l.Groups[0].Title != "Investigate the crash" {
		t.Fatalf("groups = %+v", l.Groups)
	}
	if got := l.Groups[0].Items[0].Note; got != "From the beta tester report" {
		t.Errorf("note = %q", got)
	}
	ext := l.Groups[1].Items
	want := []Status{StatusDone, StatusActive, StatusPending}
	for i, st := range want {
		if ext[i].Status != st {
			t.Errorf("item %d status = %s, want %s", i, ext[i].Status, st)
		}
	}
	if ext[1].ID != 7 || ext[1].Text != "Guard the missing session" {
		t.Errorf("explicit ID item = %+v", ext[1])
	}
	seen := map[int]bool{}
	for _, g := range l.Groups {
		for _, it := range g.Items {
			if it.ID == 0 || seen[it.ID] {
				t.Errorf("bad or duplicate ID %d", it.ID)
			}
			seen[it.ID] = true
		}
	}
	if l.NextID <= 7 {
		t.Errorf("NextID = %d, want > 7", l.NextID)
	}
}

func TestParseMarkdownRejectsProse(t *testing.T) {
	if _, err := parseMarkdown("just some words"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestMarkdownRoundTrip(t *testing.T) {
	l, err := parseMarkdown(sample)
	if err != nil {
		t.Fatal(err)
	}
	again, err := parseMarkdown(l.markdown(true))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := l.markdown(true), again.markdown(true); a != b {
		t.Errorf("round trip changed the list:\n%s\nvs\n%s", a, b)
	}
}

func TestProgress(t *testing.T) {
	g := Group{Items: []Item{{Status: StatusDone}, {Status: StatusActive}, {Status: StatusPending}, {Status: StatusPending}}}
	if got := g.progress(); got != 1.5/4 {
		t.Errorf("progress = %v", got)
	}
	pin := 0.75
	g.Progress = &pin
	if got := g.progress(); got != 0.75 {
		t.Errorf("pinned progress = %v", got)
	}
}

func TestNormalizeStatusAliases(t *testing.T) {
	l := &List{Groups: []Group{{Items: []Item{{Text: "a", Status: "in_progress"}, {Text: "b", Status: "completed"}, {Text: "c"}}}}}
	l.normalize()
	got := []Status{l.Groups[0].Items[0].Status, l.Groups[0].Items[1].Status, l.Groups[0].Items[2].Status}
	if got[0] != StatusActive || got[1] != StatusDone || got[2] != StatusPending {
		t.Errorf("statuses = %v", got)
	}
}

func TestCommands(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_LIST", "t")
	must := func(args ...string) {
		t.Helper()
		if err := run(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	must("group", "Build")
	must("add", "first")
	must("add", "second")
	must("add", "-g", "Ship", "-n", "a note", "third")
	must("start", "1")
	must("start", "2") // demotes 1
	must("next")       // finishes 2, starts 1
	must("note", "1", "why")
	must("progress", "Ship", "50")

	l, err := loadList(listPath("t"))
	if err != nil {
		t.Fatal(err)
	}
	if st := l.find(2).Status; st != StatusDone {
		t.Errorf("item 2 = %s, want done", st)
	}
	if st := l.find(1).Status; st != StatusActive {
		t.Errorf("item 1 = %s, want active", st)
	}
	if l.find(3).Note != "a note" || l.Groups[1].Title != "Ship" || *l.Groups[1].Progress != 0.5 {
		t.Errorf("group/note wrong: %+v", l.Groups[1])
	}
	must("rm", "3")
	if l, _ = loadList(listPath("t")); l.find(3) != nil {
		t.Error("rm left item 3")
	}
	if err := run([]string{"done", "99"}); err == nil {
		t.Error("done on a missing item should fail")
	}
}

func TestListNameIsSafe(t *testing.T) {
	t.Setenv("REXPAD_LIST", "")
	t.Setenv("REX_BLOCK", "block:01a1/../x")
	name := listName("")
	if strings.ContainsAny(name, ":/") || name == "" {
		t.Errorf("unsafe list name %q", name)
	}
	if filepath.Dir(listPath(name)) != stateDir() {
		t.Errorf("list escapes the state dir: %s", listPath(name))
	}
}

func TestRender(t *testing.T) {
	fonts, err := loadFonts()
	if err != nil {
		t.Fatal(err)
	}
	l, _ := parseMarkdown(sample)
	r := &renderer{fonts: fonts, scale: 2, pal: lightPalette}
	img := r.Render(l, 1000, 600, 0)
	if img.Bounds().Dx() != 1000 || img.Bounds().Dy() != 600 {
		t.Fatalf("size = %v", img.Bounds())
	}
	// The divider under the first header sits 15pt below its center line.
	y := int((marginTop + dividerOffset + 0.5) * 2)
	if got := img.RGBAAt(500, y); got != lightPalette.divider {
		t.Errorf("divider pixel = %v, want %v", got, lightPalette.divider)
	}
	if got := img.RGBAAt(2, 2); got != (color.RGBA{255, 255, 255, 255}) {
		t.Errorf("background = %v", got)
	}
	if os.Getenv("REXPAD_WRITE_TEST_PNG") != "" {
		// Handy when tuning: REXPAD_WRITE_TEST_PNG=1 go test -run TestRender
		f, _ := os.Create(filepath.Join(os.TempDir(), "rexpad-test.png"))
		defer f.Close()
		_ = png.Encode(f, img)
	}
}

func TestRenderScrollsToActive(t *testing.T) {
	fonts, _ := loadFonts()
	l := &List{}
	g := Group{Title: "Long"}
	for i := 0; i < 40; i++ {
		g.Items = append(g.Items, Item{Text: "item", Status: StatusDone})
	}
	g.Items[35].Status = StatusActive
	l.Groups = []Group{g}
	l.normalize()
	r := &renderer{fonts: fonts, scale: 1, pal: lightPalette}
	off, _ := followOffset(l, 300)
	img := r.Render(l, 400, 300, off)
	// The active row's highlight must be on screen somewhere.
	found := false
	for y := 0; y < 300 && !found; y++ {
		found = img.RGBAAt(20, y) == lightPalette.activeBg
	}
	if !found {
		t.Error("active row not scrolled into view")
	}
}

func TestParseRGB(t *testing.T) {
	cases := map[string]color.RGBA{
		"ffff/8080/0000": {255, 128, 0, 255},
		"1818/1b1b/1818": {24, 27, 24, 255},
		"f/8/0":          {255, 136, 0, 255},
	}
	for in, want := range cases {
		if got, ok := parseRGB(in); !ok || got != want {
			t.Errorf("parseRGB(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	if _, ok := parseRGB("zz/00/00"); ok {
		t.Error("bad hex accepted")
	}
}

func TestTermColorsFollowTheme(t *testing.T) {
	var tc termColors
	if p := tc.palette("terminal"); p != lightPalette {
		t.Error("no reply should fall back to the light design")
	}
	for _, r := range []string{"11;rgb:1818/1b1b/1818", "10;rgb:cece/d5d5/cece", "4;4;rgb:8181/a2a2/bebe"} {
		if !tc.apply(r) {
			t.Fatalf("apply(%q) = false", r)
		}
	}
	if tc.apply("4;1;rgb:ffff/0000/0000") {
		t.Error("palette entries other than blue should be ignored")
	}
	p := tc.palette("terminal")
	if p.bg != (color.RGBA{24, 27, 24, 255}) || p.text != (color.RGBA{206, 213, 206, 255}) || p.accent != (color.RGBA{129, 162, 190, 255}) {
		t.Errorf("palette = %+v", p)
	}
	if luminance(p.divider) <= luminance(p.bg) || luminance(p.divider) >= luminance(p.doneText) {
		t.Error("divider should sit between the background and the dimmed text")
	}
	if tc.palette("light") != lightPalette || tc.palette("dark") != darkPalette {
		t.Error("explicit themes should ignore the terminal")
	}
}

func TestThemedPaletteMatchesDesignOnWhite(t *testing.T) {
	p := themedPalette(lightPalette.text, lightPalette.bg, lightPalette.accent)
	near := func(a, b color.RGBA) bool {
		d := func(x, y uint8) int { return max(int(x), int(y)) - min(int(x), int(y)) }
		return d(a.R, b.R) <= 6 && d(a.G, b.G) <= 6 && d(a.B, b.B) <= 6
	}
	for name, pair := range map[string][2]color.RGBA{
		"divider": {p.divider, lightPalette.divider},
		"box":     {p.box, lightPalette.box},
		"note":    {p.note, lightPalette.note},
		"done":    {p.doneText, lightPalette.doneText},
	} {
		if !near(pair[0], pair[1]) {
			t.Errorf("%s = %v, want about %v", name, pair[0], pair[1])
		}
	}
}

func TestPickScale(t *testing.T) {
	t.Setenv("REXPAD_SCALE", "")
	cases := []struct {
		cellH int
		want  float64
	}{{34, 2}, {36, 2}, {17, 1}, {12, 1}, {45, 2.5}, {51, 3}}
	for _, c := range cases {
		if got := pickScale(0, winSize{rows: 10, cols: 10, xpix: 160, ypix: c.cellH * 10}); got != c.want {
			t.Errorf("cell height %d: scale %v, want %v", c.cellH, got, c.want)
		}
	}
}

func TestKittyPlacementPinsCellsAndCrops(t *testing.T) {
	var b strings.Builder
	w := bufio.NewWriter(&b)
	if err := kittyUpload(w, image.NewRGBA(image.Rect(0, 0, 4, 4)), 7); err != nil {
		t.Fatal(err)
	}
	kittyPlace(w, 7, 120, 640, 1302, 42, 19)
	w.Flush()
	got := b.String()
	if !strings.HasPrefix(got, "\x1b_Ga=t,f=100,i=7,q=2,") {
		t.Errorf("upload = %q", got[:40])
	}
	if !strings.HasSuffix(got, "\x1b[H\x1b_Ga=p,i=7,p=1,x=0,y=120,w=640,h=1302,c=42,r=19,C=1,q=2\x1b\\") {
		t.Errorf("placement = %q", got[len(got)-80:])
	}
}

func TestSkillInstall(t *testing.T) {
	dir := t.TempDir()
	if err := runSkill([]string{"install", "--dir", dir}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "rexpad", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "---\nname: rexpad\n") {
		t.Errorf("skill starts %q", string(got[:30]))
	}
}

func TestCSIScroll(t *testing.T) {
	cases := map[string]scrollKind{
		"<64;10;5M": wheelUp, "<65;10;5M": wheelDown, "<68;1;1M": wheelUp, // Shift+wheel
		"<0;3;4M": scrollNone, "A": lineUp, "B": lineDown, "5~": pageUp, "6~": pageDown,
		"H": scrollTop, "F": scrollBottom, "?997;1n": scrollNone,
	}
	for in, want := range cases {
		if got := csiScroll(in); got != want {
			t.Errorf("csiScroll(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRenderHonorsOffset(t *testing.T) {
	fonts, _ := loadFonts()
	l := &List{Groups: []Group{{Title: "Long"}}}
	for i := 0; i < 40; i++ {
		l.Groups[0].Items = append(l.Groups[0].Items, Item{Text: "item"})
	}
	l.normalize()
	r := &renderer{fonts: fonts, scale: 1, pal: lightPalette}
	_, maxOff := followOffset(l, 300)
	if maxOff <= 0 {
		t.Fatal("a 40-item list should overflow 300pt")
	}
	top := r.Render(l, 400, 300, 0)
	// The header's divider is on screen at offset 0 and gone once scrolled.
	y := int(marginTop + dividerOffset) // the 1pt divider row at scale 1
	if top.RGBAAt(200, y) != lightPalette.divider {
		t.Fatal("divider missing at the top")
	}
	if r.Render(l, 400, 300, 200).RGBAAt(200, y) == lightPalette.divider {
		t.Error("offset didn't scroll the content")
	}
	// Offsets past the end clamp instead of showing blank space.
	a, b := r.Render(l, 400, 300, maxOff), r.Render(l, 400, 300, maxOff+500)
	if string(a.Pix) != string(b.Pix) {
		t.Error("offset wasn't clamped to the content")
	}
}

func TestEaseTowardGlidesThenSettles(t *testing.T) {
	shown, target := 0.0, 14.0 // one wheel tick, in points
	first := easeToward(shown, target, animFrame, 2)
	if first <= 0 || first >= target {
		t.Fatalf("first frame = %v, want partway to %v", first, target)
	}
	elapsed := animFrame
	for shown = first; shown != target; elapsed += animFrame {
		next := easeToward(shown, target, animFrame, 2)
		if next < shown {
			t.Fatalf("went backwards: %v -> %v", shown, next)
		}
		shown = next
		if elapsed > time.Second {
			t.Fatal("never settled")
		}
	}
	if elapsed > 300*time.Millisecond {
		t.Errorf("a single tick took %v to settle", elapsed)
	}
	// Scrolling up eases down toward a smaller target too.
	if got := easeToward(20, 6, animFrame, 2); got >= 20 || got <= 6 {
		t.Errorf("upward step = %v", got)
	}
}
