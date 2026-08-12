package watermarkfeed

import (
	"bytes"
	"image/png"
	"testing"
)

func TestFrameUsesTransparentPNGWhenWatermarkIsDisabled(t *testing.T) {
	frame, err := Frame(map[string]any{"watermark_enabled": false})
	if err != nil {
		t.Fatal(err)
	}
	image, err := png.Decode(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if got := image.Bounds().Dx(); got != 1 {
		t.Fatalf("transparent frame width=%d", got)
	}
}

func TestFrameRejectsInvalidWatermarkData(t *testing.T) {
	if _, err := Frame(map[string]any{"watermark_enabled": true, "watermark_image_data_url": "data:image/png;base64,iVBORw0KGgo="}); err == nil {
		t.Fatal("expected invalid image to be rejected before replacing the live watermark")
	}
}
