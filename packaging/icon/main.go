// Command icon draws Whydah's icon, an anchor whose shank carries a download
// arrow, brass on navy, and writes it in every format the app ships:
//
//	packaging/macos/Whydah.icns     Whydah.app (with macOS's standard margin)
//	packaging/windows/whydah.ico    the Windows .exe (via go-winres; see the Makefile)
//	packaging/linux/whydah.png      512 px, for a .desktop launcher
//	internal/web/icon.png           favicon and the UI's header
//
// Run it from the repository root after changing the drawing: go run ./packaging/icon
//
// The drawing is in a 1024×1024 space: the rounded square is 824 px with its
// corner at (100, 100), as Apple's icon grid has it.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/image/vector"
)

type pt struct{ x, y float64 }

// poly is a closed outline. Shapes are filled as outlines so that strokes,
// caps and joins come out exactly, and so overlapping parts of one colour
// merge (the rasterizer clamps coverage) instead of doubling up at the edges.
type poly []pt

func (p poly) area() float64 {
	a := 0.0
	for i := range p {
		q := p[(i+1)%len(p)]
		a += p[i].x*q.y - q.x*p[i].y
	}
	return a / 2
}

// wind returns p turning one way (filled) or, with hole, the other (cut out).
func (p poly) wind(hole bool) poly {
	if (p.area() < 0) != hole {
		out := make(poly, len(p))
		for i := range p {
			out[i] = p[len(p)-1-i]
		}
		return out
	}
	return p
}

func arc(c pt, r, a0, a1 float64, n int) poly {
	var out poly
	for i := 0; i <= n; i++ {
		a := a0 + (a1-a0)*float64(i)/float64(n)
		out = append(out, pt{c.x + r*math.Cos(a), c.y + r*math.Sin(a)})
	}
	return out
}

func circle(c pt, r float64) poly { return arc(c, r, 0, 2*math.Pi, 128)[:128] }

func roundRect(x, y, w, h, r float64) poly {
	var out poly
	out = append(out, arc(pt{x + w - r, y + r}, r, -math.Pi/2, 0, 32)...)
	out = append(out, arc(pt{x + w - r, y + h - r}, r, 0, math.Pi/2, 32)...)
	out = append(out, arc(pt{x + r, y + h - r}, r, math.Pi/2, math.Pi, 32)...)
	out = append(out, arc(pt{x + r, y + r}, r, math.Pi, 3*math.Pi/2, 32)...)
	return out
}

// segment is a line of width w with round ends; a stroked path is a chain of them.
func segment(a, b pt, w float64) poly {
	ang := math.Atan2(b.y-a.y, b.x-a.x)
	out := arc(b, w/2, ang-math.Pi/2, ang+math.Pi/2, 24)
	return append(out, arc(a, w/2, ang+math.Pi/2, ang+3*math.Pi/2, 24)...)
}

func stroke(path []pt, w float64) []poly {
	var out []poly
	for i := 1; i < len(path); i++ {
		out = append(out, segment(path[i-1], path[i], w))
	}
	return out
}

func cubic(p0, p1, p2, p3 pt, n int) []pt {
	var out []pt
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(n)
		u := 1 - t
		out = append(out, pt{
			u*u*u*p0.x + 3*u*u*t*p1.x + 3*u*t*t*p2.x + t*t*t*p3.x,
			u*u*u*p0.y + 3*u*u*t*p1.y + 3*u*t*t*p2.y + t*t*t*p3.y,
		})
	}
	return out
}

// gradient is a vertical gradient between two colours, in drawing coordinates.
type gradient struct {
	y0, y1   float64
	top, bot color.NRGBA
	scale    float64 // pixels per drawing unit
	offset   float64 // drawing y at pixel row 0
}

func (g gradient) ColorModel() color.Model { return color.NRGBAModel }
func (g gradient) Bounds() image.Rectangle {
	return image.Rect(-1e6, -1e6, 1e6, 1e6)
}
func (g gradient) At(_, y int) color.Color {
	t := ((float64(y)+0.5)/g.scale + g.offset - g.y0) / (g.y1 - g.y0)
	t = math.Max(0, math.Min(1, t))
	mix := func(a, b uint8) uint8 { return uint8(math.Round(float64(a) + (float64(b)-float64(a))*t)) }
	return color.NRGBA{mix(g.top.R, g.bot.R), mix(g.top.G, g.bot.G), mix(g.top.B, g.bot.B), mix(g.top.A, g.bot.A)}
}

var (
	navy  = gradient{y0: 100, y1: 924, top: color.NRGBA{0x1f, 0x3a, 0x5f, 0xff}, bot: color.NRGBA{0x0d, 0x1a, 0x2e, 0xff}}
	brass = gradient{y0: 190, y1: 830, top: color.NRGBA{0xf3, 0xcb, 0x73, 0xff}, bot: color.NRGBA{0xc8, 0x8d, 0x33, 0xff}}
	edge  = color.NRGBA{0xff, 0xff, 0xff, 0x12} // a faint rim, so the tile holds its shape on dark backgrounds
)

