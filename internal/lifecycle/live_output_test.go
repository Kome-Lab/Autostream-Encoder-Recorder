package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
)

func TestDryRunCreatesArchiveLayoutMetadataAndCommands(t *testing.T) {
	root := t.TempDir()
	runner := &ffmpeg.DryRunRunner{}
	manager := Manager{ArchiveRoot: root, FFmpegBin: "ffmpeg", Runner: runner, Uploader: archive.DryRunUploader{}}
	result, err := manager.DryRunToOutputTarget(context.Background(), StreamJob{
		StreamID: "stream-01", Name: "Morning Stream", InputURL: "srt://input.example.com:9000?mode=caller&passphrase=input-secret",
		RTMPURL: "rtmps://youtube.example.com/live2", StreamKey: "<YOUTUBE_STREAM_KEY>",
		StartedAt: time.Date(2026, 5, 29, 1, 2, 3, 0, time.UTC), DryRun: true,
	}, "rtmps://youtube.example.com/live2/<YOUTUBE_STREAM_KEY>")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Metadata.Commands) != 2 {
		t.Fatalf("expected 2 ffmpeg commands, got %#v", result.Metadata.Commands)
	}
	for _, command := range result.Metadata.Commands {
		for _, arg := range command.Args {
			if arg == "<YOUTUBE_STREAM_KEY>" || arg == "rtmps://youtube.example.com/live2/<YOUTUBE_STREAM_KEY>" {
				t.Fatalf("stream key leaked in command metadata: %#v", result.Metadata.Commands)
			}
			if strings.Contains(arg, "input-secret") {
				t.Fatalf("input URL secret leaked in command metadata: %#v", result.Metadata.Commands)
			}
		}
	}
	if result.Metadata.StartedAtJST != "2026-05-29T10:02:03+09:00" {
		t.Fatalf("unexpected JST timestamp: %s", result.Metadata.StartedAtJST)
	}
	if result.Metadata.Upload.FileIDs["metadata.json"] == "" || result.Metadata.Upload.FileIDs["logs.jsonl"] == "" {
		t.Fatalf("expected dry-run upload IDs in metadata: %#v", result.Metadata.Upload)
	}
	if strings.Contains(result.Metadata.Archive["final_mp4"], root) || result.Metadata.Archive["final_mp4"] != "final.mp4" {
		t.Fatalf("archive metadata should expose logical artifact names only: %#v", result.Metadata.Archive)
	}
	for _, path := range []string{result.Layout.TmpMetadata(), result.Layout.FinalMetadata(), result.Layout.TmpLogs(), result.Layout.FinalLogs()} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s: %v", path, err)
		}
	}
	if filepath.Base(result.Layout.FinalMP4()) != "final.mp4" {
		t.Fatalf("unexpected final mp4: %s", result.Layout.FinalMP4())
	}
}

func TestDryRunRejectsUnsafeStreamID(t *testing.T) {
	manager := Manager{ArchiveRoot: t.TempDir(), Runner: &ffmpeg.DryRunRunner{}}
	if _, err := manager.DryRunToOutputTarget(context.Background(), StreamJob{StreamID: "../secret", Name: "bad"}, "rtmps://youtube.example.com/live2/key"); err == nil {
		t.Fatal("expected unsafe stream id to fail")
	}
}
