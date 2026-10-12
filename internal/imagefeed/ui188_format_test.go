package imagefeed

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestUI188PNGFeedKeepsRGBAAcrossOpaqueAndTransparentRevisions(t *testing.T) {
	for _, alpha := range []uint8{0, 255, 127, 255} {
		im := image.NewNRGBA(image.Rect(0, 0, 16, 9))
		for i := 0; i < len(im.Pix); i += 4 {
			copy(im.Pix[i:], []byte{23, 79, 201, alpha})
		}
		var original bytes.Buffer
		if err := png.Encode(&original, im); err != nil {
			t.Fatal(err)
		}
		frame, err := stablePNGFrame(original.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if len(frame) < 26 || frame[24] != 8 || frame[25] != 6 {
			t.Fatal("PNG feed changed decoder pixel format")
		}
		decoded, err := png.Decode(bytes.NewReader(frame))
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < 9; y++ {
			for x := 0; x < 16; x++ {
				if decoded.At(x, y) != im.At(x, y) {
					t.Fatal("normalization changed cover alpha/pixels")
				}
			}
		}
	}
}
