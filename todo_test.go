package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/sfnt"
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
	// An indented line under an item is skipped, not read as an item.
	if n := len(l.Groups[0].Items); n != 1 {
		t.Errorf("first group has %d items, want 1: %+v", n, l.Groups[0].Items)
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
	must("add", "-g", "Ship", "third")
	must("start", "1")
	must("start", "2") // demotes 1
	must("next")       // finishes 2, starts 1
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
	if l.find(3) == nil || l.Groups[1].Title != "Ship" || *l.Groups[1].Progress != 0.5 {
		t.Errorf("group wrong: %+v", l.Groups[1])
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
	if !strings.HasPrefix(got, "\x1b_Ga=t,f=32,o=z,s=4,v=4,i=7,q=2,") {
		t.Errorf("upload = %q", got[:40])
	}
	if !strings.HasSuffix(got, "\x1b7\x1b[H\x1b_Ga=p,i=7,p=1,x=0,y=120,w=640,h=1302,c=42,r=19,C=1,q=2\x1b\\\x1b8") {
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
		"<0;3;4M": scrollNone, "A": scrollNone, "?997;1n": scrollNone,
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

func TestFuzzyMatch(t *testing.T) {
	cases := []struct {
		s, q string
		want [][2]int // nil: no match
	}{
		{"Fix the fix", "fix", [][2]int{{0, 3}}},                     // a substring wins
		{"Fix the fix", "Fix", [][2]int{{0, 3}}},                     // smart case
		{"fix the fix", "Fix", nil},                                  // an uppercase letter makes it exact
		{"Build the app", "bta", [][2]int{{0, 1}, {6, 7}, {10, 11}}}, // in order, with gaps
		{"a b ab", "ab", [][2]int{{4, 6}}},
		{"axxbxab", "a b", [][2]int{{0, 1}, {3, 4}}}, // terms match on their own
		{"build app", "app bui", [][2]int{{0, 3}, {6, 9}}},
		{"Notarize", "nz", [][2]int{{0, 1}, {6, 7}}},
		{"Notarize", "zn", nil}, // out of order
		{"Ünïcode café", "cfé", [][2]int{{10, 11}, {12, 15}}},
		{"anything", " ", nil},
		{"anything", "\xc3", nil}, // half a character, mid-typing
	}
	for _, c := range cases {
		got, ok := fuzzyMatch(c.s, c.q)
		if !ok {
			got = nil
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) || ok != (c.want != nil) {
			t.Errorf("fuzzyMatch(%q, %q) = %v %v, want %v", c.s, c.q, got, ok, c.want)
		}
	}
}

// The tightest window wins over the first letters that happen to fit.
func TestFuzzyMatchTightens(t *testing.T) {
	got, _ := fuzzyMatch("a-x-b ... ab-c", "abc")
	if fmt.Sprint(got) != "[[10 12] [13 14]]" {
		t.Errorf("got %v", got)
	}
}

func TestFindCollapsesAndSteps(t *testing.T) {
	l, err := parseMarkdown("# Build\n- [x] build the app\n- [ ] test\n# Ship\n- [ ] rebuild and ship\n# Docs\n- [ ] write\n")
	if err != nil {
		t.Fatal(err)
	}
	var f findState
	f.setQuery(l, "bld")
	// Rows: 0 Build, 1 build the app, 2 test, 3 Ship, 4 rebuild and ship, 5 Docs, 6 write.
	if got := fmt.Sprint(f.vis); got != "[true true false true true false false]" {
		t.Errorf("vis = %v", got)
	}
	var rowsHit []int
	for _, m := range f.matches {
		rowsHit = append(rowsHit, m.row)
	}
	// Ship's header shows for its item but isn't a match itself.
	if fmt.Sprint(rowsHit) != "[0 1 4]" || f.status() != "1 of 3" {
		t.Errorf("matches on rows %v, status %q", rowsHit, f.status())
	}
	f.step(-1)
	if f.cur != 2 {
		t.Errorf("stepping back from the first match went to %d, want the last", f.cur)
	}
	f.step(1)
	if f.cur != 0 {
		t.Errorf("stepping on from the last match went to %d, want the first", f.cur)
	}

	// The list changes under the find: it stays on the same item.
	f.step(2) // "rebuild and ship"
	l2, _ := parseMarkdown("# Build\n- [x] build the app\n- [ ] test\n# Ship\n- [ ] rebuild and ship\n")
	l2.Groups[0].Items = append(l2.Groups[0].Items, Item{ID: 9, Text: "bold", Status: StatusPending})
	f.refresh(l2) // a new match lands before it
	if m, _ := f.current(); m.itemID != 3 {
		t.Errorf("after refresh on item %d, want 3", m.itemID)
	}

	f.setQuery(l, "zzz")
	if f.status() != "" { // the list says it, not the bar
		t.Errorf("status = %q", f.status())
	}
	f.setQuery(l, "  ")
	if f.vis != nil || f.status() != "" {
		t.Error("a blank query should show everything")
	}
}

func TestLayoutVisCollapses(t *testing.T) {
	l, _ := parseMarkdown("# A\n- [ ] one\n- [ ] two\n- [ ] three\n# B\n- [ ] four\n")
	all, h := layout(l)
	allVis, hVis := layoutVis(l, []bool{true, true, true, true, true, true})
	if fmt.Sprint(all) != fmt.Sprint(allVis) || h != hVis {
		t.Fatal("showing every row should be the plain layout")
	}
	// Hide "two": "three" moves up into its place and the list gets shorter.
	rows, h2 := layoutVis(l, []bool{true, true, false, true, true, true})
	if rows[3].center != all[2].center {
		t.Errorf("three at %v, want %v", rows[3].center, all[2].center)
	}
	if h-h2 != itemPitch {
		t.Errorf("height shrank by %v, want a row", h-h2)
	}
	if rows[2].center != all[2].center-itemPitch/2 {
		t.Errorf("hidden row folds to %v, want the seam at %v", rows[2].center, all[2].center-itemPitch/2)
	}
	// Hiding all of A puts B at the top.
	rows, _ = layoutVis(l, []bool{false, false, false, false, true, true})
	if rows[4].center != marginTop {
		t.Errorf("B at %v, want the top", rows[4].center)
	}
	// Hiding everything leaves no content.
	if _, h := layoutVis(l, make([]bool, 6)); h != 0 {
		t.Errorf("empty height %v", h)
	}
}

func TestGeoEasesAndSettles(t *testing.T) {
	l, _ := parseMarkdown("# A\n- [ ] one\n- [ ] two\n- [ ] three\n")
	from, _ := geoFor(l, nil)
	to, _ := geoFor(l, []bool{true, false, false, true})
	g := append([]rowGeo(nil), from...)
	if easeGeo(l, g, to, animFrame) {
		t.Fatal("settled in one frame")
	}
	if g[1].open <= 0 || g[1].open >= 1 || g[3].center >= from[3].center || g[3].center <= to[3].center {
		t.Errorf("first frame not partway: %+v", g)
	}
	// Folding neighbors keep to their own slots.
	if gap := g[2].center - g[1].center; math.Abs(gap-(g[1].open+g[2].open)*itemPitch/2) > 1e-9 {
		t.Errorf("rows one and two overlap: %+v", g)
	}
	elapsed := animFrame
	for !easeGeo(l, g, to, animFrame) {
		if elapsed += animFrame; elapsed > time.Second {
			t.Fatal("never settled")
		}
	}
	if !slices.Equal(g, to) {
		t.Error("settled away from the target")
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("fold took %v", elapsed)
	}
}

func TestRenderCollapsingRows(t *testing.T) {
	fonts, _ := loadFonts()
	l, _ := parseMarkdown("# Plan\n- [ ] alpha\n- [ ] beta\n- [ ] gamma\n")
	r := &renderer{fonts: fonts, scale: 1, pal: lightPalette}
	open := r.Render(l, 300, 200, 0)

	var f findState
	f.setQuery(l, "gam")
	r.marks = f.spans()
	r.geo, r.geoH = geoFor(l, f.vis)
	rows, _ := layout(l)
	r.ring = &ringGeo{y: rows[1].center, alpha: 1}
	marked := r.Render(l, 300, 200, 0)
	// Gamma moved up to alpha's place, outlined as the current match.
	x := int(math.Floor(marginLeft - activeInset + 0.5))
	if got := marked.RGBAAt(x, int(rows[1].center)); got != lightPalette.accent {
		t.Errorf("current match outline = %v, want the accent", got)
	}
	// Gamma's old place is empty now.
	y := int(rows[3].center)
	for px := 0; px < 300; px++ {
		if marked.RGBAAt(px, y) != lightPalette.bg {
			t.Fatalf("pixel %d at gamma's old row isn't background", px)
		}
	}

	// Halfway through the fold beta is faded and squeezed, not gone.
	half := make([]rowGeo, len(r.geo))
	full, _ := geoFor(l, nil)
	for i := range half {
		half[i] = rowGeo{(full[i].center + r.geo[i].center) / 2, (full[i].open + r.geo[i].open) / 2}
	}
	r.geo, r.marks, r.ring = half, nil, nil
	mid := r.Render(l, 300, 200, 0)
	if string(mid.Pix) == string(open.Pix) || string(mid.Pix) == string(marked.Pix) {
		t.Error("the halfway frame matches an end")
	}

	// Everything filtered out says so.
	f.setQuery(l, "zzz")
	r.geo, r.geoH = geoFor(l, f.vis)
	r.empty, r.emptyAlpha = "No matching items", 1
	if got := r.Render(l, 300, 200, 0); string(got.Pix) == string(open.Pix) {
		t.Error("an empty filter drew the list")
	}

	// The bar draws in any state without panicking, at the bottom's size.
	f.open, f.input = true, true
	if b, end := r.findBar(&f, 300, 34, 17, 27); b.Bounds().Dy() != 34 || end <= 27 {
		t.Errorf("bar height = %d, field ends at %v", b.Bounds().Dy(), end)
	}
}

func TestKittyPlaceAtRow(t *testing.T) {
	var b strings.Builder
	w := bufio.NewWriter(&b)
	kittyPlaceAt(w, 18, 9, 0, 640, 68, 42, 2)
	w.Flush()
	if want := "\x1b7\x1b[18;1H\x1b_Ga=p,i=9,p=1,x=0,y=0,w=640,h=68,c=42,r=2,C=1,q=2\x1b\\\x1b8"; b.String() != want {
		t.Errorf("placement = %q", b.String())
	}
}

func decodeAll(chunks ...string) []termEvent {
	var d keyDecoder
	var out []termEvent
	emit := func(ev termEvent) { out = append(out, ev) }
	for _, c := range chunks {
		d.feed([]byte(c), emit)
		d.flush(emit)
	}
	return out
}

// The bytes Rex sends with the kitty keyboard protocol on, and without.
func TestDecodeKeys(t *testing.T) {
	cases := []struct {
		in   string
		want keyPress
	}{
		{"\x1b[97;9u", keyPress{'a', modSuper}},              // Cmd-A
		{"\x1b[122;10u", keyPress{'z', modSuper | modShift}}, // Cmd-Shift-Z
		{"\x1b[127;3u", keyPress{keyBackspace, modAlt}},      // Opt-Backspace
		{"\x1b[127;9u", keyPress{keyBackspace, modSuper}},    // Cmd-Backspace
		{"\x1b[119;5u", keyPress{'w', modCtrl}},              // Ctrl-W
		{"\x1b[27u", keyPress{keyEscape, 0}},
		{"\x1b[99;5u", keyPress{'c', modCtrl}},
		{"\x1b[1;3D", keyPress{keyLeft, modAlt}},
		{"\x1b[1;10D", keyPress{keyLeft, modSuper | modShift}},
		{"\x1b[1;2C", keyPress{keyRight, modShift}},
		{"\x1b[3;3~", keyPress{keyDelete, modAlt}},
		{"\x1b[H", keyPress{keyHome, 0}},
		{"\x1b[1;2H", keyPress{keyHome, modShift}},
		{"\x1bOD", keyPress{keyLeft, 0}},
		{"\x1b[97;73u", keyPress{'a', modSuper}},     // caps lock doesn't count
		{"\x1b", keyPress{keyEscape, 0}},             // a lone Escape
		{"\x1b\x7f", keyPress{keyBackspace, modAlt}}, // legacy Opt-Backspace
		{"\x17", keyPress{'w', modCtrl}},
		{"\x7f", keyPress{keyBackspace, 0}},
		{"\r", keyPress{keyEnter, 0}},
		{"é", keyPress{'é', 0}},
		{"N", keyPress{'N', 0}},
	}
	for _, c := range cases {
		got := decodeAll(c.in)
		if len(got) != 1 || got[0].key != c.want {
			t.Errorf("decode %q = %+v, want %+v", c.in, got, c.want)
		}
	}
	// A character split across reads still arrives whole.
	if got := decodeAll("\xc3", "\xa9"); len(got) != 1 || got[0].key.code != 'é' {
		t.Errorf("split é = %+v", got)
	}
	// Releases and the protocol's own replies aren't keys.
	if got := decodeAll("\x1b[97;9:3u", "\x1b[?1u"); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
	// A paste comes whole, Enter and escapes included, and isn't typed.
	got := decodeAll("\x1b[200~one\r\x1b[Atwo\x1b[201~x")
	if len(got) != 2 || got[0].paste != "one\r\x1b[Atwo" || got[1].key.code != 'x' {
		t.Errorf("paste = %+v", got)
	}
	// Wheel ticks, scheme reports, and OSC replies still come through.
	got = decodeAll("\x1b[<65;3;4M\x1b[?997;1n\x1b]11;rgb:ffff/ffff/ffff\x07")
	if len(got) != 3 || got[0].scroll != wheelDown || !got[1].scheme || got[2].osc != "11;rgb:ffff/ffff/ffff" {
		t.Errorf("events = %+v", got)
	}
}

// type feeds keys to a field: plain text, or one keyPress.
func typeInto(f *textField, keys ...any) {
	for _, k := range keys {
		switch k := k.(type) {
		case string:
			for _, r := range k {
				f.handle(keyPress{code: r})
			}
		case keyPress:
			f.handle(k)
		}
	}
}

// show renders a field's state with [ ] around the selection and | for the caret.
func show(f *textField) string {
	a, b := f.sel()
	if a == b {
		return f.text[:a] + "|" + f.text[a:]
	}
	return f.text[:a] + "[" + f.text[a:b] + "]" + f.text[b:]
}

func TestFieldMovesLikeAMacField(t *testing.T) {
	left, right := keyPress{keyLeft, 0}, keyPress{keyRight, 0}
	optL, optR := keyPress{keyLeft, modAlt}, keyPress{keyRight, modAlt}
	cmdL, cmdR := keyPress{keyLeft, modSuper}, keyPress{keyRight, modSuper}
	shift := func(k keyPress) keyPress { k.mods |= modShift; return k }

	steps := []struct {
		keys []any
		want string
	}{
		{[]any{"fix the build"}, "fix the build|"},
		{[]any{optL}, "fix the |build"},
		{[]any{optL, optL}, "|fix the build"},
		{[]any{optR}, "fix| the build"},
		{[]any{optR}, "fix the| build"},
		{[]any{cmdR}, "fix the build|"},
		{[]any{shift(optL)}, "fix the [build]"},
		{[]any{shift(optL)}, "fix [the build]"},
		{[]any{left}, "fix |the build"}, // a selection collapses to its start
		{[]any{shift(cmdR)}, "fix [the build]"},
		{[]any{right}, "fix the build|"},
		{[]any{shift(left), shift(left)}, "fix the bui[ld]"},
		{[]any{shift(cmdL)}, "[fix the build]"}, // extends from the anchor, at the end
		{[]any{cmdL}, "|fix the build"},
		{[]any{keyPress{'a', modSuper}}, "[fix the build]"},
		{[]any{"go"}, "go|"}, // typing replaces the selection
		{[]any{keyPress{'a', modCtrl}}, "|go"},
		{[]any{keyPress{'e', modCtrl}}, "go|"},
		{[]any{keyPress{keyHome, modShift}}, "[go]"},
	}
	var f textField
	for _, s := range steps {
		typeInto(&f, s.keys...)
		if got := show(&f); got != s.want {
			t.Fatalf("after %v: %q, want %q", s.keys, got, s.want)
		}
	}
}

func TestFieldDeletesAndUndoes(t *testing.T) {
	bs := keyPress{keyBackspace, 0}
	steps := []struct {
		keys []any
		want string
	}{
		{[]any{"rebuild the app"}, "rebuild the app|"},
		{[]any{keyPress{keyBackspace, modAlt}}, "rebuild the |"},
		{[]any{keyPress{keyBackspace, modAlt}}, "rebuild |"},
		{[]any{"and ship"}, "rebuild and ship|"},
		{[]any{keyPress{keyBackspace, modSuper}}, "|"},
		{[]any{keyPress{'z', modSuper}}, "rebuild and ship|"},
		{[]any{keyPress{'z', modSuper}}, "rebuild |"}, // the typing undoes in one go
		{[]any{keyPress{'z', modSuper | modShift}}, "rebuild and ship|"},
		{[]any{keyPress{keyLeft, modSuper}, keyPress{keyDelete, modAlt}}, "| and ship"},
		{[]any{keyPress{'k', modCtrl}}, "|"},
		{[]any{"a b c", bs, bs}, "a b|"},
		{[]any{keyPress{'w', modCtrl}}, "a |"},
		{[]any{keyPress{'u', modCtrl}}, "|"},
	}
	var f textField
	for _, s := range steps {
		typeInto(&f, s.keys...)
		if got := show(&f); got != s.want {
			t.Fatalf("after %v: %q, want %q", s.keys, got, s.want)
		}
	}
}

func TestFieldCopyCutPaste(t *testing.T) {
	var f textField
	typeInto(&f, "find me", keyPress{keyLeft, modAlt | modShift})
	if r := f.handle(keyPress{'c', modSuper}); r.copy != "me" || r.edited {
		t.Errorf("copy = %+v", r)
	}
	if r := f.handle(keyPress{'x', modSuper}); r.copy != "me" || !r.edited || show(&f) != "find |" {
		t.Errorf("cut = %+v, field %q", r, show(&f))
	}
	f.paste("two\nlines\x1b")
	if show(&f) != "find two lines|" {
		t.Errorf("paste: %q", show(&f))
	}
	// Keys the field doesn't know pass through to the viewer.
	if r := f.handle(keyPress{'n', modCtrl}); r.handled {
		t.Error("the field took Ctrl-N")
	}
}

func TestFieldLaysOutInCells(t *testing.T) {
	var f textField
	runs, caret := f.layout(10, true)
	if fmt.Sprint(runs) != "[{Find false true}]" || caret != 0 {
		t.Errorf("empty: %v caret %d", runs, caret)
	}
	typeInto(&f, "fix the build")
	runs, caret = f.layout(10, true)
	// The caret sits after the text, so the last 9 characters show.
	if fmt.Sprint(runs) != "[{the build false false}]" || caret != 9 || f.scrollCol != 4 {
		t.Errorf("scrolled: %v caret %d scroll %d", runs, caret, f.scrollCol)
	}
	typeInto(&f, keyPress{keyLeft, modAlt | modShift})
	runs, caret = f.layout(10, true)
	if fmt.Sprint(runs) != "[{the  false false} {build true false}]" || caret != 4 {
		t.Errorf("selected: %v caret %d", runs, caret)
	}
	if runs, _ := f.layout(10, false); len(runs) != 1 || runs[0].selected {
		t.Errorf("not typing, the selection hides: %v", runs)
	}
	typeInto(&f, keyPress{keyLeft, modSuper})
	if _, caret = f.layout(10, true); caret != 0 || f.scrollCol != 0 {
		t.Errorf("home: caret %d scroll %d", caret, f.scrollCol)
	}
	// Wide characters take two cells.
	var g textField
	typeInto(&g, "日本語")
	if _, caret = g.layout(10, true); caret != 6 {
		t.Errorf("after three wide characters the caret is at %d, want 6", caret)
	}
}

func TestRingSettlesInAndGlides(t *testing.T) {
	// Appearing: it starts wide and faint and closes in on the row.
	g, to := ringGeo{y: 40, grow: ringGrow}, ringGeo{y: 40, alpha: 1}
	g = easeRing(g, to, animFrame, 2)
	if g.alpha <= 0 || g.alpha >= 1 || g.grow <= 0 || g.grow >= ringGrow {
		t.Fatalf("first frame = %+v", g)
	}
	// Moving on: it glides to the next match rather than jumping.
	to.y = 96
	elapsed := time.Duration(0)
	for prev := g.y; g != to; elapsed += animFrame {
		g = easeRing(g, to, animFrame, 2)
		if g.y < prev {
			t.Fatalf("went backwards: %v -> %v", prev, g.y)
		}
		if g.y != to.y && g.y >= to.y {
			t.Fatalf("overshot: %v", g.y)
		}
		prev = g.y
		if elapsed > time.Second {
			t.Fatal("never settled")
		}
	}
	if elapsed < 3*animFrame || elapsed > 400*time.Millisecond {
		t.Errorf("glide took %v", elapsed)
	}

	// Drawn faint and wide, it's lighter than at rest and reaches further out.
	fonts, _ := loadFonts()
	l, _ := parseMarkdown("- [ ] one\n- [ ] two\n")
	r := &renderer{fonts: fonts, scale: 1, pal: lightPalette}
	rows, _ := layout(l)
	r.ring = &ringGeo{y: rows[1].center, alpha: 1}
	rest := r.Render(l, 300, 120, 0)
	r.ring = &ringGeo{y: rows[1].center, alpha: 0.4, grow: 3}
	wide := r.Render(l, 300, 120, 0)
	top := int(rows[1].center - itemPitch/2 + 2) // the ring's top edge at rest
	if rest.RGBAAt(150, top) != lightPalette.accent {
		t.Fatal("no ring at rest")
	}
	if c := wide.RGBAAt(150, top-3); c == lightPalette.bg || c == lightPalette.accent {
		t.Errorf("growing ring at its wider edge = %v, want a faint accent", c)
	}
}

func TestNoMatchesFadesIn(t *testing.T) {
	fonts, _ := loadFonts()
	l, _ := parseMarkdown("- [ ] one\n")
	var f findState
	f.setQuery(l, "zzz")
	r := &renderer{fonts: fonts, scale: 2, pal: lightPalette, empty: "No matching items"}
	r.geo, r.geoH = geoFor(l, f.vis)
	ink := func(alpha float64) int { // how much darker than the background the message is
		r.emptyAlpha = alpha
		img := r.Render(l, 300, 80, 0)
		sum := 0
		for i := 0; i < len(img.Pix); i += 4 {
			sum += 255 - int(img.Pix[i])
		}
		return sum
	}
	none, half, full := ink(0), ink(0.5), ink(1)
	if none != 0 || !(half > 0 && half < full) {
		t.Errorf("ink at 0, 0.5, 1 = %d, %d, %d; want 0 < half < full", none, half, full)
	}
}

func TestClearRenderPaintsNoBackground(t *testing.T) {
	fonts, _ := loadFonts()
	l, _ := parseMarkdown("# Plan\n- [~] active\n- [ ] pending\n")
	// Clear renders follow a terminal theme, whose tints are accent mixes.
	pal := themedPalette(rgb(220, 222, 226), rgb(17, 18, 22), rgb(84, 136, 238))
	r := &renderer{fonts: fonts, scale: 2, pal: pal.seeThrough(), clear: true}
	img := r.Render(l, 400, 200, 0)
	rows, _ := layout(l)
	at := func(x, y float64) color.RGBA { return img.RGBAAt(int(x*2), int(y*2)) }
	if c := at(390/2, 190/2); c.A != 0 {
		t.Errorf("empty space = %v, want transparent", c)
	}
	// The ring and the checkbox are hollow, not filled with a background.
	if c := at(marginLeft+iconCenterX, rows[0].center); c.A != 0 {
		t.Errorf("empty ring's middle = %v, want transparent", c)
	}
	if c := at(marginLeft+boxCenter, rows[2].center); c.A != 0 {
		t.Errorf("pending box's middle = %v, want transparent", c)
	}
	// The active row is a see-through tint of the accent.
	if c := at(150, rows[1].center-9); c.A == 0 || c.A == 255 {
		t.Errorf("active row = %v, want see-through", c)
	}
	// The grays are see-through too: the divider under the header and the
	// pending box's outline.
	if c := at(100, rows[0].center+dividerOffset+0.25); c.A == 0 || c.A == 255 {
		t.Errorf("divider = %v, want see-through", c)
	}
	if c := at(marginLeft+boxCenter-boxSize/2+0.25, rows[2].center); c.A == 0 || c.A == 255 {
		t.Errorf("box outline = %v, want see-through", c)
	}
	// Over its background it looks the same as the painted render.
	r.clear, r.pal = false, pal
	painted := r.Render(l, 400, 200, 0)
	over := image.NewRGBA(img.Bounds())
	draw.Draw(over, over.Bounds(), image.NewUniform(pal.bg), image.Point{}, draw.Src)
	draw.Draw(over, over.Bounds(), img, image.Point{}, draw.Over)
	diff := 0
	for i := range over.Pix {
		d := int(over.Pix[i]) - int(painted.Pix[i])
		diff = max(diff, d, -d)
	}
	if diff > 3 {
		t.Errorf("clear over the background differs from painted by up to %d", diff)
	}
}

func TestNewRowsUnfoldIntoPlace(t *testing.T) {
	old := &List{Groups: []Group{{Title: "Build", Items: []Item{{ID: 1, Text: "compile"}, {ID: 2, Text: "test"}}}}}
	nl := &List{Groups: []Group{
		{Title: "Build", Items: []Item{{ID: 1, Text: "compile"}, {ID: 3, Text: "lint"}, {ID: 2, Text: "test"}}},
		{Title: "Ship", Items: []Item{{ID: 4, Text: "upload"}}},
	}}
	from := arrivalGeo(old, nil, nl)
	var opens []float64
	for _, g := range from {
		opens = append(opens, g.open)
	}
	// Build, compile, lint (new), test, Ship (new), upload (new).
	if fmt.Sprint(opens) != "[1 1 0 1 0 0]" {
		t.Fatalf("opens = %v", opens)
	}
	// Folded shut, lint takes no room yet: test sits where it was.
	oldRows, _ := layout(old)
	if from[3].center != oldRows[2].center {
		t.Errorf("test starts at %v, want %v", from[3].center, oldRows[2].center)
	}
	// Easing opens lint and pushes test down to its new place.
	to, _ := geoFor(nl, nil)
	g := append([]rowGeo(nil), from...)
	for !easeGeo(nl, g, to, animFrame) {
	}
	if !slices.Equal(g, to) {
		t.Error("didn't settle at the new layout")
	}

	// Rewriting the same plan with new IDs replays nothing.
	same := &List{Groups: []Group{{Title: "Build", Items: []Item{{ID: 7, Text: "compile"}, {ID: 8, Text: "test"}}}}}
	if from, to := arrivalGeo(old, nil, same), func() []rowGeo { g, _ := geoFor(same, nil); return g }(); !slices.Equal(from, to) {
		t.Errorf("a rewrite animated: %v", from)
	}
	// A first list just appears.
	if arrivalGeo(&List{}, nil, nl) != nil {
		t.Error("the first list animated")
	}
}

func TestEncodeImageIsStraightRGBA(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 3, 1))
	img.Set(0, 0, color.RGBA{0, 0, 0, 0})
	img.SetRGBA(1, 0, color.RGBA{50, 100, 25, 100}) // premultiplied: half of 100, 200, 50 at alpha 100... roughly
	img.SetRGBA(2, 0, color.RGBA{10, 20, 30, 255})
	e := encodeImage(img.SubImage(image.Rect(1, 0, 3, 1)).(*image.RGBA))
	if e.w != 2 || e.h != 1 {
		t.Fatalf("size %dx%d", e.w, e.h)
	}
	zr, err := zlib.NewReader(bytes.NewReader(e.data))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	want := []byte{128, 255, 64, 100, 10, 20, 30, 255} // straight alpha
	if !bytes.Equal(raw, want) {
		t.Errorf("raw = %v, want %v", raw, want)
	}
}

func TestEdgeFades(t *testing.T) {
	// At the top there's nothing above: no top fade, a full bottom one.
	if top, bot := edgeFades(0, 500, 60); top != 0 || bot != 1 {
		t.Errorf("at the top: %v %v", top, bot)
	}
	// Half a fade from the end, half a fade.
	if top, bot := edgeFades(470, 500, 60); top != 1 || bot != 0.5 {
		t.Errorf("near the end: %v %v", top, bot)
	}
	// Clear: the edge row is transparent, and the far side of the band untouched.
	img := image.NewRGBA(image.Rect(0, 0, 2, 10))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	fadeRows(img, 0, 10, true, 1, true, color.RGBA{})
	if a := img.RGBAAt(0, 0).A; a > 10 {
		t.Errorf("faded edge alpha %d", a)
	}
	if a := img.RGBAAt(0, 9).A; a < 190 {
		t.Errorf("inner side alpha %d", a)
	}
	mid := img.RGBAAt(0, 5)
	if mid.R != mid.A || mid.A < 60 || mid.A > 180 {
		t.Errorf("middle = %v, want premultiplied and partway", mid)
	}
	// Painted: it fades toward the background color instead.
	img2 := image.NewRGBA(image.Rect(0, 0, 1, 4))
	for i := range img2.Pix {
		img2.Pix[i] = 255
	}
	fadeRows(img2, 0, 4, false, 1, false, color.RGBA{0, 0, 0, 255})
	if c := img2.RGBAAt(0, 3); c.R > 30 || c.A != 255 {
		t.Errorf("painted bottom edge = %v", c)
	}
}

func TestRingHidesUntilProgressStarts(t *testing.T) {
	fonts, _ := loadFonts()
	l, _ := parseMarkdown("# Building\n- [ ] pour\n- [ ] frame\n")
	r := &renderer{fonts: fonts, scale: 2, pal: lightPalette}
	rows, _ := layout(l)
	ringAt := func(img *image.RGBA) color.RGBA {
		return img.RGBAAt(14, int(rows[0].center*2)) // on the ring, left of center
	}
	// leftInk is the leftmost dark pixel on the header row: where the title starts.
	leftInk := func(img *image.RGBA) int {
		y := int((rows[0].center + 2) * 2)
		for x := 0; x < 200; x++ {
			if c := img.RGBAAt(x, y); c.R < 120 && c.B < 120 { // the title's near-black, not the blue ring
				return x
			}
		}
		return -1
	}
	none := r.Render(l, 300, 100, 0)
	if ringAt(none) != lightPalette.bg {
		t.Error("a group with nothing done drew its ring")
	}
	l.Groups[0].Items[0].Status = StatusDone
	done := r.Render(l, 300, 100, 0)
	if ringAt(done) == lightPalette.bg {
		t.Error("no ring once something's done")
	}
	if a, b := leftInk(none), leftInk(done); a < 0 || b <= a+20 {
		t.Errorf("title starts at %d with no ring and %d with one; it should move over", a, b)
	}
	// Mid-animation the title is between the two.
	r.icons = map[string]float64{groupKeys(l)[&l.Groups[0]]: 0.5}
	if m := leftInk(r.Render(l, 300, 100, 0)); m <= leftInk(none) || m >= leftInk(done) {
		t.Errorf("halfway the title starts at %d", m)
	}
}

func TestDotsFollowTheText(t *testing.T) {
	fonts, _ := loadFonts()
	l, _ := parseMarkdown("- [~] Raise the steel frame\n")
	r := &renderer{fonts: fonts, scale: 2, pal: darkPalette, clear: true}
	without := r.Render(l, 400, 60, 0)
	r.dots = true
	with := r.Render(l, 400, 60, 0)
	textEnd := (marginLeft + textX + r.textWidth("Raise the steel frame", false, 0)) * 2
	minX, maxX := 9999, -1
	for y := 0; y < 60; y++ {
		for x := 0; x < 400; x++ {
			if with.RGBAAt(x, y) != without.RGBAAt(x, y) {
				minX, maxX = min(minX, x), max(maxX, x)
			}
		}
	}
	if maxX < 0 {
		t.Fatal("no dots drawn")
	}
	if float64(minX) < textEnd {
		t.Errorf("dots start at %dpx, before the text ends at %.0fpx", minX, textEnd)
	}
	if w := float64(maxX-minX+1) / 2; w < dotsW-1 || w > dotsW+1 {
		t.Errorf("dots are %vpt wide, want %v", w, dotsW)
	}
	// A long item wraps, leaving them room after its last line.
	l2, _ := parseMarkdown("- [~] " + strings.Repeat("very long item text ", 10) + "\n")
	right := 200/2.0 - marginRight
	r.wrapList(l2, right)
	if x := r.dotsX(&l2.Groups[0].Items[0], right, 1); x+dotsW > right+0.01 {
		t.Errorf("dots end at %v, past the row's end at %v", x+dotsW, right)
	}
}

func TestItemsWrap(t *testing.T) {
	for _, scale := range []float64{1, 2} {
		t.Run(fmt.Sprint(scale, "x"), func(t *testing.T) { testItemsWrap(t, scale) })
	}
}

func testItemsWrap(t *testing.T, scale float64) {
	r := &renderer{fonts: testFonts(t), scale: scale, pal: lightPalette}
	text := "Order the CNC cells and conveyors before the steel frame goes up"
	maxW := 160.0
	starts := r.wrapText(text, maxW)
	if len(starts) < 2 {
		t.Fatalf("a long item stayed on one line: %v", starts)
	}
	it := &Item{Text: text, lines: starts}
	lines, _ := itemLines(it)
	if got := strings.Join(lines, " "); got != text {
		t.Errorf("lines %q don't add back up to the text", lines)
	}
	for _, line := range lines {
		if w := r.textWidth(line, false, 0); w > maxW+0.5 {
			t.Errorf("line %q is %vpt, over %v", line, w, maxW)
		}
		if strings.HasPrefix(line, " ") || line == "" {
			t.Errorf("line %q starts with a space or is empty", line)
		}
	}
	// Breaks fall between words.
	for _, line := range lines[:len(lines)-1] {
		if !strings.Contains(text, line+" ") {
			t.Errorf("line %q ends mid-word", line)
		}
	}
	// A word longer than a line breaks mid-word rather than overflowing.
	long := strings.Repeat("x", 80)
	for _, line := range func() []string { l, _ := itemLines(&Item{Text: long, lines: r.wrapText(long, maxW)}); return l }() {
		if w := r.textWidth(line, false, 0); w > maxW+0.5 {
			t.Errorf("long word line is %vpt", w)
		}
	}
	// Short text is one line.
	if s := r.wrapText("Ship", maxW); len(s) != 1 {
		t.Errorf("short text wrapped: %v", s)
	}

	// The layout makes room for the extra lines, and the next item moves down.
	l, _ := parseMarkdown("# Plan\n- [ ] " + text + "\n- [ ] next\n")
	before, _ := layout(l)
	r.wrapList(l, wrapWidthInverse(maxW))
	after, _ := layout(l)
	ex := l.Groups[0].Items[0].extra()
	if ex <= 0 || after[2].center-before[2].center != ex || after[1].extra != ex {
		t.Errorf("extra %v; next item moved %v", ex, after[2].center-before[2].center)
	}
	// Wrapping again for the same width does nothing.
	if r.wrapList(l, wrapWidthInverse(maxW)) {
		t.Error("rewrapped for the same width")
	}
}

// wrapWidthInverse is the row end that gives lines maxW wide.
func wrapWidthInverse(maxW float64) float64 { return maxW + marginLeft + textX + dotsGap + dotsW }

func TestFindHighlightsOnWrappedLines(t *testing.T) {
	fonts, _ := loadFonts()
	l, _ := parseMarkdown("- [ ] Order the CNC cells and conveyors early since the lead time is long\n")
	r := &renderer{fonts: fonts, scale: 2, pal: lightPalette}
	const W = 360 // narrow enough to wrap
	plain := r.Render(l, W, 120, 0)
	it := &l.Groups[0].Items[0]
	if len(it.lines) < 2 {
		t.Fatalf("didn't wrap: %v", it.lines)
	}
	var f findState
	f.setQuery(l, "lead")
	r.marks = f.spans()
	marked := r.Render(l, W, 120, 0)
	rows, _ := layout(l)
	// "lead" is on a later line: that line changes, the first doesn't.
	line := func(i int) (changed bool) {
		y0 := int((rows[0].center - 9 + float64(i)*lineH) * 2)
		for y := y0; y < y0+36; y++ {
			for x := 0; x < W; x++ {
				if plain.RGBAAt(x, y) != marked.RGBAAt(x, y) {
					return true
				}
			}
		}
		return false
	}
	lines, _ := itemLines(it)
	at := -1
	for i, ln := range lines {
		if strings.Contains(ln, "lead") {
			at = i
		}
	}
	if at < 1 {
		t.Fatalf("lead isn't on a later line: %q", lines)
	}
	for i := range lines {
		if line(i) != (i == at) {
			t.Errorf("line %d (%q) changed: %v; only line %d should", i, lines[i], line(i), at)
		}
	}
}

func TestViewerCommandCarriesCallerSettings(t *testing.T) {
	t.Setenv("REXPAD_FONT", "/fonts/Mono Regular.otf")
	t.Setenv("REXPAD_FONT_BOLD", "")
	t.Setenv("REXPAD_SCALE", "2")
	t.Setenv("XDG_STATE_HOME", "/state")
	t.Setenv("REX_BLOCK", "")
	got := viewerCommand("/bin/rexpad", "l", showOptions{theme: "dark"})
	want := []string{"/usr/bin/env", "REXPAD_FONT=/fonts/Mono Regular.otf", "REXPAD_SCALE=2", "XDG_STATE_HOME=/state",
		"/bin/rexpad", "view", "--list", "l", "--theme", "dark"}
	if !slices.Equal(got, want) {
		t.Errorf("viewerCommand = %q\nwant %q", got, want)
	}
}

func TestViewerCommandWithoutSettings(t *testing.T) {
	for _, k := range viewerSettings {
		t.Setenv(k, "")
	}
	t.Setenv("REX_BLOCK", "")
	got := viewerCommand("/bin/rexpad", "l", showOptions{theme: "terminal"})
	want := []string{"/bin/rexpad", "view", "--list", "l", "--theme", "terminal"}
	if !slices.Equal(got, want) {
		t.Errorf("viewerCommand = %q\nwant %q", got, want)
	}
}

func TestShowPaneStartsViewerWithCallerSettings(t *testing.T) {
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	fakeRex := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\necho block-1\n"
	if err := os.WriteFile(filepath.Join(bin, "rex"), []byte(fakeRex), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REX_SERVER", "test")
	t.Setenv("REX_BLOCK", "") // one rex call: no block move afterwards
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_SCALE", "2")
	if err := showPane("l", showOptions{split: "right", ratio: 30, theme: "dark"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	want := strings.Join(viewerCommand(self, "l", showOptions{split: "right", ratio: 30, theme: "dark"}), "\n")
	if !strings.HasSuffix(strings.TrimSpace(string(got)), "\n--\n"+want) {
		t.Errorf("rex split argv:\n%s\nwant it to end with:\n--\n%s", got, want)
	}
}

// A setting the viewer reads but show doesn't pass on is silently lost in
// the pane, so every environment read outside show must be in viewerSettings.
func TestViewerSettingsCoverEveryEnvRead(t *testing.T) {
	passedAnotherWay := map[string]bool{
		"REXPAD_LIST": true, "REX_BLOCK": true, // show passes --list
		"REX_SERVER": true, // read only by show
	}
	// Any quoted setting name counts, so a read through a helper is found too.
	getenv := regexp.MustCompile(`"((?:REXPAD|XDG)_[A-Z_]+)"`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "pane.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range getenv.FindAllStringSubmatch(string(src), -1) {
			if key := m[1]; !passedAnotherWay[key] && !slices.Contains(viewerSettings, key) {
				t.Errorf("%s reads $%s, which show does not pass to the viewer", f, key)
			}
		}
	}
}

// fontFile writes Go's bundled regular font to a file, as a stand-in for a
// font the user names in $REXPAD_FONT.
func fontFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "font.ttf")
	if err := os.WriteFile(p, goregular.TTF, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// clearTextSettings keeps a developer's own $REXPAD_* values out of a test.
func clearTextSettings(t *testing.T) {
	for _, k := range []string{"REXPAD_FONT", "REXPAD_FONT_BOLD", "REXPAD_FONT_SIZE", "REXPAD_TEXT_GAMMA"} {
		t.Setenv(k, "")
	}
}

func mustLoadFonts(t *testing.T) *fontSet {
	t.Helper()
	fonts, err := loadFonts()
	if err != nil {
		t.Fatal(err)
	}
	return fonts
}

func testFonts(t *testing.T) *fontSet {
	t.Helper()
	clearTextSettings(t)
	return mustLoadFonts(t)
}

// installedFont is the first of paths that exists, or skips the test: some
// checks need a font with real shaping features, which the bundled Go fonts
// don't have.
func installedFont(t *testing.T, paths ...string) string {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skipf("none of these fonts is installed: %v", paths)
	return ""
}

// ink is how much alpha img holds in total, and the box around it.
func ink(img *image.RGBA) (sum int, box image.Rectangle) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if a := img.RGBAAt(x, y).A; a > 0 {
				sum += int(a)
				box = box.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	return sum, box
}

// drawText draws s alone on a transparent image and returns the image.
func drawText(r *renderer, s string, x float64, bold bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 400, 80))
	r.text(img, s, x, 40, bold, color.RGBA{A: 255}, 0)
	return img
}

func TestFontSettingsDefaults(t *testing.T) {
	fonts := testFonts(t)
	if fonts.size != 14 || fonts.regular.tracking != -0.011 || fonts.edge != nil || len(fonts.warnings) > 0 {
		t.Errorf("size %v, tracking %v, edge set %v, warnings %q; want 14, -0.011, false, none",
			fonts.size, fonts.regular.tracking, fonts.edge != nil, fonts.warnings)
	}
}

func TestFontSettingsFromEnvironment(t *testing.T) {
	clearTextSettings(t)
	t.Setenv("REXPAD_FONT", fontFile(t))
	t.Setenv("REXPAD_FONT_SIZE", "12")
	t.Setenv("REXPAD_TEXT_GAMMA", "1.4")
	fonts := mustLoadFonts(t)
	// SF Pro's tracking is wrong for any other font.
	if fonts.size != 12 || fonts.regular.tracking != 0 || len(fonts.warnings) > 0 {
		t.Errorf("size %v, tracking %v, warnings %q; want 12, 0, none", fonts.size, fonts.regular.tracking, fonts.warnings)
	}
	if fonts.edge == nil || fonts.edge[0] != 0 || fonts.edge[255] != 255 || fonts.edge[128] <= 128 {
		t.Errorf("gamma 1.4 should darken edges and keep 0 and 255: %v", fonts.edge)
	}
}

func TestFontSettingsIgnoreBadValues(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "junk.otf")
	if err := os.WriteFile(junk, []byte("not a font"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ font, size, gamma string }{
		{junk, "-3", "dark"},
		{"/no/such/font.otf", "17", "3.5"}, // past the caps
		{junk, "9.5", "0.4"},
	} {
		clearTextSettings(t)
		t.Setenv("REXPAD_FONT", c.font)
		t.Setenv("REXPAD_FONT_SIZE", c.size)
		t.Setenv("REXPAD_TEXT_GAMMA", c.gamma)
		fonts := mustLoadFonts(t)
		if fonts.size != 14 || fonts.regular.tracking != -0.011 || fonts.edge != nil {
			t.Errorf("%+v: size %v, tracking %v, edge set %v; want the defaults", c, fonts.size, fonts.regular.tracking, fonts.edge != nil)
		}
		if len(fonts.warnings) != 1 || !strings.Contains(fonts.warnings[0], c.font) {
			t.Errorf("%+v: warnings %q, want one that names the font", c, fonts.warnings)
		}
	}
}

func TestFontLoadsFromACollection(t *testing.T) {
	clearTextSettings(t)
	t.Setenv("REXPAD_FONT", installedFont(t, "/System/Library/Fonts/Menlo.ttc", "/System/Library/Fonts/Courier.ttc"))
	fonts := mustLoadFonts(t)
	if fonts.regular.tracking != 0 || len(fonts.warnings) > 0 {
		t.Errorf("tracking %v, warnings %q: the .ttc was not used", fonts.regular.tracking, fonts.warnings)
	}
}

func TestTextSnapsToWholePixelsBelow2x(t *testing.T) {
	fonts := testFonts(t)
	whole := func(scale float64) bool {
		run := (&renderer{fonts: fonts, scale: scale}).layout("Review the patch, then open the PR", false)
		for _, g := range run.glyphs {
			if g.x%64 != 0 {
				return false
			}
		}
		return run.width%64 == 0
	}
	if !whole(1) {
		t.Error("at 1x every glyph should start on a whole pixel")
	}
	if whole(2) {
		t.Error("at 2x glyphs keep their fractional positions, as the design has them")
	}
}

func TestTextStartsOnAWholePixelBelow2x(t *testing.T) {
	r := &renderer{fonts: testFonts(t), scale: 1}
	imgA := drawText(r, "a", 10.3, false)
	_, a := ink(imgA)
	imgB := drawText(r, "a", 10.7, false)
	_, b := ink(imgB)
	if a.Size() != b.Size() {
		t.Fatalf("ink boxes %v and %v differ in size", a, b)
	}
	for y := 0; y < a.Dy(); y++ {
		for x := 0; x < a.Dx(); x++ {
			pa := imgA.RGBAAt(a.Min.X+x, a.Min.Y+y)
			pb := imgB.RGBAAt(b.Min.X+x, b.Min.Y+y)
			if pa != pb {
				t.Fatalf("the same letter drew differently at x=10.3 and x=10.7 (%v vs %v at %d,%d)", pa, pb, x, y)
			}
		}
	}
}

func TestLayoutGlyphsMatchTheFont(t *testing.T) {
	clearTextSettings(t)
	t.Setenv("REXPAD_FONT", fontFile(t))
	fonts := mustLoadFonts(t)
	const s = "Ship it"
	run := (&renderer{fonts: fonts, scale: 2}).layout(s, false)
	var buf sfnt.Buffer
	var want []sfnt.GlyphIndex
	for _, c := range s {
		id, _ := fonts.regular.outlines.GlyphIndex(&buf, c)
		want = append(want, id)
	}
	var got []sfnt.GlyphIndex
	for _, g := range run.glyphs {
		got = append(got, g.id)
	}
	if !slices.Equal(got, want) {
		t.Errorf("shaped glyphs %v, want the font's own %v", got, want)
	}
}

func TestMeasureMatchesDrawnText(t *testing.T) {
	fonts := testFonts(t)
	for _, scale := range []float64{1, 2} {
		r := &renderer{fonts: fonts, scale: scale, pal: lightPalette}
		img := image.NewRGBA(image.Rect(0, 0, 800, 100))
		const s = "Wrap long items, and refresh the README"
		drawn, _ := r.text(img, s, 10, 40, false, lightPalette.text, 0)
		if measured := r.measure(s, false) / scale; measured != drawn {
			t.Errorf("at %vx: measured %v points, drew %v", scale, measured, drawn)
		}
	}
}

func TestFontSizeChangesText(t *testing.T) {
	sized := func(size string) (width float64, height int) {
		clearTextSettings(t)
		t.Setenv("REXPAD_FONT_SIZE", size)
		r := &renderer{fonts: mustLoadFonts(t), scale: 2}
		_, box := ink(drawText(r, "H", 10, false))
		return r.measure("Ship it", false), box.Dy()
	}
	w12, h12 := sized("12")
	w14, h14 := sized("14")
	if w12 >= w14 || h12 >= h14 {
		t.Errorf("12pt text is %vpx wide and %vpx tall, 14pt is %v and %v; want 12pt smaller in both", w12, h12, w14, h14)
	}
}

func TestTextGammaDarkensEdges(t *testing.T) {
	fonts := testFonts(t)
	r := &renderer{fonts: fonts, scale: 1}
	plain, _ := ink(drawText(r, "o", 10, false))
	fonts.edge = edgeCurve(2)
	darker, _ := ink(drawText(r, "o", 10, false))
	if darker <= plain {
		t.Errorf("ink with gamma 2 is %d, without %d; want more", darker, plain)
	}
}

func TestBoldTextUsesBoldOutlines(t *testing.T) {
	r := &renderer{fonts: testFonts(t), scale: 2}
	regular, _ := ink(drawText(r, "Plan", 10, false))
	bold, _ := ink(drawText(r, "Plan", 10, true))
	if bold <= regular*11/10 {
		t.Errorf("bold ink %d is not clearly more than regular %d", bold, regular)
	}
}

func TestShapingKernsWithSFPro(t *testing.T) {
	clearTextSettings(t)
	t.Setenv("REXPAD_FONT", installedFont(t, "/Library/Fonts/SF-Pro-Text-Regular.otf",
		filepath.Join(os.Getenv("HOME"), "Library/Fonts/SF-Pro-Text-Regular.otf")))
	r := &renderer{fonts: mustLoadFonts(t), scale: 2}
	if pair, apart := r.measure("AV", false), r.measure("A", false)+r.measure("V", false); pair >= apart {
		t.Errorf(`"AV" is %vpx, "A" and "V" apart %vpx; the kerning pair was not applied`, pair, apart)
	}
	// A find highlight that starts at V starts where V is drawn.
	run := r.layout("AV", false)
	if x := r.xAt("AV", run, 1); x != float64(run.glyphs[1].x)/64 || x >= r.measure("A", false) {
		t.Errorf("x of V is %v, glyph at %v, A alone %v", x, float64(run.glyphs[1].x)/64, r.measure("A", false))
	}
}

func TestShapingHealsTextureWithMonaspace(t *testing.T) {
	clearTextSettings(t)
	home := os.Getenv("HOME")
	t.Setenv("REXPAD_FONT", installedFont(t,
		filepath.Join(home, "Library/Fonts/MonaspaceArgon-Regular.otf"),
		"/Library/Fonts/MonaspaceArgon-Regular.otf",
		"/usr/share/fonts/opentype/monaspace/MonaspaceArgon-Regular.otf"))
	r := &renderer{fonts: mustLoadFonts(t), scale: 2}
	alone := r.layout("m", false).glyphs[0].id
	if healed := r.layout("mil", false).glyphs[0].id; healed == alone {
		t.Errorf(`"m" before "il" is glyph %d, the same as alone; contextual alternates were not applied`, healed)
	}
}

// fitByShortening is the plain way to fit s: drop a rune at a time until the
// rest and an ellipsis fit.
func fitByShortening(r *renderer, s string, bold bool, maxW float64) (string, int, bool) {
	if r.measure(s, bold) <= maxW*r.scale {
		return s, len(s), false
	}
	for len(s) > 0 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
		if r.measure(trimRight(s)+"…", bold) <= maxW*r.scale {
			break
		}
	}
	s = trimRight(s)
	return s + "…", len(s), true
}

