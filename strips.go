package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image"
	"sync"
)

// While rows fold, every frame is a new picture, and encoding a whole
// pane-sized PNG is most of a frame's cost. So moving frames go out as
// strips: bands a few cells tall, each its own image pinned to its own
// cells. They encode in parallel, and a band whose pixels didn't change
// since the last frame (above the first moving row, or empty space below the
// list) isn't sent again. Once things settle, the viewer goes back to one
// tall sheet that scrolls by moving its crop.

// stripRows is how many cell rows a strip spans.
const stripRows = 3

type stripSet struct {
	key   string   // the geometry they were cut for
	ids   []int    // the image showing each strip, 0 for none
	pix   [][]byte // what each strip last showed
	stale []int    // images from an old grid, to delete at the next swap
}

func (s *stripSet) active() bool {
	for _, id := range s.ids {
		if id != 0 {
			return true
		}
	}
	return false
}

// strip is one band of a frame that needs sending.
type strip struct {
	k, row, rows int     // index, first cell row (1-based), rows covered
	px           []byte  // its pixels, to compare with the next frame
	enc          encoded // what to send
	h            int     // height in pixels
}

// changedStrips cuts img (the view, cellH-pixel rows, rows of them) into
// strips and encodes, in parallel, the ones that differ from what s shows.
func (s *stripSet) changedStrips(img *image.RGBA, cellH, rows int, key string) []strip {
	n := (rows + stripRows - 1) / stripRows
	if s.key != key || len(s.ids) != n {
		// A different grid: every strip is new (the old ones go in the swap).
		for _, id := range s.ids {
			if id != 0 {
				s.stale = append(s.stale, id)
			}
		}
		s.key = key
		s.pix = make([][]byte, n)
		s.ids = append(s.ids[:0:0], make([]int, n)...)
	}
	var out []strip
	for k := 0; k < n; k++ {
		r0 := k * stripRows
		rr := min(stripRows, rows-r0)
		y0, y1 := r0*cellH, (r0+rr)*cellH
		px := img.Pix[y0*img.Stride : y1*img.Stride]
		if s.ids[k] != 0 && bytes.Equal(px, s.pix[k]) {
			continue
		}
		out = append(out, strip{k: k, row: r0 + 1, rows: rr, px: px, h: y1 - y0})
	}
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(st *strip) {
			defer wg.Done()
			sub := img.SubImage(image.Rect(0, (st.row-1)*cellH, img.Bounds().Dx(), (st.row-1)*cellH+st.h))
			st.enc = encodeImage(sub.(*image.RGBA))
		}(&out[i])
	}
	wg.Wait()
	return out
}

// encoded is an image as the kitty protocol's raw format takes it: 32-bit
// RGBA with straight alpha, zlib-compressed (f=32, o=z). It's quicker to
// make than a PNG, which tries five filters on every row, and comes out
// smaller for these images, which are mostly transparent.
type encoded struct {
	data []byte
	w, h int
}

func encodeImage(img *image.RGBA) encoded {
	b := img.Bounds()
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	row := make([]byte, b.Dx()*4)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		src := img.Pix[img.PixOffset(b.Min.X, y):][:len(row)]
		for i := 0; i < len(row); i += 4 {
			switch a := src[i+3]; a {
			case 0, 255:
				copy(row[i:i+4], src[i:i+4])
			default: // image.RGBA is premultiplied; the protocol wants straight
				for c := 0; c < 3; c++ {
					row[i+c] = uint8(min(255, (int(src[i+c])*255+int(a)/2)/int(a)))
				}
				row[i+3] = a
			}
		}
		zw.Write(row)
	}
	zw.Close()
	return encoded{buf.Bytes(), b.Dx(), b.Dy()}
}

// kittyUploadEncoded transmits e as image id without placing it.
func kittyUploadEncoded(w *bufio.Writer, e encoded, id int) {
	data := base64.StdEncoding.EncodeToString(e.data)
	const chunk = 4096
	for i := 0; i < len(data); i += chunk {
		end := min(i+chunk, len(data))
		more := 0
		if end < len(data) {
			more = 1
		}
		if i == 0 {
			fmt.Fprintf(w, "\x1b_Ga=t,f=32,o=z,s=%d,v=%d,i=%d,q=2,m=%d;%s\x1b\\", e.w, e.h, id, more, data[i:end])
		} else {
			fmt.Fprintf(w, "\x1b_Gm=%d;%s\x1b\\", more, data[i:end])
		}
	}
}
