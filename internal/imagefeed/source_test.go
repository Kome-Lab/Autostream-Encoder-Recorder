package imagefeed

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSourceServesInitialFrameAndPushesUpdateImmediately(t *testing.T) {
	initial := []byte("initial-image-frame")
	source, err := New("test image", initial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(source.InputURL(), "tcp://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	gotInitial := make([]byte, len(initial))
	if _, err := io.ReadFull(conn, gotInitial); err != nil {
		t.Fatalf("read initial image frame: %v", err)
	}
	if !bytes.Equal(gotInitial, initial) {
		t.Fatalf("initial image frame=%q, want %q", gotInitial, initial)
	}

	updated := []byte("updated-image-frame")
	if err := source.Update(updated); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	gotUpdated := make([]byte, len(updated))
	if _, err := io.ReadFull(conn, gotUpdated); err != nil {
		t.Fatalf("updated image was not pushed immediately: %v", err)
	}
	if !bytes.Equal(gotUpdated, updated) {
		t.Fatalf("updated image frame=%q, want %q", gotUpdated, updated)
	}
}

func TestSourceRejectsEmptyFramesAndUpdatesAfterClose(t *testing.T) {
	if _, err := New("test image", nil); err == nil {
		t.Fatal("expected empty initial frame to be rejected")
	} else if got, want := err.Error(), "test image frame is required"; got != want {
		t.Fatalf("empty initial frame error=%q, want %q", got, want)
	}

	source, err := New("test image", []byte("frame"))
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Update(nil); err == nil {
		t.Fatal("expected empty updated frame to be rejected")
	} else if got, want := err.Error(), "test image frame is required"; got != want {
		t.Fatalf("empty update error=%q, want %q", got, want)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.Update([]byte("next")); err == nil {
		t.Fatal("expected update after close to be rejected")
	} else if got, want := err.Error(), "test image source is closed"; got != want {
		t.Fatalf("closed update error=%q, want %q", got, want)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestSourceRefreshesCurrentFrameAndClosesLoopbackConnection(t *testing.T) {
	if got, want := frameInterval, 500*time.Millisecond; got != want {
		t.Fatalf("frame interval=%v, want %v", got, want)
	}
	frame := []byte("periodic-frame")
	source, err := New("test image", frame)
	if err != nil {
		t.Fatal(err)
	}
	address := strings.TrimPrefix(source.InputURL(), "tcp://")
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	got := make([]byte, len(frame))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read initial image frame: %v", err)
	}
	started := time.Now()
	if err := conn.SetReadDeadline(time.Now().Add(2 * frameInterval)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read periodic image frame: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("periodic image frame=%q, want %q", got, frame)
	}
	if elapsed := time.Since(started); elapsed < frameInterval/2 {
		t.Fatalf("periodic image frame arrived too early after %v", elapsed)
	}

	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected current connection to close with the source")
	}
	if next, err := net.DialTimeout("tcp", address, 200*time.Millisecond); err == nil {
		_ = next.Close()
		t.Fatal("expected loopback listener to reject connections after close")
	}
}
