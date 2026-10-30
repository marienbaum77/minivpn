package ui

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"

	"github.com/marienbaum77/minivpn/internal/session"
)

// Tray colors: grey off, amber while starting or stopping, green on, red
// when the last session ended with an error.
var (
	colorOff  = color.RGBA{0x9c, 0xa3, 0xaf, 0xff}
	colorBusy = color.RGBA{0xf5, 0x9e, 0x0b, 0xff}
	colorOn   = color.RGBA{0x22, 0xc5, 0x5e, 0xff}
	colorBad  = color.RGBA{0xef, 0x44, 0x44, 0xff}
)

func stateColor(st session.Status) color.RGBA {
	switch {
	case st.Phase == session.On:
		return colorOn
	case st.Phase != session.Off:
		return colorBusy
	case st.Error != "":
		return colorBad
	}
	return colorOff
}

// circleICO draws an anti-aliased dot and wraps the PNG in an ICO container,
// which is what the Windows tray takes.
func circleICO(c color.RGBA) []byte {
	const size = 32
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	center, radius := float64(size)/2, float64(size)/2-2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := math.Hypot(float64(x)+0.5-center, float64(y)+0.5-center)
			cov := math.Max(0, math.Min(1, radius-d+0.5))
			if cov == 0 {
				continue
			}
			px := c
			if d > radius-2.5 { // darker rim keeps it visible on light taskbars
				px = color.RGBA{c.R * 3 / 4, c.G * 3 / 4, c.B * 3 / 4, 0xff}
			}
			px.A = uint8(cov * 255)
			img.SetRGBA(x, y, premultiply(px))
		}
	}
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, img)

	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, [3]uint16{0, 1, 1}) // reserved, type icon, one image
	binary.Write(&ico, binary.LittleEndian, struct {
		W, H, Colors, Reserved uint8
		Planes, BitCount       uint16
		Size, Offset           uint32
	}{size, size, 0, 0, 1, 32, uint32(pngBuf.Len()), 6 + 16})
	ico.Write(pngBuf.Bytes())
	return ico.Bytes()
}

func premultiply(c color.RGBA) color.RGBA {
	a := uint32(c.A)
	return color.RGBA{uint8(uint32(c.R) * a / 255), uint8(uint32(c.G) * a / 255), uint8(uint32(c.B) * a / 255), c.A}
}
