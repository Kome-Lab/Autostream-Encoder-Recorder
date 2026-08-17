package watermarkfeed

import (
	"bytes"
	"image/png"
	"io"
	"net"
	"strings"
	"testing"
	"time"
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

func TestSourcePushesUpdatedFrameWithoutWaitingForPeriodicRefresh(t *testing.T) {
	initial, err := Frame(map[string]any{"watermark_enabled": false})
	if err != nil {
		t.Fatal(err)
	}
	source, err := New(map[string]any{"watermark_enabled": false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(source.InputURL(), "tcp://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.ReadFull(conn, make([]byte, len(initial))); err != nil {
		t.Fatalf("read initial watermark frame: %v", err)
	}

	updated := []byte("updated-watermark-frame")
	if err := source.Update(updated); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(updated))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("updated watermark was not pushed immediately: %v", err)
	}
	if !bytes.Equal(got, updated) {
		t.Fatalf("updated watermark frame=%q, want %q", got, updated)
	}
}
