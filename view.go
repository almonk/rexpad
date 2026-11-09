package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"maps"
	"math"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type viewOptions struct {
	theme   string  // terminal (follow the terminal's colors), light, or dark
	scale   float64 // device pixels per point; 0 picks from the cell size
	noClick bool    // clicks do not toggle items or collapse groups
}

// clickHit is what a click is on: an item's box, a group's header, or
// neither (both nil).
type clickHit struct {
	item  *Item
	group *Group
}

// clickedAt is what a click at pixel c is on, in the frame drawn at scale
// and scrolled offsetPx down, with the rows where geo puts them. The find
// bar covers the pane from viewPx down.
func clickedAt(l *List, geo []rowGeo, c mouseClick, scale float64, offsetPx, viewPx int) clickHit {
	if c.y >= viewPx {
		return clickHit{}
	}
	x, y := float64(c.x)/scale, float64(c.y+offsetPx)/scale
	if it := hitItem(l, geo, x, y); it != nil {
		return clickHit{item: it}
	}
	return clickHit{group: hitHeader(l, geo, x, y)}
}

// clickTarget names what a click is on, so that a press and its release
// compare equal across reloads of the list: an item by its ID, a header by
// its groupKeys key. The zero value is neither.
type clickTarget struct {
	item  int
	group string
}

func (h clickHit) target(l *List) clickTarget {
	switch {
	case h.item != nil:
		return clickTarget{item: h.item.ID}
	case h.group != nil:
		return clickTarget{group: groupKeys(l)[h.group]}
	}
	return clickTarget{}
}

// clickTracker pairs a press with its release: a click acts only when the
// button goes down and comes up on the same box or header.
type clickTracker struct{ pressed clickTarget }

// add takes a press or release on target on and returns what to toggle,
// or the zero target.
func (t *clickTracker) add(c mouseClick, on clickTarget) clickTarget {
	if !c.release {
		t.pressed = on
		return clickTarget{}
	}
	downOn := t.pressed
	t.pressed = clickTarget{}
	if on != downOn {
		return clickTarget{}
	}
	return on
}

// winSize is the pane's size. guessed says the terminal reported no pixel
// size, so xpix and ypix are made up.
type winSize struct {
	rows, cols, xpix, ypix int
	guessed                bool
}

func getWinSize(fd int) winSize {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws.Row == 0 || ws.Col == 0 {
		return winSize{rows: 24, cols: 80, xpix: 80 * 16, ypix: 24 * 34, guessed: true}
	}
	w := winSize{rows: int(ws.Row), cols: int(ws.Col), xpix: int(ws.Xpixel), ypix: int(ws.Ypixel)}
	if w.xpix == 0 || w.ypix == 0 {
		// No pixel size reported: assume a Retina cell.
		w.xpix, w.ypix, w.guessed = w.cols*16, w.rows*34, true
	}
	return w
}

func pickScale(flag float64, ws winSize) float64 {
	if flag > 0 {
		return flag
	}
	if v, err := strconv.ParseFloat(os.Getenv("REXPAD_SCALE"), 64); err == nil && v > 0 {
		return v
	}
	// Size the list with the terminal's own text: a 13pt terminal font gives
	// cells about 17pt tall, so 34 px cells mean 2x (a Retina Mac) and a
	// phone's 3x cells come out near 2.5 to 3. Round to half steps so a Mac
	// stays at exactly 2x, where the layout matches the design pixel for pixel.
	return max(1, math.Round(float64(ws.ypix)/float64(ws.rows)/17*2)/2)
}

// kittyUpload transmits img as image id without placing it.
func kittyUpload(w *bufio.Writer, img *image.RGBA, id int) error {
	kittyUploadEncoded(w, encodeImage(img), id)
	return nil
}

// kittyPlace shows the w x h pixel window of image id starting at row y,
// at the top-left cell. Placing again with the same placement ID moves the
// window, which is how the viewer scrolls without re-sending pixels.
//
// The placement is pinned to cols x rows cells, so a client whose cells
// differ in pixels from the size the pty reports (a phone viewing the pane)
// scales it to the grid instead of drawing past the edge. C=1 leaves the
// cursor alone, so a full-height placement never scrolls the screen.
func kittyPlace(w *bufio.Writer, id, y, width, height, cols, rows int) {
	kittyPlaceAt(w, 1, id, y, width, height, cols, rows)
}

// kittyPlaceCell places image id's whole width x height at cell (row, col),
// pinned to cols x rows cells, at z-index z (above the list for z > 0). It
// saves and restores the cursor like kittyPlaceAt.
func kittyPlaceCell(w *bufio.Writer, row, col, id, width, height, cols, rows, z int) {
	fmt.Fprintf(w, "\x1b7\x1b[%d;%dH\x1b_Ga=p,i=%d,p=1,x=0,y=0,w=%d,h=%d,c=%d,r=%d,z=%d,C=1,q=2\x1b\\\x1b8",
		row, col, id, width, height, cols, rows, z)
}

// kittyPlaceAt is kittyPlace with the image's top edge on cell row row
// (1-based). It saves and restores the cursor around the move, so a cursor
// sitting in the find field never visibly leaves it (cursor shaders would
// see it jump).
func kittyPlaceAt(w *bufio.Writer, row, id, y, width, height, cols, rows int) {
	cup := "\x1b[H"
	if row > 1 {
		cup = fmt.Sprintf("\x1b[%d;1H", row)
	}
	fmt.Fprintf(w, "\x1b7%s\x1b_Ga=p,i=%d,p=1,x=0,y=%d,w=%d,h=%d,c=%d,r=%d,C=1,q=2\x1b\\\x1b8",
		cup, id, y, width, height, cols, rows)
}