func TestFitMatchesShortening(t *testing.T) {
	fonts := testFonts(t)
	texts := []string{
		"Order the CNC cells and conveyors before the steel frame goes up",
		"Review the patch, then open the PR on almonk/rexpad",
		"naïve café — straße, 日本語 mixed in",
	}
	for _, scale := range []float64{1, 2} {
		r := &renderer{fonts: fonts, scale: scale}
		for _, s := range texts {
			for _, maxW := range []float64{20, 60, 133.3, 250, 1000} {
				shown, kept, cut := r.fit(s, false, maxW)
				wShown, wKept, wCut := fitByShortening(r, s, false, maxW)
				if shown != wShown || kept != wKept || cut != wCut {
					t.Errorf("%vx, %q in %vpt: fit gave %q %d %v, want %q %d %v", scale, s, maxW, shown, kept, cut, wShown, wKept, wCut)
				}
			}
		}
	}
}

func TestXAtMatchesTheRun(t *testing.T) {
	r := &renderer{fonts: testFonts(t), scale: 1}
	const s = "Ship it…"
	run := r.layout(s, false)
	for i, g := range run.glyphs {
		at := len(string([]rune(s)[:i]))
		if x := r.xAt(s, run, at); x != float64(g.x)/64 {
			t.Errorf("x at byte %d is %v, glyph %d is at %v", at, x, i, float64(g.x)/64)
		}
	}
	if x := r.xAt(s, run, len(s)); x != float64(run.width)/64 {
		t.Errorf("x at the end is %v, the run is %v wide", x, float64(run.width)/64)
	}
}

