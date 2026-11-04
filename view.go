package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type viewOptions struct {
	theme string  // terminal (follow the terminal's colors), light, or dark
	scale float64 // device pixels per point; 0 picks from the cell size
}

type winSize struct{ rows, cols, xpix, ypix int }

func getWinSize(fd int) winSize {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws.Row == 0 || ws.Col == 0 {
		return winSize{rows: 24, cols: 80, xpix: 80 * 16, ypix: 24 * 34}
	}
	w := winSize{rows: int(ws.Row), cols: int(ws.Col), xpix: int(ws.Xpixel), ypix: int(ws.Ypixel)}
	if w.xpix == 0 || w.ypix == 0 {
		// No pixel size reported: assume a Retina cell.
		w.xpix, w.ypix = w.cols*16, w.rows*34
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
func kittyUpload(w *bufio.Writer, img image.Image, id int) error {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return err
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())
	const chunk = 4096
	for i := 0; i < len(data); i += chunk {
		end := min(i+chunk, len(data))
		more := 0
		if end < len(data) {
			more = 1
		}
		if i == 0 {
			fmt.Fprintf(w, "\x1b_Ga=t,f=100,i=%d,q=2,m=%d;%s\x1b\\", id, more, data[i:end])
		} else {
			fmt.Fprintf(w, "\x1b_Gm=%d;%s\x1b\\", more, data[i:end])
		}
	}
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
	fmt.Fprintf(w, "\x1b[H\x1b_Ga=p,i=%d,p=1,x=0,y=%d,w=%d,h=%d,c=%d,r=%d,C=1,q=2\x1b\\",
		id, y, width, height, cols, rows)
}

func kittyDelete(w *bufio.Writer, id int) {
	fmt.Fprintf(w, "\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id)
}

// --- terminal input and colors -------------------------------------------

// termEvent is one thing read from the terminal: a key press, an OSC reply,
// or a color-scheme change report (CSI ? 997 ; n n, mode 2031).
type termEvent struct {
	key    byte
	osc    string
	scheme bool
	scroll scrollKind // a wheel tick or a navigation key sent as an escape sequence
}

type scrollKind int

const (
	scrollNone scrollKind = iota
	wheelUp
	wheelDown
	lineUp
	lineDown
	pageUp
	pageDown
	scrollTop
	scrollBottom
)

// csiScroll maps a finished CSI sequence (without the ESC [) to a scroll:
// SGR mouse wheel reports (mode 1006), arrows, Page Up/Down, Home/End.
func csiScroll(seq string) scrollKind {
	if strings.HasPrefix(seq, "<") && strings.HasSuffix(seq, "M") {
		btn, err := strconv.Atoi(strings.SplitN(seq[1:], ";", 2)[0])
		if err != nil {
			return scrollNone
		}
		switch btn &^ (4 | 8 | 16) { // ignore Shift, Alt, Ctrl
		case 64:
			return wheelUp
		case 65:
			return wheelDown
		}
		return scrollNone
	}
	switch seq {
	case "A":
		return lineUp
	case "B":
		return lineDown
	case "5~":
		return pageUp
	case "6~":
		return pageDown
	case "H", "1~", "7~":
		return scrollTop
	case "F", "4~", "8~":
		return scrollBottom
	}
	return scrollNone
}

// readTerminal splits stdin into keys and terminal replies.
func readTerminal(events chan<- termEvent) {
	const (
		normal = iota
		esc
		osc
		oscEsc
		csi
	)
	state := normal
	var seq []byte
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			close(events)
			return
		}
		for _, c := range buf[:n] {
			switch state {
			case normal:
				if c == 0x1b {
					state = esc
				} else {
					events <- termEvent{key: c}
				}
			case esc:
				seq = seq[:0]
				switch c {
				case ']':
					state = osc
				case '[':
					state = csi
				default:
					state = normal
				}
			case osc:
				switch c {
				case 0x07:
					events <- termEvent{osc: string(seq)}
					state = normal
				case 0x1b:
					state = oscEsc
				default:
					seq = append(seq, c)
				}
			case oscEsc:
				if c == '\\' {
					events <- termEvent{osc: string(seq)}
					state = normal
				} else {
					seq = append(seq, 0x1b, c)
					state = osc
				}
			case csi:
				seq = append(seq, c)
				if c >= 0x40 && c <= 0x7e {
					if strings.HasPrefix(string(seq), "?997;") {
						events <- termEvent{scheme: true}
					} else if k := csiScroll(string(seq)); k != scrollNone {
						events <- termEvent{scroll: k}
					}
					state = normal
				}
			}
		}
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
	// Alternate screen, hidden cursor, color-scheme change reports (2031), and
	// mouse reports in SGR form (1000, 1006) so the wheel scrolls the list.
	fmt.Fprint(w, "\x1b[?1049h\x1b[?25l\x1b[2J\x1b[?2031h\x1b[?1000h\x1b[?1006h")
	w.Flush()
	defer func() {
		fmt.Fprint(w, "\x1b_Ga=d,d=A,q=2\x1b\\\x1b[?1006l\x1b[?1000l\x1b[?2031l\x1b[?25h\x1b[?1049l")
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
		valid           bool
	}
	var (
		lastStamp string
		lastSize  winSize
		imgID     = 1
		cur       sheet
		list      = &List{}

		// Scrolling: the view follows the active item until the user scrolls,
		// and goes back to following when a different item becomes active.
		follow     = true
		manual     float64 // offset in points while not following
		lastOffset float64
		maxOffset  float64
		viewH      float64
		anchor     int // the active item when the user started scrolling
		scrolled   bool
	)
	draw := func(force bool) {
		stamp := ""
		if st, err := os.Stat(path); err == nil {
			stamp = fmt.Sprint(st.ModTime().UnixNano(), st.Size())
		}
		ws := getWinSize(int(out.Fd()))
		changed := stamp != lastStamp || ws != lastSize
		if !force && !scrolled && !changed {
			return
		}
		scrolled = false
		if stamp != lastStamp {
			if l, err := loadList(path); err == nil {
				list = l
				if !follow && activeID(list) != anchor {
					follow = true
				}
			} // A bad write keeps the last good list on screen.
		}
		lastStamp, lastSize = stamp, ws

		// Whole cells only, so the image is exactly the grid it's pinned to.
		cellW, cellH := ws.xpix/ws.cols, ws.ypix/ws.rows
		widthPx, viewPx := cellW*ws.cols, cellH*ws.rows
		scale := pickScale(opt.scale, ws)
		viewH = float64(viewPx) / scale
		offset, maxOff := followOffset(list, viewH)
		maxOffset = maxOff
		if !follow {
			manual = math.Max(0, math.Min(manual, maxOffset))
			offset = manual
		}
		lastOffset = offset
		offsetPx := int(math.Round(offset * scale))

		if force || changed || !cur.valid || offsetPx < cur.top || offsetPx+viewPx > cur.top+cur.height {
			_, contentH := layout(list)
			contentPx := int(math.Ceil(contentH * scale))
			height := max(viewPx, min(contentPx, maxSheetPx))
			top := 0
			if contentPx > height {
				// Center the view in the rendered part of a very long list.
				top = max(0, min(offsetPx-(height-viewPx)/2, contentPx-height))
			}
			r := &renderer{fonts: fonts, scale: scale, pal: pal}
			img := r.Render(list, widthPx, height, float64(top)/scale)
			next := sheet{id: imgID, top: top, height: height, valid: true}
			imgID++
			if err := kittyUpload(w, img, next.id); err != nil {
				return
			}
			// Place the new sheet, then drop the old one, so updates don't flash.
			kittyPlace(w, next.id, offsetPx-next.top, widthPx, viewPx, ws.cols, ws.rows)
			if cur.valid {
				kittyDelete(w, cur.id)
			}
			cur = next
		} else {
			kittyPlace(w, cur.id, offsetPx-cur.top, widthPx, viewPx, ws.cols, ws.rows)
		}
		w.Flush()
	}

	// scrollBy moves the view and stops following the active item. Redraws
	// wait for the next frame so a burst of trackpad ticks costs one render.
	scrollBy := func(k scrollKind) {
		if follow {
			follow, manual, anchor = false, lastOffset, activeID(list)
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
		scrolled = manual != lastOffset
	}

	draw(true)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	frame := time.NewTicker(16 * time.Millisecond)
	defer frame.Stop()
	// Re-ask for colors now and then too: not every theme change sends a 2031 report.
	recolor := time.NewTicker(2 * time.Second)
	defer recolor.Stop()
	for {
		select {
		case <-tick.C:
			draw(false)
		case <-frame.C:
			if scrolled {
				draw(false)
			}
		case <-recolor.C:
			queryColors(w)
			w.Flush()
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
				if colors.apply(ev.osc) {
					if p := colors.palette(opt.theme); p != pal {
						pal = p
						draw(true)
					}
				}
			case ev.scroll != scrollNone:
				scrollBy(ev.scroll)
			case ev.key == 'q' || ev.key == 3 || ev.key == 4: // q, Ctrl-C, Ctrl-D
				return nil
			case ev.key == 'k':
				scrollBy(lineUp)
			case ev.key == 'j':
				scrollBy(lineDown)
			case ev.key == 'b' || ev.key == 'u':
				scrollBy(pageUp)
			case ev.key == ' ' || ev.key == 'd':
				scrollBy(pageDown)
			case ev.key == 'g':
				scrollBy(scrollTop)
			case ev.key == 'G':
				scrollBy(scrollBottom)
			case ev.key == 'f': // follow the active item again
				follow = true
				draw(true)
			case ev.key == 'r' || ev.key == 12: // r, Ctrl-L
				draw(true)
			}
		case <-quit:
			return nil
		}
	}
}