func kittyDelete(w *bufio.Writer, id int) {
	fmt.Fprintf(w, "\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id)
}

// --- terminal input and colors -------------------------------------------

// readTerminal decodes stdin into key presses and terminal replies.
func readTerminal(events chan<- termEvent) {
	var d keyDecoder
	emit := func(ev termEvent) { events <- ev }
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			close(events)
			return
		}
		d.feed(buf[:n], emit)
		d.flush(emit)
	}
}

// termColors are the theme colors the terminal reported.
type termColors struct {
	fg, bg, blue          color.RGBA
	hasFg, hasBg, hasBlue bool
}

// queryColors asks for the foreground, background, and palette blue. Rex
// answers BEL-terminated queries.
func queryColors(w *bufio.Writer) {
	fmt.Fprint(w, "\x1b]10;?\x07\x1b]11;?\x07\x1b]4;4;?\x07")
}

// apply records an OSC color reply and reports whether it was one.
func (tc *termColors) apply(reply string) bool {
	i := strings.Index(reply, ";rgb:")
	if i < 0 {
		return false
	}
	c, ok := parseRGB(reply[i+5:])
	if !ok {
		return false
	}
	switch reply[:i] {
	case "10":
		tc.fg, tc.hasFg = c, true
	case "11":
		tc.bg, tc.hasBg = c, true
	case "4;4":
		tc.blue, tc.hasBlue = c, true
	default:
		return false
	}
	return true
}

// parseRGB reads an X11 color spec like "ffff/8080/0000" (1 to 4 hex digits each).
func parseRGB(spec string) (color.RGBA, bool) {
	parts := strings.Split(spec, "/")
	if len(parts) != 3 {
		return color.RGBA{}, false
	}
	var out [3]uint8
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 16, 32)
		if err != nil || len(p) == 0 || len(p) > 4 {
			return color.RGBA{}, false
		}
		max := float64(uint64(1)<<(4*len(p)) - 1)
		out[i] = uint8(float64(v)/max*255 + 0.5)
	}
	return color.RGBA{out[0], out[1], out[2], 255}, true
}

// palette picks the colors for a theme flag and what the terminal reported.
func (tc termColors) palette(theme string) palette {
	switch theme {
	case "light":
		return lightPalette
	case "dark":
		return darkPalette
	}
	if !tc.hasBg {
		return lightPalette
	}
	base := lightPalette
	if luminance(tc.bg) < 0.18 {
		base = darkPalette
	}
	fg, accent := base.text, base.accent
	if tc.hasFg {
		fg = tc.fg
	}
	if tc.hasBlue {
		accent = tc.blue
	}
	return themedPalette(fg, tc.bg, accent)
}

// Scroll easing: each frame closes the same share of the remaining distance
// (exponential smoothing). Rapid wheel ticks keep moving the target, so the
// view keeps gliding instead of stepping; a single tick settles in ~150 ms.
const (
	colorPoll   = 250 * time.Millisecond // how often to ask for the theme's colors
	colorSettle = 30 * time.Millisecond  // for the rest of a theme's replies to arrive

	animFrame = 8 * time.Millisecond  // ~120 Hz, a crop move costs a few bytes
	foldFrame = 15 * time.Millisecond // ~60 Hz for frames that are new pictures
	easeTau   = 45 * time.Millisecond
)

// easeToward moves shown toward target for a frame of length dt, snapping
// once it's within half a device pixel. Offsets are in points.
func easeToward(shown, target float64, dt time.Duration, scale float64) float64 {
	k := 1 - math.Exp(-float64(dt)/float64(easeTau))
	shown += (target - shown) * k
	if math.Abs(target-shown)*scale < 0.5 {
		return target
	}
	return shown
}

// activeID is the first active item's ID, or 0.
func activeID(l *List) int {
	for _, g := range l.Groups {
		for _, it := range g.Items {
			if it.Status == StatusActive {
				return it.ID
			}
		}
	}
	return 0
}