// stderrOf runs f and returns what it wrote to stderr.
func stderrOf(t *testing.T, f func() error) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = write
	runErr := f()
	os.Stderr = saved
	write.Close()
	out, _ := io.ReadAll(read)
	if runErr != nil {
		t.Fatal(runErr)
	}
	return string(out)
}

func TestShowAndRenderWarnAboutAnUnusableFont(t *testing.T) {
	clearTextSettings(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_LIST", "w")
	t.Setenv("REXPAD_FONT", "/no/such/font.otf")
	if err := run([]string{"add", "an item"}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "rex"), []byte("#!/bin/sh\necho block-1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REX_SERVER", "test")
	t.Setenv("REX_BLOCK", "")
	png := filepath.Join(t.TempDir(), "list.png")
	for _, args := range [][]string{{"render", "-o", png}, {"show"}} {
		if got := stderrOf(t, func() error { return run(args) }); !strings.Contains(got, "/no/such/font.otf") {
			t.Errorf("%v wrote %q to stderr, want a warning that names the font", args, got)
		}
	}
}

func TestConcurrentAddsKeepEveryItem(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_LIST", "race")
	const writers = 40
	errs := make(chan error, writers)
	for i := range writers {
		go func() { errs <- run([]string{"add", fmt.Sprint("item ", i)}) }()
	}
	for range writers {
		if err := <-errs; err != nil {
			t.Errorf("add: %v", err)
		}
	}
	l, err := loadList(listPath("race"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(l.Groups[0].Items); n != writers {
		t.Errorf("the list kept %d of %d items: concurrent writes lost updates", n, writers)
	}
}

func TestToggleByUser(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := listPath("toggle")
	saveMarkdown(t, path, "- [ ] pending {#1}\n- [~] active {#2}\n- [x] done {#3}\n")
	want := map[int]Status{1: StatusDone, 2: StatusDone, 3: StatusPending}
	for id, shown := range map[int]Status{1: StatusPending, 2: StatusActive, 3: StatusDone} {
		if err := toggleByUser(path, id, shown); err != nil {
			t.Fatal(err)
		}
	}
	l := mustLoad(t, path)
	for id, st := range want {
		if it := l.find(id); it.Status != st || it.By != byUser {
			t.Errorf("item %d = %s by %q, want %s by %q", id, it.Status, it.By, st, byUser)
		}
	}
	// The click toggles what the viewer showed, even if the file moved on.
	if err := toggleByUser(path, 1, StatusPending); err != nil {
		t.Fatal(err)
	}
	if l, _ = loadList(path); l.find(1).Status != StatusDone {
		t.Errorf("a click on a box shown open made it %s, want done", l.find(1).Status)
	}
	if err := toggleByUser(path, 99, StatusPending); err == nil {
		t.Error("toggling a missing item should fail")
	}
}

func TestCommandsClearTheUserMark(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_LIST", "marks")
	path := listPath("marks")
	reset := func() {
		t.Helper()
		l, _ := parseMarkdown("- [ ] one {#1}\n- [~] two {#2}\n- [x] three {#3}\n")
		for _, g := range l.Groups {
			for i := range g.Items {
				g.Items[i].By = byUser
			}
		}
		if err := saveList(path, l); err != nil {
			t.Fatal(err)
		}
	}
	cases := [][]string{
		{"done", "1"}, {"undo", "1"}, {"start", "1"}, {"edit", "1", "new text"},
		{"done"}, {"next"}, {"set"},
	}
	for _, args := range cases {
		reset()
		if args[0] == "set" {
			// set replaces every item, even with the same text and status.
			out, _ := json.Marshal(mustLoad(t, path))
			withStdin(t, string(out), func() { captureStdout(t, func() { mustRun(t, "set", "--json") }) })
		} else {
			captureStdout(t, func() { mustRun(t, args...) })
		}
		l := mustLoad(t, path)
		for _, g := range l.Groups {
			for _, it := range g.Items {
				touched := args[0] == "set" ||
					(args[0] == "next" && it.ID <= 2) || // finishes 2, starts 1
					(len(args) > 1 && it.ID == 1) ||
					(args[0] == "start" && it.ID == 2) || // demoted to pending
					(args[0] == "done" && len(args) == 1 && it.ID == 2)
				if touched && it.By != "" {
					t.Errorf("%v: item %d kept by %q", args, it.ID, it.By)
				}
				if !touched && it.By != byUser {
					t.Errorf("%v: item %d lost the user's mark", args, it.ID)
				}
			}
		}
	}
}

func TestGetShowsTheUserMarkOnlyInJSON(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_LIST", "get")
	path := listPath("get")
	saveMarkdown(t, path, "- [ ] one {#1}\n")
	if err := toggleByUser(path, 1, StatusPending); err != nil {
		t.Fatal(err)
	}
	if got := captureStdout(t, func() { mustRun(t, "get") }); got != "- [x] one {#1}\n" {
		t.Errorf("get = %q", got)
	}
	if got := captureStdout(t, func() { mustRun(t, "get", "--json") }); !strings.Contains(got, `"by": "user"`) {
		t.Errorf("get --json has no by field:\n%s", got)
	}
}

func mustRun(t *testing.T, args ...string) {
	t.Helper()
	if err := run(args); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
}

func mustLoad(t *testing.T, path string) *List {
	t.Helper()
	l, err := loadList(path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// saveMarkdown writes the checklist md as the list at path.
func saveMarkdown(t *testing.T, path, md string) {
	t.Helper()
	l, err := parseMarkdown(md)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveList(path, l); err != nil {
		t.Fatal(err)
	}
}

// captureStdout runs fn and returns what it printed.
func captureStdout(t *testing.T, fn func()) string { return capture(t, &os.Stdout, fn) }

// capture runs fn and returns what it wrote to *f, such as os.Stderr.
func capture(t *testing.T, f **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := *f
	*f = w
	defer func() { *f = old }()
	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// withStdin runs fn with input on stdin.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(input)
	f.Seek(0, 0)
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old; f.Close() }()
	fn()
}

func TestWaitForUserReturnsMarkedItems(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := listPath("wait")
	saveMarkdown(t, path, "# Mine\n- [ ] agent's {#1}\n# Yours\n- [ ] log in {#2}\n- [ ] approve {#3}\n")
	// A click before the wait starts still counts: an agent cannot start
	// waiting at the same instant it adds the step.
	if err := toggleByUser(path, 2, StatusPending); err != nil {
		t.Fatal(err)
	}
	got, err := waitForUser(path, nil, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if md := got.markdown(true); md != "# Yours\n- [x] log in {#2}\n" {
		t.Errorf("wait printed %q", md)
	}

	// With IDs, only those items count.
	start := time.Now()
	go func() {
		time.Sleep(40 * time.Millisecond)
		toggleByUser(path, 3, StatusPending)
	}()
	got, err = waitForUser(path, []int{1, 3}, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if md := got.markdown(true); md != "# Yours\n- [x] approve {#3}\n" {
		t.Errorf("wait 1 3 printed %q", md)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Error("wait 1 3 returned before the user changed 1 or 3")
	}
}

func TestWaitForUserFailsOnMissingItems(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := listPath("waitgone")
	saveMarkdown(t, path, "- [ ] one {#1}\n- [ ] two {#2}\n")
	if _, err := waitForUser(path, []int{9}, time.Millisecond); err == nil {
		t.Error("wait on an item that does not exist should fail")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		updateList(path, func(l *List) error { l.remove(2); return nil })
	}()
	done := make(chan error, 1)
	go func() { _, err := waitForUser(path, []int{2}, time.Millisecond); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("wait should fail once its items are removed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait kept waiting on a removed item")
	}
}

func TestWaitCommandPrintsTheGetFormat(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_LIST", "waitcmd")
	path := listPath("waitcmd")
	saveMarkdown(t, path, "- [x] one {#1}\n")
	if err := toggleByUser(path, 1, StatusDone); err != nil {
		t.Fatal(err)
	}
	if got := captureStdout(t, func() { mustRun(t, "wait", "1") }); got != "- [ ] one {#1}\n" {
		t.Errorf("wait printed %q", got)
	}
}

func TestDecodeClicks(t *testing.T) {
	var got []mouseClick
	var d keyDecoder
	d.feed([]byte("\x1b[I\x1b[<0;122;155M\x1b[<0;122;155m\x1b[<64;5;5M\x1b[<1;3;4M\x1b[<4;3;4M\x1b[<32;3;4M"), func(ev termEvent) {
		if ev.click != nil {
			got = append(got, *ev.click)
		}
	})
	// Only plain left-button presses and releases: not the wheel, the
	// middle button, Shift-click, or a drag.
	want := []mouseClick{{x: 122, y: 155}, {x: 122, y: 155, release: true}}
	if !slices.Equal(got, want) {
		t.Errorf("clicks = %+v, want %+v", got, want)
	}
}

// itemID is the item's ID, or 0 for no item.
func itemID(it *Item) int {
	if it == nil {
		return 0
	}
	return it.ID
}

func TestHitItem(t *testing.T) {
	l, _ := parseMarkdown("# G\n- [ ] a {#1}\n- [ ] b {#2}\n# H\n- [ ] c {#3}\n")
	l.find(2).lines = []int{0, 1} // b wraps to two lines
	rows, _ := layout(l)
	center := map[int]float64{}
	for _, rw := range rows {
		if rw.kind == rowItem {
			center[rw.item.ID] = rw.center
		}
	}
	boxX := marginLeft + boxCenter
	cases := []struct {
		name string
		x, y float64
		want int
	}{
		{"on a box", boxX, center[1], 1},
		{"at the pane's left edge", 0, center[3], 3},
		{"near the top of the row", boxX, center[1] - itemPitch/2 + 1, 1},
		{"in a wrapped item's second line", boxX, center[2] + lineH, 2},
		{"on the text", marginLeft + textX + 4, center[1], 0},
		{"on a group title", boxX, rows[0].center, 0},
		{"between a title and its first item", boxX, rows[0].center + 16, 0},
		{"below the list", boxX, center[3] + itemPitch, 0},
	}
	for _, c := range cases {
		if got := itemID(hitItem(l, nil, c.x, c.y)); got != c.want {
			t.Errorf("%s: hit %d, want %d", c.name, got, c.want)
		}
	}

	// While find folds rows away, a click hits the row drawn there, and
	// never one still folding.
	geo, _ := geoFor(l, []bool{true, false, true, true, true})
	if got := itemID(hitItem(l, geo, boxX, geo[2].center)); got != 2 {
		t.Errorf("folded: hit %d where item 2 is drawn, want 2", got)
	}
	if got := itemID(hitItem(l, geo, boxX, center[1])); got == 1 {
		t.Error("folded: hit item 1, which is folded away")
	}
	geo[4].open = 0.5
	if got := itemID(hitItem(l, geo, boxX, geo[4].center)); got != 0 {
		t.Errorf("hit item %d while it was still folding", got)
	}
}

func TestClickedItemUsesTheFrameOnScreen(t *testing.T) {
	l, _ := parseMarkdown("- [ ] a {#1}\n- [ ] b {#2}\n- [ ] c {#3}\n")
	rows, _ := layout(l)
	const scale = 2.0
	boxPx := int((marginLeft + boxCenter) * scale)
	at := func(id int) int { return int(rows[id-1].center * scale) }
	if got := itemID(clickedAt(l, nil, mouseClick{x: boxPx, y: at(2)}, scale, 0, 1000).item); got != 2 {
		t.Errorf("unscrolled: hit %d, want 2", got)
	}
	// Scrolled down one row: item 3 is drawn where item 2 was.
	if got := itemID(clickedAt(l, nil, mouseClick{x: boxPx, y: at(2)}, scale, int(itemPitch*scale), 1000).item); got != 3 {
		t.Errorf("scrolled: hit %d, want 3", got)
	}
	// The find bar covers the pane below viewPx.
	if got := itemID(clickedAt(l, nil, mouseClick{x: boxPx, y: at(3)}, scale, 0, at(3)-10).item); got != 0 {
		t.Errorf("a click on the find bar hit item %d", got)
	}
}

func TestViewArgs(t *testing.T) {
	// A click keeps focus in the viewer's pane, so the agent's pane is not
	// passed to the viewer.
	t.Setenv("REX_BLOCK", "block:agent")
	got := viewArgs("/bin/rexpad", "mylist", showOptions{theme: "dark"})
	want := []string{"/bin/rexpad", "view", "--list", "mylist", "--theme", "dark"}
	if !slices.Equal(got, want) {
		t.Errorf("viewArgs = %q, want %q", got, want)
	}
	got = viewArgs("/bin/rexpad", "mylist", showOptions{theme: "dark", noClick: true})
	want = []string{"/bin/rexpad", "view", "--list", "mylist", "--theme", "dark", "--no-click"}
	if !slices.Equal(got, want) {
		t.Errorf("viewArgs = %q, want %q", got, want)
	}
}

func TestParseSGRMouseRejectsMalformedReports(t *testing.T) {
	for _, in := range []string{"<0;12M", "<a;1;1M", "<0;1;1", "<0;1;1;9M", "<0;;1M", "0;1;1M"} {
		if m, ok := parseSGRMouse(in); ok {
			t.Errorf("parseSGRMouse(%q) = %+v, want no report", in, m)
		}
	}
	if k := csiScroll("<64;5;5m"); k != scrollNone {
		t.Errorf("a wheel release scrolled: %v", k)
	}
}

func TestClickTrackerPairsPressAndRelease(t *testing.T) {
	// Each step is a press (down) or a release (up) on an item's box, 0 for
	// no box. want is what each step toggles.
	down := func(id int) mouseClick { return mouseClick{x: id} }
	up := func(id int) mouseClick { return mouseClick{x: id, release: true} }
	cases := []struct {
		name  string
		steps []mouseClick
		want  []int
	}{
		{"down and up on one box", []mouseClick{down(1), up(1)}, []int{0, 1}},
		{"down on one box, up on another", []mouseClick{down(1), up(2)}, []int{0, 0}},
		{"down off a box, up on one", []mouseClick{down(0), up(2)}, []int{0, 0}},
		{"up with no press", []mouseClick{up(3)}, []int{0}},
		{"a second press replaces the first", []mouseClick{down(1), down(2), up(2)}, []int{0, 0, 2}},
		{"one press, two releases", []mouseClick{down(1), up(1), up(1)}, []int{0, 1, 0}},
		{"a release off the box ends the press", []mouseClick{down(1), up(0), up(1)}, []int{0, 0, 0}},
	}
	for _, c := range cases {
		var tr clickTracker
		var got []int
		for _, s := range c.steps {
			got = append(got, tr.add(s, clickTarget{item: s.x}).item) // the step's x stands in for the box it is on
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: toggles %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHitItemMatchesRenderWithStaleGeo(t *testing.T) {
	l, _ := parseMarkdown("- [ ] a {#1}\n- [ ] b {#2}\n")
	rows, _ := layout(l)
	// One row short, as if made for another version of the list, and with
	// item 2 folded away. Render ignores it and draws the resting layout,
	// so a click hits the rows where that layout puts them.
	stale := []rowGeo{{rows[0].center, 0}}
	if it := hitItem(l, stale, marginLeft+boxCenter, rows[1].center); it == nil || it.ID != 2 {
		t.Errorf("hit %v, want item 2 where Render draws it", it)
	}
}

func TestDecodeModeReports(t *testing.T) {
	var got []modeReport
	var d keyDecoder
	d.feed([]byte("\x1b[?1016;1$y\x1b[?1016;0$y\x1b[?2026;3$y\x1b[?1$y"), func(ev termEvent) {
		if ev.mode != nil {
			got = append(got, *ev.mode)
		}
		if ev.key.code != 0 {
			t.Errorf("a mode report came out as key %+v", ev.key)
		}
	})
	want := []modeReport{{1016, 1}, {1016, 0}, {2026, 3}}
	if !slices.Equal(got, want) {
		t.Errorf("reports = %+v, want %+v", got, want)
	}
	if !(modeReport{1016, 1}).on() || !(modeReport{1016, 3}).on() || (modeReport{1016, 0}).on() || (modeReport{1016, 2}).on() {
		t.Error("on() should be true only for set (1) and permanently set (3)")
	}
}

func TestWaitErrorsNameTheMissingItems(t *testing.T) {
	l, _ := parseMarkdown("- [ ] one {#1}\n")
	if err := checkWatched(l, []int{9}); err == nil || err.Error() != "the list has no item 9" {
		t.Errorf("one ID: %v", err)
	}
	if err := checkWatched(l, []int{8, 9}); err == nil || err.Error() != "the list has none of the items 8, 9" {
		t.Errorf("two IDs: %v", err)
	}
}

func TestWaitWarnsWithNoViewerOpen(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_LIST", "nopane")
	path := listPath("nopane")
	saveMarkdown(t, path, "- [ ] one {#1}\n")
	if err := toggleByUser(path, 1, StatusPending); err != nil {
		t.Fatal(err)
	}
	stderr := capture(t, &os.Stderr, func() { captureStdout(t, func() { mustRun(t, "wait") }) })
	if !strings.Contains(stderr, "no viewer pane") {
		t.Errorf("wait with no pane open warned %q", stderr)
	}
}

// keyOf is the key that groupKeys gives group gi of l.
func keyOf(l *List, gi int) string { return groupKeys(l)[&l.Groups[gi]] }

func TestCollapsedIsStoredButNotInMarkdown(t *testing.T) {
	const md = "# A\n- [ ] a {#1}\n\n# B\n- [ ] b {#2}\n"
	l, _ := parseMarkdown(md)
	l.Groups[0].Collapsed = true
	data, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), `"collapsed":true`); n != 1 {
		t.Errorf("JSON has %d collapsed fields, want 1 (left out when false): %s", n, data)
	}
	var back List
	if err := json.Unmarshal(data, &back); err != nil || !back.Groups[0].Collapsed || back.Groups[1].Collapsed {
		t.Errorf("round trip lost the state: %+v, %v", back.Groups, err)
	}
	if got := l.markdown(true); got != md {
		t.Errorf("markdown = %q, want %q", got, md)
	}
}

func TestToggleCollapsedByUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.json")
	saveMarkdown(t, path, "# A\n- [~] a {#1}\n# A\n- [ ] b {#2}\n")
	key := keyOf(mustLoad(t, path), 1) // the second group titled A
	if err := toggleCollapsedByUser(path, key, false); err != nil {
		t.Fatal(err)
	}
	l := mustLoad(t, path)
	if l.Groups[0].Collapsed || !l.Groups[1].Collapsed {
		t.Fatalf("collapsed = %v, %v; want only the second group", l.Groups[0].Collapsed, l.Groups[1].Collapsed)
	}
	if it := l.find(1); it.Status != StatusActive || it.By != "" {
		t.Errorf("collapsing changed an item: %+v", *it)
	}
	// A click means what the user saw: a second click from a stale frame
	// that still showed the group open keeps it collapsed.
	if err := toggleCollapsedByUser(path, key, false); err != nil {
		t.Fatal(err)
	}
	if !mustLoad(t, path).Groups[1].Collapsed {
		t.Error("a click on a group shown open expanded it")
	}
	if err := toggleCollapsedByUser(path, key, true); err != nil {
		t.Fatal(err)
	}
	if mustLoad(t, path).Groups[1].Collapsed {
		t.Error("a click on a group shown collapsed did not expand it")
	}
	if err := toggleCollapsedByUser(path, "Gone\x000", false); err == nil {
		t.Error("a click on a group that is gone should fail")
	}
}

func TestSetKeepsCollapsedGroups(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_LIST", "setcollapse")
	path := listPath("setcollapse")
	set := func(input string, args ...string) *List {
		t.Helper()
		withStdin(t, input, func() { captureStdout(t, func() { mustRun(t, append([]string{"set"}, args...)...) }) })
		return mustLoad(t, path)
	}
	set("# Build\n- [ ] a\n# Ship\n- [ ] b\n")
	if err := toggleCollapsedByUser(path, "Build\x000", false); err != nil {
		t.Fatal(err)
	}
	l := set("# Release\n- [ ] c\n# Build\n- [ ] a2\n")
	if !l.Groups[1].Collapsed || l.Groups[0].Collapsed {
		t.Errorf("after set: Release %v, Build %v; want only Build collapsed", l.Groups[0].Collapsed, l.Groups[1].Collapsed)
	}
	if l = set("# Built\n- [ ] a\n"); l.Groups[0].Collapsed {
		t.Error("a renamed group kept the collapsed state")
	}
	l = set(`{"groups":[{"title":"Docs","collapsed":true,"items":[{"text":"d"}]}]}`, "--json")
	if !l.Groups[0].Collapsed {
		t.Error("set --json dropped a collapsed field it was given")
	}
	if got := captureStdout(t, func() { mustRun(t, "get") }); strings.Contains(got, "collapsed") {
		t.Errorf("get shows the state: %q", got)
	}
}

func TestSetPairsGroupsThatShareATitleInOrder(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_LIST", "sametitle")
	path := listPath("sametitle")
	set := func(input string) *List {
		t.Helper()
		withStdin(t, input, func() { captureStdout(t, func() { mustRun(t, "set") }) })
		return mustLoad(t, path)
	}
	set("# A\n- [ ] a\n# A\n- [ ] b\n")
	if err := toggleCollapsedByUser(path, "A\x001", false); err != nil {
		t.Fatal(err)
	}
	if l := set("# A\n- [ ] a\n# A\n- [ ] b\n"); l.Groups[0].Collapsed || !l.Groups[1].Collapsed {
		t.Errorf("collapsed = %v, %v; want only the second A", l.Groups[0].Collapsed, l.Groups[1].Collapsed)
	}
	// With one A left, it is the first A, which was open.
	if l := set("# A\n- [ ] a\n"); l.Groups[0].Collapsed {
		t.Error("the one A left took the second A's state")
	}
}

func TestSetReplacesAnUnreadableList(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_LIST", "broken")
	path := listPath("broken")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr := capture(t, &os.Stderr, func() {
		withStdin(t, "# G\n- [ ] a\n", func() { captureStdout(t, func() { mustRun(t, "set") }) })
	})
	if l := mustLoad(t, path); len(l.Groups) != 1 || l.Groups[0].Title != "G" {
		t.Errorf("set did not replace the broken list: %+v", l.Groups)
	}
	if !strings.Contains(stderr, "collapsed") {
		t.Errorf("set gave no warning that it could not keep collapsed groups: %q", stderr)
	}
}

func TestCommandsKeepGroupsCollapsed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("REXPAD_LIST", "keepcollapse")
	path := listPath("keepcollapse")
	saveMarkdown(t, path, "# Build\n- [ ] a {#1}\n- [ ] b {#2}\n# Ship\n- [ ] c {#3}\n")
	if err := toggleCollapsedByUser(path, "Build\x000", false); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "start", "1")
	mustRun(t, "done", "2")
	mustRun(t, "edit", "1", "a, edited")
	captureStdout(t, func() { mustRun(t, "add", "-g", "Build", "d") })
	mustRun(t, "group", "Build") // moves it last
	l := mustLoad(t, path)
	if g := l.group("Build"); g == nil || !g.Collapsed {
		t.Errorf("an agent command expanded the group: %+v", g)
	}
}

// collapsedList parses md and collapses the groups at the given indices.
func collapsedList(t *testing.T, md string, groups ...int) *List {
	t.Helper()
	l, err := parseMarkdown(md)
	if err != nil {
		t.Fatal(err)
	}
	for _, gi := range groups {
		l.Groups[gi].Collapsed = true
	}
	return l
}

func TestLayoutFoldsCollapsedGroups(t *testing.T) {
	const md = "# A\n- [ ] a {#1}\n- [ ] b {#2}\n# B\n- [ ] c {#3}\n"
	l := collapsedList(t, md, 0)
	rows, h := layout(l)
	if len(rows) != 5 {
		t.Fatalf("%d rows, want 5: a collapsed group keeps its rows", len(rows))
	}
	var opens []float64
	for _, rw := range rows {
		opens = append(opens, rw.open)
	}
	if want := []float64{1, 0, 0, 1, 1}; !slices.Equal(opens, want) {
		t.Errorf("open = %v, want %v", opens, want)
	}
	// It takes the room of a group with no items.
	empty, _ := parseMarkdown("# A\n# B\n- [ ] c {#3}\n")
	erows, eh := layout(empty)
	if rows[3].center != erows[1].center || rows[4].center != erows[2].center || h != eh {
		t.Errorf("B at %v, c at %v, height %v; want %v, %v, %v as with an empty A",
			rows[3].center, rows[4].center, h, erows[1].center, erows[2].center, eh)
	}
	open, _ := parseMarkdown(md)
	openRows, _ := layout(open)
	for i, rw := range openRows {
		if rw.open != 1 {
			t.Errorf("open list: row %d open %v, want 1", i, rw.open)
		}
	}
	// A group with no title has no header to click, so it never collapses.
	untitled := collapsedList(t, "- [ ] a {#1}\n", 0)
	if r, _ := layout(untitled); r[0].open != 1 {
		t.Error("an untitled group collapsed")
	}
}

func TestGeoForFoldsCollapsedGroupsUnlessFindShowsThem(t *testing.T) {
	l := collapsedList(t, "# A\n- [ ] apple {#1}\n- [ ] pear {#2}\n# B\n- [ ] plum {#3}\n", 0)
	g, _ := geoFor(l, nil)
	if g[1].open != 0 || g[2].open != 0 || g[4].open != 1 {
		t.Errorf("at rest: %+v, want A's items folded", g)
	}
	// Find shows matching items even in a collapsed group, and leaves the
	// stored state alone.
	var f findState
	f.setQuery(l, "pear")
	g, _ = geoFor(l, f.vis)
	if g[0].open != 1 || g[1].open != 0 || g[2].open != 1 {
		t.Errorf("finding pear: %+v, want A's header and pear open", g)
	}
	if !l.Groups[0].Collapsed {
		t.Error("find changed the stored state")
	}
	f.setQuery(l, "")
	if g, _ = geoFor(l, f.vis); g[2].open != 0 {
		t.Error("with the query cleared, the collapsed group stayed open")
	}
}

func TestCollapseAndExpandFold(t *testing.T) {
	const md = "# A\n- [ ] a {#1}\n- [ ] b {#2}\n# B\n- [ ] c {#3}\n"
	for _, c := range []struct {
		name     string
		from, to *List
	}{
		{"collapse", collapsedList(t, md), collapsedList(t, md, 0)},
		{"expand", collapsedList(t, md, 0), collapsedList(t, md)},
	} {
		from := arrivalGeo(c.from, nil, c.to)
		to, _ := geoFor(c.to, nil)
		if from == nil || slices.Equal(from, to) {
			t.Errorf("%s: rows jump into place instead of folding", c.name)
			continue
		}
		g := slices.Clone(from)
		for i := 0; i < 200 && !easeGeo(c.to, g, to, 15*time.Millisecond); i++ {
		}
		if !slices.Equal(g, to) {
			t.Errorf("%s: settled at %+v, want %+v", c.name, g, to)
		}
	}
}

func TestRenderHidesCollapsedItems(t *testing.T) {
	fonts, err := loadFonts()
	if err != nil {
		t.Fatal(err)
	}
	const md = "# A\n- [~] a {#1}\n# B\n- [ ] b {#2}\n"
	hasActiveRow := func(l *List) bool {
		img := (&renderer{fonts: fonts, scale: 1, pal: lightPalette}).Render(l, 400, 300, 0)
		for y := 0; y < 300; y++ {
			if img.RGBAAt(20, y) == lightPalette.activeBg {
				return true
			}
		}
		return false
	}
	if !hasActiveRow(collapsedList(t, md)) {
		t.Fatal("the open group's active row is not drawn")
	}
	if hasActiveRow(collapsedList(t, md, 0)) {
		t.Error("the collapsed group's active row is drawn")
	}
}

func TestHitItemSkipsCollapsedItems(t *testing.T) {
	l := collapsedList(t, "# A\n- [ ] a {#1}\n- [ ] b {#2}\n# B\n- [ ] c {#3}\n", 0)
	rows, _ := layout(l)
	boxX := marginLeft + boxCenter
	for y := 0.0; y < rows[4].center+itemPitch; y++ {
		if id := itemID(hitItem(l, nil, boxX, y)); id == 1 || id == 2 {
			t.Fatalf("hit collapsed item %d at y %v", id, y)
		}
	}
	if id := itemID(hitItem(l, nil, boxX, rows[4].center)); id != 3 {
		t.Errorf("hit %d where item 3 is drawn, want 3", id)
	}
}

func TestFollowGoesToTheHeaderOfACollapsedGroup(t *testing.T) {
	l := &List{}
	many := func(title string) Group {
		g := Group{Title: title}
		for i := 0; i < 30; i++ {
			g.Items = append(g.Items, Item{Text: "item", Status: StatusDone})
		}
		return g
	}
	later := many("Later")
	later.Items[20].Status = StatusActive
	later.Collapsed = true
	l.Groups = []Group{many("First"), later, many("Tail")}
	l.normalize()
	rows, _ := layout(l)
	header := -1.0
	for _, rw := range rows {
		if rw.kind == rowHeader && rw.group.Title == "Later" {
			header = rw.center
		}
	}
	const viewH = 300.0
	if off, _ := followOffset(l, viewH); off != header-viewH/3 {
		t.Errorf("offset %v, want %v: the collapsed group's header a third of the way down", off, header-viewH/3)
	}
	// With nothing active, the first open item leads the same way.
	l.Groups[1].Items[20].Status = StatusPending
	if off, _ := followOffset(l, viewH); off != header-viewH/3 {
		t.Errorf("no active item: offset %v, want %v", off, header-viewH/3)
	}
}

// groupTitle is the group's title, or "" for no group.
func groupTitle(g *Group) string {
	if g == nil {
		return ""
	}
	return g.Title
}

func TestHitHeader(t *testing.T) {
	l, _ := parseMarkdown("# A\n- [ ] a {#1}\n# B\n- [ ] b {#2}\n")
	rows, _ := layout(l)
	a, b := rows[0].center, rows[2].center
	cases := []struct {
		name string
		x, y float64
		want string
	}{
		{"on the title", marginLeft + titleX + 5, a, "A"},
		{"on the ring", marginLeft + iconCenterX, a, "A"},
		{"at the pane's left edge", 0, a, "A"},
		{"on the arrow at the right", 380, a, "A"},
		{"half a row above the center", 100, a - itemPitch/2, "A"},
		{"just above the divider", 100, a + dividerOffset - 0.5, "A"},
		{"on the second header", 100, b, "B"},
		{"above the header", 100, a - itemPitch/2 - 1, ""},
		{"on the divider", 100, a + dividerOffset + 0.5, ""},
		{"on an item", 100, rows[1].center, ""},
		{"left of the pane", -1, a, ""},
	}
	for _, c := range cases {
		if got := groupTitle(hitHeader(l, nil, c.x, c.y)); got != c.want {
			t.Errorf("%s: hit %q, want %q", c.name, got, c.want)
		}
	}
	// While rows fold, a click hits the header where it is drawn, and never
	// one that is still folding.
	geo, _ := geoFor(l, nil)
	geo[2].center += 10
	if got := groupTitle(hitHeader(l, geo, 100, geo[2].center)); got != "B" {
		t.Errorf("moved: hit %q, want B", got)
	}
	geo[2].open = 0.5
	if got := groupTitle(hitHeader(l, geo, 100, geo[2].center)); got != "" {
		t.Errorf("hit %q while it was still folding", got)
	}
}

func TestClickedAtTellsBoxesFromHeaders(t *testing.T) {
	l, _ := parseMarkdown("# A\n- [ ] a {#1}\n")
	rows, _ := layout(l)
	const scale = 2.0
	px := func(x, y float64) mouseClick { return mouseClick{x: int(x * scale), y: int(y * scale)} }
	if h := clickedAt(l, nil, px(marginLeft+boxCenter, rows[1].center), scale, 0, 1000); itemID(h.item) != 1 || h.group != nil {
		t.Errorf("a click on the box hit %+v, want item 1", h)
	}
	h := clickedAt(l, nil, px(100, rows[0].center), scale, 0, 1000)
	if h.item != nil || groupTitle(h.group) != "A" {
		t.Errorf("a click on the header hit %+v, want group A", h)
	}
	if got := h.target(l); got != (clickTarget{group: keyOf(l, 0)}) {
		t.Errorf("header target = %+v", got)
	}
	if h := clickedAt(l, nil, px(100, rows[1].center), scale, 0, 1000); h != (clickHit{}) {
		t.Errorf("a click on an item's text hit %+v, want nothing", h)
	}
	// The find bar covers the pane below viewPx, headers too.
	c := px(100, rows[0].center)
	if h := clickedAt(l, nil, c, scale, 0, c.y-1); h != (clickHit{}) {
		t.Errorf("a click on the find bar hit %+v", h)
	}
}

func TestClickTrackerTellsHeadersFromBoxes(t *testing.T) {
	headerA, headerB, box := clickTarget{group: "A\x000"}, clickTarget{group: "B\x000"}, clickTarget{item: 1}
	cases := []struct {
		name         string
		press, relse clickTarget
		want         clickTarget
	}{
		{"down and up on one header", headerA, headerA, headerA},
		{"down on a header, up on a box", headerA, box, clickTarget{}},
		{"down on a box, up on a header", box, headerA, clickTarget{}},
		{"down on one header, up on another", headerA, headerB, clickTarget{}},
	}
	for _, c := range cases {
		var tr clickTracker
		tr.add(mouseClick{}, c.press)
		if got := tr.add(mouseClick{release: true}, c.relse); got != c.want {
			t.Errorf("%s: toggles %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestHeaderArrowShowsState(t *testing.T) {
	fonts, err := loadFonts()
	if err != nil {
		t.Fatal(err)
	}
	const scale, widthPt = 2.0, 400.0
	l := collapsedList(t, "# A\n- [ ] a {#1}\n")
	rows, _ := layout(l)
	ax, cy := widthPt-marginRight-arrowInset, rows[0].center
	painted := func(img *image.RGBA, x, y float64) bool {
		return img.RGBAAt(int(math.Floor((ax+x)*scale)), int(math.Floor((cy+y)*scale))) != lightPalette.bg
	}
	// On the left arm of a down arrow, and on the top arm of a right arrow.
	downArm, rightArm := [2]float64{-3, -1.25}, [2]float64{-1.25, -3}
	for _, collapsed := range []bool{false, true} {
		l.Groups[0].Collapsed = collapsed
		img := (&renderer{fonts: fonts, scale: scale, pal: lightPalette}).Render(l, int(widthPt*scale), 200, 0)
		if got := painted(img, downArm[0], downArm[1]); got == collapsed {
			t.Errorf("collapsed %v: down arrow's arm drawn %v", collapsed, got)
		}
		if got := painted(img, rightArm[0], rightArm[1]); got != collapsed {
			t.Errorf("collapsed %v: right arrow's arm drawn %v", collapsed, got)
		}
	}
}

func TestLongTitleLeavesRoomForTheArrow(t *testing.T) {
	fonts, err := loadFonts()
	if err != nil {
		t.Fatal(err)
	}
	const scale, widthPt = 2.0, 300.0
	l, _ := parseMarkdown("# " + strings.Repeat("Wide title ", 40) + "\n- [ ] a\n")
	rows, _ := layout(l)
	img := (&renderer{fonts: fonts, scale: scale, pal: lightPalette}).Render(l, int(widthPt*scale), 200, 0)
	arrowLeft := widthPt - marginRight - arrowInset - arrowArm - 1
	cy := rows[0].center
	for x := (arrowLeft - 4) * scale; x < arrowLeft*scale; x++ {
		for y := (cy - 9) * scale; y < (cy+9)*scale; y++ {
			if c := img.RGBAAt(int(x), int(y)); c != lightPalette.bg {
				t.Fatalf("pixel %v at (%v, %v) in the gap before the arrow: the title runs into it", c, x, y)
			}
		}
	}
}
