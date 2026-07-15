package archive

import (
	"path/filepath"
	"testing"
)

func TestLayout(t *testing.T) {
	layout, err := NewLayout("/var/lib/autostream/archives", "stream-01")
	if err != nil {
		t.Fatal(err)
	}
	if layout.FinalMKV() != filepath.Clean("/var/lib/autostream/archives/tmp/stream-01/final.mkv") {
		t.Fatalf("unexpected final mkv: %s", layout.FinalMKV())
	}
	if layout.FinalMP4() != filepath.Clean("/var/lib/autostream/archives/final/stream-01/final.mp4") {
		t.Fatalf("unexpected final mp4: %s", layout.FinalMP4())
	}
	if layout.PreviewPlaylist() != filepath.Clean("/var/lib/autostream/archives/tmp/stream-01/preview/index.m3u8") {
		t.Fatalf("unexpected preview playlist: %s", layout.PreviewPlaylist())
	}
	if layout.PreviewSegmentPattern() != filepath.Clean("/var/lib/autostream/archives/tmp/stream-01/preview/segment-%06d.ts") {
		t.Fatalf("unexpected preview segment pattern: %s", layout.PreviewSegmentPattern())
	}
}

func TestLayoutRejectsUnsafeStreamID(t *testing.T) {
	if _, err := NewLayout("/tmp/archives", "../secret"); err == nil {
		t.Fatal("expected unsafe stream id to fail")
	}
}

func TestLayoutRejectsFFmpegSyntaxStreamID(t *testing.T) {
	unsafeIDs := []string{
		"stream|evil",
		"stream[evil]",
		"stream,evil",
		"stream:evil",
		"stream evil",
		"stream\nevil",
		"stream\revil",
	}
	for _, id := range unsafeIDs {
		if _, err := NewLayout("/tmp/archives", id); err == nil {
			t.Fatalf("expected unsafe stream id %q to fail", id)
		}
	}
}