func runView(name string, opt viewOptions) error {
	fonts, err := loadFonts()
	if err != nil {
		return err
	}
	path := listPath(name)
	in, out := int(os.Stdin.Fd()), os.Stdout

	if term.IsTerminal(in) {
		old, err := term.MakeRaw(in)
		if err == nil {
			defer term.Restore(in, old)
		}
	}
	events := make(chan termEvent, 64)
	go readTerminal(events)

	w := bufio.NewWriterSize(out, 1<<16)
	// Alternate screen, hidden cursor, color-scheme change reports (2031),
	// mouse reports in SGR form (1000, 1006) so the wheel scrolls the list
	// and clicks reach the checkboxes, with positions in pixels (1016) so a
	// click lands on the right row, bracketed paste (2004), and the kitty
	// keyboard protocol's disambiguate flag (> 1 u) so Command and Option
	// keys reach the find field.
	fmt.Fprint(w, "\x1b[?1049h\x1b[?25l\x1b[H\x1b[2J\x1b[?2031h\x1b[?1000h\x1b[?1006h\x1b[?1016h\x1b[?2004h\x1b[>1u")
	w.Flush()
	defer func() {
		fmt.Fprint(w, "\x1b_Ga=d,d=A,q=2\x1b\\\x1b[<u\x1b[?2004l\x1b[?1016l\x1b[?1006l\x1b[?1000l\x1b[?2031l\x1b[?25h\x1b[?1049l")
		w.Flush()
	}()

	// Wait for the theme so the first frame has the right colors. Rex answers
	// from the app, which may not have attached a new pane yet, so keep asking
	// until it does. A terminal that never answers gets the light design.
	var colors termColors
	queryColors(w)
	w.Flush()
	if opt.theme == "terminal" || opt.theme == "auto" {
		deadline := time.After(3 * time.Second)
		retry := time.NewTicker(250 * time.Millisecond)
	wait:
		for !(colors.hasFg && colors.hasBg && colors.hasBlue) {
			select {
			case ev, open := <-events:
				if !open {
					retry.Stop()
					return nil
				}
				colors.apply(ev.osc)
			case <-retry.C:
				queryColors(w)
				w.Flush()
			case <-deadline:
				break wait
			}
		}
		retry.Stop()
	}
	pal := colors.palette(opt.theme)
	// Following the terminal's theme, the list has no background of its own,
	// so the pane's shows through (Rex may draw it differently from the
	// color it reports). The fixed light and dark themes paint theirs.
	clear := opt.theme == "terminal" || opt.theme == "auto"
	// drawPal is the palette images are drawn with: see-through grays when
	// clear. pal itself stays opaque for the find field's terminal colors.
	drawPal := func() palette {
		if clear {
			return pal.seeThrough()
		}
		return pal
	}

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// The viewer renders the whole list as one tall "sheet" image, uploads it
	// once, and scrolls by moving the placement's window over it. A sheet is
	// re-rendered only when the list, size, or theme changes, or when a very
	// long list scrolls past the part of it that was rendered.
	const maxSheetPx = 8192
	type sheet struct {
		id, top, height int // image ID; first content row and height, in pixels
		gen             int // the row geometry it was drawn with (geoGen)
		valid           bool
	}
	var (
		lastStamp string
		lastSize  winSize
		imgID     = 1
		cur       sheet
		list      = &List{}
		scale     = 1.0

		// Scrolling: the view follows the active item until the user scrolls,
		// and goes back to following when a different item becomes active.
		// target is where the view is heading, in points; shown is where it is.
		// shown eases toward target, so wheel ticks glide instead of jumping.
		follow    = true
		manual    float64 // the target while not following
		target    float64
		shown     float64
		maxOffset float64
		viewH     float64
		anchor    int // the active item when the user started scrolling
		started   bool

		anim     *time.Ticker
		animC    <-chan time.Time // nil while idle
		lastStep time.Time

		find     findState
		findFrom float64 // the view's top when the find started, in points
		bar      sheet   // the find bar's image; valid while it's placed
		listRows int     // rows the list's placement covers, under the bar
		listPx   int     // and their height in pixels
		lastFind string  // what the bar last showed, to skip redraws

		// Collapsing: while find is open (and while the list folds back open
		// after it closes) geo says where each row is and how open, and eases
		// toward geoTo. Every frame of that is a fresh image; geoGen counts
		// the changes so the sheet knows when it's stale.
		geo, geoTo []rowGeo
		strips     stripSet // mid-fold frames, sent a band at a time
		// Each group's ring grows in (icons eases toward iconsTo) when
		// progress starts, and the title slides over to make room.
		icons, iconsTo map[string]float64
		// Each active item slides its text over for the working dots
		// (spins eases toward spinsTo). The dots themselves animate as
		// their own small images above the list (dotIDs), redrawn at
		// dotsFPS, so the list doesn't have to be.
		spins, spinsTo map[int]float64
		dotsEpoch      = time.Now()
		dotIDs         []int
		dotsKey        string
		sheetImg       *image.RGBA // cur's pixels, to cut the edge fades from
		bands          [2]int      // the faded top and bottom edge images, 0 for none
		shownKey       string      // what's on screen, to skip placing it again
		lastStrip      time.Time   // when the last one went out
		geoToH         float64
		geoGen         int

		// The find field is terminal text with the real cursor in it.
		field    [3]int     // row, first column, and width in cells last written (1-based)
		cursorAt string     // the cursor's last position sequence, "" while hidden
		park     = "\x1b[H" // where the hidden cursor rests: where it was last seen

		// The current match's outline glides from match to match, and
		// settles in when it appears; ring eases toward ringTo.
		ring, ringTo ringGeo

		// "No matching items" fades in as the last rows fold away.
		emptyA, emptyTo float64
	)
	startAnim := func() {
		if anim == nil {
			anim = time.NewTicker(animFrame)
			animC = anim.C
			lastStep = time.Now().Add(-animFrame) // the first step moves right away
		}
	}
	stopAnim := func() {
		if anim != nil {
			anim.Stop()
			anim, animC = nil, nil
		}
	}

	// draw brings the screen up to date. It re-renders the sheet only when
	// the list, size, or theme changed, or the view left the rendered part;
	// otherwise it only moves the crop window.
	// The cursor is either showing in the find field (cursorAt) or hidden
	// and parked where it was last seen: the top left cell until find is
	// first used, then the field's last caret position. Cursor shaders track
	// even a hidden cursor from where it last showed, so it must never rest
	// anywhere else.
	//
	// guarded runs out, which moves the cursor and puts it back, as one
	// synchronized update (mode 2026), so the terminal never draws the
	// cursor mid-trip. atHome says out only moves it to the top left: a
	// cursor parked there doesn't move at all. The updates are kept to these
	// few bytes: image uploads stream outside them.
	guarded := func(atHome bool, out func()) {
		if cursorAt == "" && atHome && park == "\x1b[H" {
			out()
			return
		}
		fmt.Fprint(w, "\x1b[?2026h")
		out()
		fmt.Fprint(w, "\x1b[?2026l")
	}
	drawFrame := func(force bool) {
		stamp := ""
		if st, err := os.Stat(path); err == nil {
			stamp = fmt.Sprint(st.ModTime().UnixNano(), st.Size())
		}
		ws := getWinSize(int(out.Fd()))
		resized := ws != lastSize
		changed := stamp != lastStamp || resized
		// wrap wraps l's items for the pane's width, which layout needs
		// before anything is positioned.
		wrap := func(l *List) bool {
			sc := pickScale(opt.scale, ws)
			widthPx := ws.xpix / ws.cols * ws.cols
			return (&renderer{fonts: fonts, scale: sc}).wrapList(l, float64(widthPx)/sc-marginRight)
		}
		if wrap(list) && geo != nil {
			// The width changed and rows rewrapped: start the rows from rest.
			geoTo, geoToH = geoFor(list, find.vis)
			geo = append([]rowGeo(nil), geoTo...)
			geoGen++
		}
		if stamp != lastStamp {
			if l, err := loadList(path); err == nil {
				old, oldGeo := list, geo
				wrap(l)
				list = l
				if find.open {
					// Don't pull the view away from what's being found.
					find.refresh(list)
					anchor = activeID(list)
				}
				// New rows unfold into place: each row starts as open as it
				// was (new ones shut) and eases to where it belongs now.
				geoTo, geoToH = geoFor(list, find.vis)
				var from []rowGeo
				if started {
					from = arrivalGeo(old, oldGeo, list)
				}
				if from != nil && !slices.Equal(from, geoTo) {
					geo = from
					startAnim()
				} else if geo != nil {
					geo = append([]rowGeo(nil), geoTo...)
					if !find.open {
						geo = nil
					}
				}
				geoGen++
				if !find.open && !follow && activeID(list) != anchor {
					follow = true
				}
			} // A bad write keeps the last good list on screen.
		}
		lastStamp, lastSize = stamp, ws

		// Whole cells only, so the image is exactly the grid it's pinned to.
		// The find bar takes whole rows at the bottom, and the list's view
		// shrinks above it so nothing hides underneath.
		cellW, cellH := ws.xpix/ws.cols, ws.ypix/ws.rows
		scale = pickScale(opt.scale, ws)
		barRows := 0
		if find.open && ws.rows > 1 {
			barRows = max(1, min(ws.rows-1, int(math.Round(findBarPt*scale/float64(cellH)))))
		}
		rows := ws.rows - barRows
		if rows != listRows {
			changed, listRows = true, rows
		}
		widthPx, viewPx := cellW*ws.cols, cellH*rows
		listPx = viewPx
		viewH = float64(viewPx) / scale
		followTo, maxOff := followOffset(list, viewH)
		maxOffset = maxOff
		contentH := 0.0
		if geo != nil {
			// Room for wherever the rows are and wherever they're going.
			contentH = math.Max(geoHeight(geo), geoToH)
			maxOffset = math.Max(0, contentH-viewH)
		}
		// Point the outline at the current match, where that row is now.
		if m, ok := find.current(); find.open && ok {
			y, ex := 0.0, 0.0
			if rows, _ := layout(list); m.row < len(rows) {
				y, ex = rows[m.row].center, rows[m.row].extra
			}
			if geo != nil {
				y = geo[m.row].center
			}
			if ring.alpha == 0 {
				ring = ringGeo{y: y, grow: ringGrow, extra: ex} // appearing: start out wide and faint
			}
			ringTo = ringGeo{y: y, alpha: 1, extra: ex}
		} else {
			ringTo = ringGeo{y: ring.y, grow: ring.grow}
		}
		if ring != ringTo {
			startAnim()
		}
		// Rings: in once anything in the group is under way. A group seen
		// for the first time starts where it belongs.
		iconsTo = map[string]float64{}
		for g, k := range groupKeys(list) {
			iconsTo[k] = 0
			if g.progress() > 0 {
				iconsTo[k] = 1
			}
		}
		if icons == nil {
			icons = map[string]float64{}
		}
		for k, v := range iconsTo {
			if _, ok := icons[k]; !ok {
				icons[k] = v
			}
		}
		for k := range icons {
			if _, ok := iconsTo[k]; !ok {
				delete(icons, k) // the group is gone
			}
		}
		iconsMoving := !maps.Equal(icons, iconsTo)
		if iconsMoving {
			startAnim()
		}
		spinsTo = map[int]float64{}
		for _, g := range list.Groups {
			for _, it := range g.Items {
				spinsTo[it.ID] = 0
				if it.Status == StatusActive {
					spinsTo[it.ID] = 1
				}
			}
		}
		if spins == nil {
			spins = map[int]float64{}
		}
		for id, v := range spinsTo {
			if _, ok := spins[id]; !ok {
				spins[id] = v // new items start where they belong
			}
		}
		for id := range spins {
			if _, ok := spinsTo[id]; !ok {
				delete(spins, id)
			}
		}
		spinsMoving := !maps.Equal(spins, spinsTo)
		if spinsMoving {
			startAnim()
		}
		emptyTo = 0
		if find.open && strings.TrimSpace(find.query) != "" && len(find.matches) == 0 {
			// Wait until the rows have nearly gone, so it doesn't land on them.
			most := 0.0
			for _, g := range geo {
				most = math.Max(most, g.open)
			}
			if most < 0.25 {
				emptyTo = 1
			}
		}
		if emptyA != emptyTo {
			startAnim()
		}
		moving := geo != nil && !slices.Equal(geo, geoTo) || ring != ringTo || emptyA != emptyTo || iconsMoving || spinsMoving
		if follow {
			target = followTo
		} else {
			manual = math.Max(0, math.Min(manual, maxOffset))
			target = manual
		}
		if !started || resized {
			shown = target // nothing to glide from
			started = true
		}
		shown = math.Max(0, math.Min(shown, maxOffset))
		if shown != target {
			startAnim()
		}
		offsetPx := int(math.Round(shown * scale))

		newRenderer := func() *renderer {
			r := &renderer{fonts: fonts, scale: scale, pal: drawPal(), marks: find.spans(), geo: geo, geoH: contentH, clear: clear, icons: maps.Clone(icons),
				spins: maps.Clone(spins), dots: moving, dotsPhase: time.Since(dotsEpoch).Seconds(), noClick: opt.noClick}
			if ring.alpha > 0 {
				g := ring
				r.ring = &g
			}
			r.empty, r.emptyAlpha = "No matching items", emptyA
			return r
		}
		if geo == nil {
			_, contentH = layout(list)
		}
		if moving && !force && time.Since(lastStrip) < foldFrame {
			// The easing runs every animFrame, but a fresh picture goes out
			// at most every foldFrame: decoding them is the terminal's cost.
		} else if moving {
			// Mid-fold: the view as strips, sending only the ones that changed.
			lastStrip = time.Now()
			img := newRenderer().Render(list, widthPx, viewPx, float64(offsetPx)/scale)
			if fr := fadeRowsFor(scale, cellH, rows); fr > 0 && maxOffset > 0 {
				top, bot := edgeFades(offsetPx, int(math.Round(maxOffset*scale)), fr*cellH)
				fadeRows(img, 0, fr*cellH, true, top, clear, pal.bg)
				fadeRows(img, viewPx-fr*cellH, fr*cellH, false, bot, clear, pal.bg)
			}
			todo := strips.changedStrips(img, cellH, rows, fmt.Sprint(widthPx, cellH, ws.cols, rows))
			ids := make([]int, len(todo))
			for i, st := range todo {
				ids[i] = imgID
				imgID++
				kittyUploadEncoded(w, st.enc, ids[i])
			}
			// Swap them in at once: place the new, drop what they replace.
			fmt.Fprint(w, "\x1b[?2026h")
			for i, st := range todo {
				kittyPlaceAt(w, st.row, ids[i], 0, widthPx, st.h, ws.cols, st.rows)
				if old := strips.ids[st.k]; old != 0 {
					kittyDelete(w, old)
				}
				strips.ids[st.k], strips.pix[st.k] = ids[i], st.px
			}
			for _, id := range append(strips.stale, bands[:]...) {
				if id != 0 {
					kittyDelete(w, id)
				}
			}
			strips.stale, bands, shownKey = nil, [2]int{}, ""
			if cur.valid {
				kittyDelete(w, cur.id)
				cur = sheet{}
			}
			fmt.Fprint(w, "\x1b[?2026l")
		} else {
			replaced := 0 // a sheet this one replaces, to drop once it's placed
			if force || changed || !cur.valid || cur.gen != geoGen || offsetPx < cur.top || offsetPx+viewPx > cur.top+cur.height {
				contentPx := int(math.Ceil(contentH * scale))
				height := max(viewPx, min(contentPx, maxSheetPx))
				top := 0
				if contentPx > height {
					// Center the view in the rendered part of a very long list.
					top = max(0, min(offsetPx-(height-viewPx)/2, contentPx-height))
				}
				img := newRenderer().Render(list, widthPx, height, float64(top)/scale)
				next := sheet{id: imgID, top: top, height: height, gen: geoGen, valid: true}
				imgID++
				if err := kittyUpload(w, img, next.id); err != nil {
					return
				}
				if cur.valid {
					replaced = cur.id
				}
				cur, sheetImg = next, img
			}

			// Put the sheet's crop on screen, with faded edges cut from it
			// where there's more to scroll to. Unless it's what's there.
			sy := offsetPx - cur.top
			fr := 0
			if maxOffset > 0 {
				fr = fadeRowsFor(scale, cellH, rows)
			}
			fadePx := fr * cellH
			top, bot := 0.0, 0.0
			if fr > 0 {
				top, bot = edgeFades(offsetPx, int(math.Round(maxOffset*scale)), fadePx)
			}
			key := fmt.Sprint(cur.id, sy, fr, top, bot, rows, widthPx, ws.cols)
			leftovers := strips.active() || len(strips.stale) > 0
			if replaced != 0 || force || leftovers || key != shownKey {
				shownKey = key
				var next [2]int
				if fr > 0 {
					edges := []*image.RGBA{
						fadeBand(sheetImg, sy, fadePx, true, top, clear, pal.bg),
						fadeBand(sheetImg, sy+viewPx-fadePx, fadePx, false, bot, clear, pal.bg),
					}
					for i, e := range edges {
						next[i] = imgID
						imgID++
						kittyUpload(w, e, next[i])
					}
				}
				old := append([]int{replaced}, bands[:]...)
				old = append(old, strips.ids...)
				old = append(old, strips.stale...)
				place := func() {
					if fr > 0 {
						kittyPlaceAt(w, 1+fr, cur.id, sy+fadePx, widthPx, viewPx-2*fadePx, ws.cols, rows-2*fr)
						kittyPlaceAt(w, 1, next[0], 0, widthPx, fadePx, ws.cols, fr)
						kittyPlaceAt(w, rows-fr+1, next[1], 0, widthPx, fadePx, ws.cols, fr)
					} else {
						kittyPlace(w, cur.id, sy, widthPx, viewPx, ws.cols, rows)
					}
					// Then drop what that replaced, so nothing flashes.
					for _, id := range old {
						if id != 0 {
							kittyDelete(w, id)
						}
					}
				}
				if fr == 0 && replaced == 0 && !leftovers && bands == [2]int{} {
					guarded(true, place) // only the crop moved
				} else {
					fmt.Fprint(w, "\x1b[?2026h") // the parts change together
					place()
					fmt.Fprint(w, "\x1b[?2026l")
				}
				bands, strips = next, stripSet{}
			}
		}

		// The working dots: while frames are new pictures they're in them;
		// otherwise each active item's dots are a small image over the list.
		{
			// Where each active row's dots are: its center line, and just
			// after its text.
			var want []struct{ center, x float64 }
			measure := &renderer{fonts: fonts, scale: scale, pal: pal}
			right := float64(widthPx)/scale - marginRight
			if !moving {
				for _, rw := range drawnRows(list, geo) {
					if rw.kind != rowItem || rw.open < 1 || spins[rw.item.ID] < 1 || rw.item.Status != StatusActive {
						continue
					}
					want = append(want, struct{ center, x float64 }{rw.center + rw.extra, measure.dotsX(rw.item, right, 1)})
				}
			}
			fadePx := 0
			if maxOffset > 0 {
				fadePx = fadeRowsFor(scale, cellH, rows) * cellH
			}
			frame := int(time.Since(dotsEpoch).Seconds() * dotsFPS)
			type spot struct {
				row, col, w, h int
				x, y           float64
			}
			var spots []spot
			for _, g := range want {
				y := g.center*scale - float64(offsetPx) // in the view, in pixels
				if y < float64(fadePx+cellH/2) || y > float64(viewPx-fadePx-cellH/2) || rows < 2 {
					continue // off screen or in a faded edge
				}
				x := g.x * scale
				c0, c1 := int(x)/cellW, min(ws.cols, int(math.Ceil((x+dotsW*scale)/float64(cellW))))
				r0 := max(0, min(rows-2, int(y-dotDiameter*scale)/cellH))
				spots = append(spots, spot{r0 + 1, c0 + 1, c1 - c0, 2, x - float64(c0*cellW), y - float64(r0*cellH)})
			}
			key := fmt.Sprint(frame, spots, pal, scale)
			if len(spots) == 0 {
				key = ""
			}
			if key != dotsKey || force {
				dotsKey = key
				r := &renderer{fonts: fonts, scale: scale, pal: drawPal(), clear: clear}
				var ids []int
				for _, sp := range spots {
					img := image.NewRGBA(image.Rect(0, 0, sp.w*cellW, sp.h*cellH))
					r.workingDots(img, sp.x/scale, sp.y/scale, float64(frame)/dotsFPS, 1)
					ids = append(ids, imgID)
					kittyUpload(w, img, imgID)
					imgID++
				}
				if len(ids) > 0 || len(dotIDs) > 0 {
					fmt.Fprint(w, "\x1b[?2026h")
					for i, sp := range spots {
						kittyPlaceCell(w, sp.row, sp.col, ids[i], sp.w*cellW, sp.h*cellH, sp.w, sp.h, 1)
					}
					for _, id := range dotIDs {
						kittyDelete(w, id)
					}
					fmt.Fprint(w, "\x1b[?2026l")
				}
				dotIDs = ids
			}
		}

		// The find bar is its own small image, redrawn when what it shows changes.
		// It sits on the bar's second-last row, with the drawn magnifier and count
		// centered on that row around it.
		e := &find.edit
		barKey := fmt.Sprint(find.input, e.text, e.cur, e.anchor, find.status(), pal, ws, barRows)
		clearField := func() {
			if field[2] > 0 {
				fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0m%s", field[0], field[1], strings.Repeat(" ", field[2]))
				field = [3]int{}
			}
		}
		switch {
		case barRows > 0 && (force || changed || !bar.valid || barKey != lastFind):
			r := &renderer{fonts: fonts, scale: scale, pal: drawPal(), clear: clear}
			barPx := cellH * barRows
			textRow := rows + max(1, barRows-1)                                   // a row up from the bottom when there's room
			col0 := int(math.Ceil((marginLeft + textX) * scale / float64(cellW))) // 0-based
			cy := (float64((textRow-rows-1)*cellH) + float64(cellH)/2) / scale
			img, end := r.findBar(&find, widthPx, barPx, cy, float64(col0*cellW)/scale)
			next := sheet{id: imgID, height: barPx, valid: true}
			imgID++
			if err := kittyUpload(w, img, next.id); err == nil {
				guarded(false, func() { kittyPlaceAt(w, rows+1, next.id, 0, widthPx, barPx, ws.cols, barRows) })
				if bar.valid {
					kittyDelete(w, bar.id)
				}
				bar = next
			}
			width := max(0, int(math.Floor(end*scale/float64(cellW)))-col0)
			// Rewriting the field walks the cursor along its row: keep that
			// out of sight too.
			fmt.Fprint(w, "\x1b[?2026h")
			clearField()
			runs, caret := e.layout(width, find.input)
			fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0m%s\x1b[%d;%dH", textRow, col0+1, strings.Repeat(" ", width), textRow, col0+1)
			for _, run := range runs {
				switch {
				case run.placeholder:
					c := pal.doneText
					fmt.Fprintf(w, "\x1b[38;2;%d;%d;%dm", c.R, c.G, c.B)
				case run.selected:
					c := mix(pal.accent, pal.bg, 0.6)
					fmt.Fprintf(w, "\x1b[48;2;%d;%d;%dm", c.R, c.G, c.B)
				}
				fmt.Fprint(w, run.text, "\x1b[0m")
			}
			field = [3]int{textRow, col0 + 1, width}
			cursorAt = "" // writing moved it: put it back below
			if find.input {
				cursorAt = fmt.Sprintf("\x1b[%d;%dH", textRow, col0+1+caret)
				park = cursorAt
				fmt.Fprint(w, cursorAt, "\x1b[?25h")
			} else {
				fmt.Fprint(w, "\x1b[?25l", park) // hidden: park it where it was seen
			}
			fmt.Fprint(w, "\x1b[?2026l")
		case barRows == 0 && bar.valid:
			kittyDelete(w, bar.id)
			bar = sheet{}
			fmt.Fprint(w, "\x1b[?2026h\x1b[?25l") // hide it before clearing walks it along the row
			cursorAt = ""
			clearField()
			fmt.Fprint(w, park, "\x1b[?2026l") // and park it where it was seen
		}
		lastFind = barKey
	}
	draw := func(force bool) {
		drawFrame(force)
		w.Flush()
	}

	// step advances the glide and the fold by one frame and redraws.
	step := func(now time.Time) {
		dt := now.Sub(lastStep)
		shown = easeToward(shown, target, dt, scale)
		lastStep = now
		settled := true
		if geo != nil && !slices.Equal(geo, geoTo) {
			settled = easeGeo(list, geo, geoTo, dt)
			geoGen++
		}
		if ring != ringTo {
			ring = easeRing(ring, ringTo, dt, scale)
			geoGen++
			settled = settled && ring == ringTo
		}
		for id, to := range spinsTo {
			if v, ok := spins[id]; ok && v != to {
				spins[id] = easeRingBy(ringGeo{alpha: v}, ringGeo{alpha: to}, dt, scale, collapseTau).alpha
				geoGen++
				settled = settled && spins[id] == to
			}
		}
		for k, to := range iconsTo {
			if v, ok := icons[k]; ok && v != to {
				icons[k] = easeRingBy(ringGeo{alpha: v}, ringGeo{alpha: to}, dt, scale, collapseTau).alpha
				geoGen++
				settled = settled && icons[k] == to
			}
		}
		if emptyA != emptyTo {
			emptyA = easeRingBy(ringGeo{alpha: emptyA}, ringGeo{alpha: emptyTo}, dt, scale, collapseTau).alpha
			geoGen++
			settled = settled && emptyA == emptyTo
		}
		if settled && geo != nil && !find.open {
			geo = nil // fully open again: back to the plain layout
			geoGen++
		}
		if shown == target && settled {
			stopAnim()
		}
		draw(false)
	}
	// refold points the rows at find's current filter and starts them moving.
	refold := func() {
		geoTo, geoToH = geoFor(list, find.vis)
		if len(geo) != len(geoTo) {
			geo = append([]rowGeo(nil), geoTo...)
			geoGen++
		}
		if !slices.Equal(geo, geoTo) {
			startAnim()
		}
	}
	// scrollTo glides the view to offset, as if the user had scrolled there.
	scrollTo := func(offset float64) {
		if follow {
			follow, anchor = false, activeID(list)
		}
		manual = math.Max(0, offset)
		target = manual
		if shown != target {
			startAnim()
		}
	}

	// scrollBy moves the target and stops following the active item. The
	// first frame of the glide is drawn right away, so input never waits.
	scrollBy := func(k scrollKind) {
		if follow {
			follow, manual, anchor = false, target, activeID(list)
		}
		page := math.Max(itemPitch, viewH-2*itemPitch)
		switch k {
		case wheelUp:
			manual -= itemPitch / 2
		case wheelDown:
			manual += itemPitch / 2
		case lineUp:
			manual -= itemPitch
		case lineDown:
			manual += itemPitch
		case pageUp:
			manual -= page
		case pageDown:
			manual += page
		case scrollTop:
			manual = 0
		case scrollBottom:
			manual = maxOffset
		}
		manual = math.Max(0, math.Min(manual, maxOffset))
		target = manual
		if shown != target {
			idle := anim == nil
			startAnim()
			if idle {
				step(time.Now())
			}
		}
	}

	// reveal scrolls the current match into view if it isn't already.
	reveal := func() {
		if len(find.matches) == 0 {
			return
		}
		c := geoTo[find.matches[find.cur].row].center // where it's going to be
		if c >= target+itemPitch && c <= target+viewH-itemPitch {
			return
		}
		scrollTo(math.Min(c-viewH/3, math.Max(0, geoToH-viewH)))
	}
	// closeFind folds the whole list back open and keeps the match that was
	// current in view, so Esc lands you on what you found, in context.
	closeFind := func() {
		m, ok := find.current()
		find = findState{}
		refold()
		draw(true) // the bar's rows go back to the list first
		if ok {
			scrollTo(math.Max(0, math.Min(geoTo[m.row].center-viewH/3, geoToH-viewH)))
		} else {
			scrollTo(findFrom)
		}
		draw(false)
	}
	// queried re-folds after the query changed and heads for the top.
	queried := func() {
		refold()
		if strings.TrimSpace(find.query) == "" {
			scrollTo(findFrom)
		} else {
			scrollTo(0)
		}
		draw(true)
		reveal()
	}
	findStep := func(d int) {
		find.step(d)
		draw(true)
		reveal()
	}
	// edited re-runs the find after the field's text changed.
	edited := func() {
		find.setQuery(list, find.edit.text)
		queried()
	}
	// findKey handles a key while typing into the find bar. The field takes
	// what it can; the rest steer the find.
	findKey := func(k keyPress) {
		switch {
		case k.is(keyEscape, 0) || k.is('c', modCtrl) || k.is('g', modCtrl):
			closeFind()
			return
		case k.is(keyEnter, 0):
			if strings.TrimSpace(find.edit.text) == "" {
				closeFind()
				return
			}
			find.input = false
			draw(true)
			return
		case k.is(keyUp, 0) || k.is('p', modCtrl):
			findStep(-1)
			return
		case k.is(keyDown, 0) || k.is('n', modCtrl):
			findStep(1)
			return
		}
		res := find.edit.handle(k)
		if res.copy != "" {
			// OSC 52 puts it on the clipboard of the machine you're looking from.
			fmt.Fprintf(w, "\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(res.copy)))
		}
		switch {
		case res.edited:
			edited()
		case res.handled:
			draw(false) // the caret or selection moved
		}
	}
	startFind := func() {
		if find.open {
			// Back into the field with the query selected, like Command-F
			// in a Mac app: type to replace it, or arrow to edit it.
			find.input = true
			find.edit.anchor, find.edit.cur = 0, len(find.edit.text)
			draw(true)
			return
		}
		findFrom = shown
		find = findState{open: true, input: true}
		refold()
		draw(true)
	}

	// handleKey acts on a key press and reports whether it quits.
	handleKey := func(k keyPress) bool {
		if find.input {
			findKey(k)
			return false
		}
		switch {
		case k.is('/', 0) || k.is('f', modCtrl) || k.is('f', modSuper):
			startFind()
		case find.open && k.is('n', 0):
			findStep(1)
		case find.open && (k.is('N', 0) || k.is('n', modShift)):
			findStep(-1)
		case find.open && (k.is(keyEscape, 0) || k.is('c', modCtrl)):
			closeFind()
		case k.is('q', 0) || k.is('c', modCtrl) || k.is('d', modCtrl):
			return true
		case k.is('k', 0) || k.is(keyUp, 0):
			scrollBy(lineUp)
		case k.is('j', 0) || k.is(keyDown, 0):
			scrollBy(lineDown)
		case k.is('b', 0) || k.is('u', 0) || k.is(keyPageUp, 0):
			scrollBy(pageUp)
		case k.is(' ', 0) || k.is('d', 0) || k.is(keyPageDown, 0):
			scrollBy(pageDown)
		case k.is('g', 0) || k.is(keyHome, 0):
			scrollBy(scrollTop)
		case k.is('G', 0) || k.is('g', modShift) || k.is(keyEnd, 0):
			scrollBy(scrollBottom)
		case k.is('f', 0): // follow the active item again
			follow = true
			draw(false)
		case k.is('r', 0) || k.is('l', modCtrl):
			draw(true)
		}
		return false
	}

	// handleClick toggles an item, or collapses or expands a group, when
	// the button goes down and comes up on the item's box or the group's
	// header. Focus stays in this pane.
	//
	// Clicks work only once the terminal confirms that it reports pixels
	// (mode 1016): a terminal that reports cells instead would put every
	// click on the first rows. A made-up pixel size cannot be mapped either.
	var clicks clickTracker
	pixelReports := false
	handleClick := func(c mouseClick) {
		if opt.noClick || !pixelReports || lastSize.guessed {
			return
		}
		hit := clickedAt(list, geo, c, scale, int(math.Round(shown*scale)), listPx)
		on := hit.target(list)
		if clicks.add(c, on) == (clickTarget{}) {
			return
		}
		var err error
		if hit.item != nil {
			err = toggleByUser(path, hit.item.ID, hit.item.Status)
		} else {
			err = toggleCollapsedByUser(path, on.group, hit.group.Collapsed)
		}
		if err != nil {
			// The pane has no room for a message: the bell says the
			// click did not take, and the box or arrow shows it unchanged.
			fmt.Fprint(w, "\a")
		}
		draw(false)
	}

	draw(true)
	// Asked here, not with the modes above: the wait for the theme's colors
	// reads and drops every other reply.
	fmt.Fprint(w, "\x1b[?1016$p")
	w.Flush()
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	defer stopAnim()
	// Re-ask for colors often: switching Rex's theme sends no report (2031
	// is for the system's light and dark), so asking is how a new theme
	// shows up. It's three short queries.
	recolor := time.NewTicker(colorPoll)
	defer recolor.Stop()
	// The working dots' frames; a draw with nothing else to do is cheap.
	dotsTick := time.NewTicker(time.Second / dotsFPS)
	defer dotsTick.Stop()
	// A theme change answers in three replies (foreground, background,
	// blue); wait a moment after the first so they redraw once, together.
	var repaint <-chan time.Time
	for {
		select {
		case <-tick.C:
			draw(false)
		case now := <-animC:
			step(now)
		case <-recolor.C:
			queryColors(w)
			w.Flush()
		case <-dotsTick.C:
			if dotsKey != "" {
				draw(false)
			}
		case <-repaint:
			repaint = nil
			if p := colors.palette(opt.theme); p != pal {
				pal = p
				draw(true)
			}
		case <-winch:
			draw(true)
		case ev, open := <-events:
			if !open {
				return nil
			}
			switch {
			case ev.scheme:
				queryColors(w)
				w.Flush()
			case ev.osc != "":
				if colors.apply(ev.osc) && repaint == nil && colors.palette(opt.theme) != pal {
					repaint = time.After(colorSettle)
				}
			case ev.scroll != scrollNone:
				scrollBy(ev.scroll)
			case ev.mode != nil:
				if ev.mode.mode == 1016 {
					pixelReports = ev.mode.on()
				}
			case ev.click != nil:
				handleClick(*ev.click)
			case ev.paste != "":
				if find.input {
					find.edit.paste(ev.paste)
					edited()
				}
			case ev.key.code != 0:
				if handleKey(ev.key) {
					return nil
				}
			}
		case <-quit:
			return nil
		}
	}
}
