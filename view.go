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

// kittyImage writes img at the cursor with the kitty graphics protocol and
// leaves the cursor where it was (C=1), so a full-height image never scrolls.
// It pins the placement to cols x rows cells: a client whose cells differ in
// pixels from the size the pty reports (a phone viewing the pane) scales the
// image to the grid instead of drawing it past the edge.
func kittyImage(w *bufio.Writer, img image.Image, id, cols, rows int) error {
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
			fmt.Fprintf(w, "\x1b_Ga=T,f=100,i=%d,p=1,q=2,C=1,c=%d,r=%d,m=%d;%s\x1b\\", id, cols, rows, more, data[i:end])
		} else {
			fmt.Fprintf(w, "\x1b_Gm=%d;%s\x1b\\", more, data[i:end])
		}
	}
	return nil
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
	// Alternate screen, hidden cursor, and color-scheme change reports (2031).
	fmt.Fprint(w, "\x1b[?1049h\x1b[?25l\x1b[2J\x1b[?2031h")
	w.Flush()
	defer func() {
		fmt.Fprint(w, "\x1b_Ga=d,d=A,q=2\x1b\\\x1b[?2031l\x1b[?25h\x1b[?1049l")
		w.Flush()
	}()

	// Wait briefly for the theme so the first frame has the right colors.
	var colors termColors
	queryColors(w)
	w.Flush()
	if opt.theme == "terminal" || opt.theme == "auto" {
		deadline := time.After(time.Second)
	wait:
		for !(colors.hasFg && colors.hasBg && colors.hasBlue) {
			select {
			case ev, open := <-events:
				if !open {
					return nil
				}
				colors.apply(ev.osc)
			case <-deadline:
				break wait
			}
		}
	}
	pal := colors.palette(opt.theme)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	var (
		lastStamp string
		lastSize  winSize
		imgID     = 1
		curID     = 0
		list      = &List{}
	)
	draw := func(force bool) {
		stamp := ""
		if st, err := os.Stat(path); err == nil {
			stamp = fmt.Sprint(st.ModTime().UnixNano(), st.Size())
		}
		ws := getWinSize(int(out.Fd()))
		if !force && stamp == lastStamp && ws == lastSize {
			return
		}
		if stamp != lastStamp {
			if l, err := loadList(path); err == nil {
				list = l
			} // A bad write keeps the last good list on screen.
		}
		lastStamp, lastSize = stamp, ws

		// Whole cells only, so the image is exactly the grid it's pinned to.
		cellW, cellH := ws.xpix/ws.cols, ws.ypix/ws.rows
		r := &renderer{fonts: fonts, scale: pickScale(opt.scale, ws), pal: pal}
		img := r.Render(list, cellW*ws.cols, cellH*ws.rows)

		// Place the new image, then drop the old one, so updates don't flash.
		next := imgID
		imgID++
		fmt.Fprint(w, "\x1b[H")
		if err := kittyImage(w, img, next, ws.cols, ws.rows); err != nil {
			return
		}
		if curID != 0 {
			kittyDelete(w, curID)
		}
		curID = next
		w.Flush()
	}

	draw(true)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	// Re-ask for colors now and then too: not every theme change sends a 2031 report.
	recolor := time.NewTicker(2 * time.Second)
	defer recolor.Stop()
	for {
		select {
		case <-tick.C:
			draw(false)
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
			case ev.key == 'q' || ev.key == 3 || ev.key == 4: // q, Ctrl-C, Ctrl-D
				return nil
			case ev.key == 'r' || ev.key == 12: // r, Ctrl-L
				draw(true)
			}
		case <-quit:
			return nil
		}
	}
}
