package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
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
	fonts, _ := loadFonts()
	r := &renderer{fonts: fonts, scale: 2, pal: lightPalette}
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
	got := viewerCommand("/bin/rexpad", "l", "dark")
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
	got := viewerCommand("/bin/rexpad", "l", "terminal")
	want := []string{"/bin/rexpad", "view", "--list", "l", "--theme", "terminal"}
	if !slices.Equal(got, want) {
		t.Errorf("viewerCommand = %q\nwant %q", got, want)
	}
}