// anchor is the brass mark: ring, crossbar, shank, the arrow, the arms and flukes.
func anchor() []poly {
	out := []poly{
		circle(pt{512, 240}, 56+23).wind(false),
		circle(pt{512, 240}, 56-23).wind(true),
	}
	out = append(out, stroke([]pt{{396, 368}, {628, 368}}, 52)...)
	out = append(out, stroke([]pt{{512, 312}, {512, 786}}, 58)...)
	out = append(out, stroke([]pt{{426, 498}, {512, 584}, {598, 498}}, 58)...)
	arms := append(cubic(pt{296, 600}, pt{304, 728}, pt{404, 792}, pt{512, 792}, 48),
		cubic(pt{512, 792}, pt{620, 792}, pt{720, 728}, pt{728, 600}, 48)[1:]...)
	out = append(out, stroke(arms, 56)...)
	out = append(out, poly{{290, 504}, {224, 630}, {356, 616}}, poly{{734, 504}, {800, 630}, {668, 616}})
	for i, p := range out {
		if i > 1 { // the ring's two circles are already wound
			out[i] = p.wind(false)
		}
	}
	return out
}

// render draws the icon at size px. With margin, the tile sits inside the
// full 1024 grid (macOS, Linux); without, it fills the image (favicon, Windows).
func render(size int, margin bool) *image.NRGBA {
	view, origin := 1024.0, 0.0
	if !margin {
		view, origin = 824, 100
	}
	s := float64(size) / view
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	fill := func(src image.Image, shapes ...poly) {
		z := vector.NewRasterizer(size, size)
		for _, p := range shapes {
			for i, q := range p {
				x, y := float32((q.x-origin)*s), float32((q.y-origin)*s)
				if i == 0 {
					z.MoveTo(x, y)
				} else {
					z.LineTo(x, y)
				}
			}
			z.ClosePath()
		}
		z.Draw(img, img.Bounds(), src, image.Point{})
	}
	place := func(g gradient) gradient { g.scale, g.offset = s, origin; return g }
	fill(place(navy), roundRect(100, 100, 824, 824, 185).wind(false))
	fill(image.NewUniform(edge), roundRect(100, 100, 824, 824, 185).wind(false), roundRect(108, 108, 808, 808, 177).wind(true))
	fill(place(brass), anchor()...)
	return img
}

func pngBytes(img image.Image) []byte {
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&b, img); err != nil {
		log.Fatal(err)
	}
	return b.Bytes()
}

// icns: a header, then one PNG per type code. Retina types (ic11–ic14) are
// the next size up drawn for a half-size slot.
func icns() []byte {
	types := []struct {
		code string
		size int
	}{
		{"icp4", 16}, {"icp5", 32}, {"icp6", 48}, {"ic07", 128}, {"ic08", 256}, {"ic09", 512},
		{"ic10", 1024}, {"ic11", 32}, {"ic12", 64}, {"ic13", 256}, {"ic14", 512},
	}
	cache := map[int][]byte{}
	var body bytes.Buffer
	for _, t := range types {
		data, ok := cache[t.size]
		if !ok {
			data = pngBytes(render(t.size, true))
			cache[t.size] = data
		}
		body.WriteString(t.code)
		_ = binary.Write(&body, binary.BigEndian, uint32(8+len(data)))
		body.Write(data)
	}
	var out bytes.Buffer
	out.WriteString("icns")
	_ = binary.Write(&out, binary.BigEndian, uint32(8+body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

// ico: a directory of PNG images, which Windows reads at every size since Vista.
func ico() []byte {
	sizes := []int{16, 20, 24, 32, 40, 48, 64, 128, 256}
	var images [][]byte
	for _, n := range sizes {
		images = append(images, pngBytes(render(n, false)))
	}
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, n := range sizes {
		dim := uint8(n)
		if n >= 256 {
			dim = 0 // 0 means 256
		}
		_ = binary.Write(&out, binary.LittleEndian, struct {
			W, H, Colors, Reserved uint8
			Planes, BPP            uint16
			Size, Offset           uint32
		}{dim, dim, 0, 0, 1, 32, uint32(len(images[i])), uint32(offset)})
		offset += len(images[i])
	}
	for _, img := range images {
		out.Write(img)
	}
	return out.Bytes()
}

func write(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s (%d KB)\n", path, (len(data)+1023)/1024)
}

func main() {
	write("packaging/macos/Whydah.icns", icns())
	write("packaging/windows/whydah.ico", ico())
	write("packaging/linux/whydah.png", pngBytes(render(512, true)))
	write("internal/web/icon.png", pngBytes(render(128, false)))
	if len(os.Args) > 1 { // a preview sheet: go run ./packaging/icon preview.png
		write(os.Args[1], pngBytes(sheet()))
	}
}

// sheet lays out the sizes side by side on light and dark, to check by eye.
func sheet() *image.NRGBA {
	sizes := []int{512, 128, 64, 32, 16}
	w := 40
	for _, n := range sizes {
		w += n + 24
	}
	out := image.NewNRGBA(image.Rect(0, 0, w, 2*(512+48)))
	for row, bg := range []color.NRGBA{{0xe9, 0xe7, 0xe1, 0xff}, {0x1c, 0x1c, 0x1c, 0xff}} {
		for y := row * 560; y < (row+1)*560; y++ {
			for x := 0; x < w; x++ {
				out.SetNRGBA(x, y, bg)
			}
		}
		x := 24
		for _, n := range sizes {
			img := render(n, n >= 128)
			top := row*560 + 24 + 512 - n
			for yy := 0; yy < n; yy++ {
				for xx := 0; xx < n; xx++ {
					c := img.NRGBAAt(xx, yy)
					a := float64(c.A) / 255
					blend := func(f, b uint8) uint8 { return uint8(float64(f)*a + float64(b)*(1-a)) }
					out.SetNRGBA(x+xx, top+yy, color.NRGBA{blend(c.R, bg.R), blend(c.G, bg.G), blend(c.B, bg.B), 0xff})
				}
			}
			x += n + 24
		}
	}
	return out
}
