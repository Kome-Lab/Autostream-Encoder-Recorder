package ffmpeg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLiveTeePreviewWithFFmpeg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping FFmpeg integration test in short mode")
	}
	ffmpegBin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg is not installed: %v", err)
	}

	root := filepath.Join(t.TempDir(), "tee path [one] O'Brien")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "input.mkv")
	createFFmpegFixture(t, ffmpegBin, input)
	profile := EncoderProfile{Width: 160, Height: 90, FPS: 10, VideoBitrate: "300k", AudioBitrate: "64k", SampleRate: 48000, KeyframeSec: 2}

	t.Run("writes playable start-to-now HLS and ENDLIST", func(t *testing.T) {
		previewDir := filepath.Join(root, "preview output [one] O'Brien")
		if err := os.MkdirAll(previewDir, 0o750); err != nil {
			t.Fatal(err)
		}
		playlist := filepath.Join(previewDir, "index.m3u8")
		outputFLV := filepath.Join(root, "live output.flv")
		archiveMKV := filepath.Join(root, "archive output.mkv")
		args := BuildLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(input, outputFLV, archiveMKV, playlist, "", "", profile)
		runFFmpeg(t, ffmpegBin, args)

		body, err := os.ReadFile(playlist)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if !strings.Contains(text, "#EXT-X-ENDLIST") {
			t.Fatalf("completed preview playlist is missing ENDLIST:\n%s", text)
		}
		if strings.Contains(text, filepath.ToSlash(root)) || strings.Contains(text, filepath.Clean(root)) {
			t.Fatalf("playlist contains an absolute local path:\n%s", text)
		}
		segmentPattern := regexp.MustCompile(`(?m)^segment-[0-9]{6}\.ts$`)
		segments := segmentPattern.FindAllString(text, -1)
		if len(segments) < 7 || !strings.Contains(text, "#EXT-X-MEDIA-SEQUENCE:0") {
			t.Fatalf("expected the complete start-to-now playlist, got %d segments:\n%s", len(segments), text)
		}
		for _, name := range segments {
			info, err := os.Stat(filepath.Join(previewDir, name))
			if err != nil {
				t.Fatalf("segment %s is missing: %v", name, err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				t.Fatalf("segment %s is not a non-empty regular file", name)
			}
		}
	})

	t.Run("preview open failure does not stop other slaves", func(t *testing.T) {
		playlist := filepath.Join(root, "missing-parent", "preview", "index.m3u8")
		outputFLV := filepath.Join(root, "isolated live.flv")
		archiveMKV := filepath.Join(root, "isolated archive.mkv")
		args := BuildLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(input, outputFLV, archiveMKV, playlist, "", "", profile)
		runFFmpeg(t, ffmpegBin, args)
		for _, path := range []string{outputFLV, archiveMKV} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("non-preview slave did not complete after preview failure: %v", err)
			}
			if info.Size() == 0 {
				t.Fatalf("non-preview slave is empty after preview failure: %s", path)
			}
		}
	})

	t.Run("live output failure does not stop archive or preview", func(t *testing.T) {
		previewDir := filepath.Join(root, "provider failure preview")
		if err := os.MkdirAll(previewDir, 0o750); err != nil {
			t.Fatal(err)
		}
		playlist := filepath.Join(previewDir, "index.m3u8")
		// The parent is intentionally absent. This simulates a provider/relay
		// connection that cannot be opened while keeping local outputs valid.
		liveOutput := filepath.Join(root, "missing live parent", "live.flv")
		archiveMKV := filepath.Join(root, "provider failure archive.mkv")
		args := BuildLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(input, liveOutput, archiveMKV, playlist, "", "", profile)
		runFFmpeg(t, ffmpegBin, args)

		archiveInfo, err := os.Stat(archiveMKV)
		if err != nil {
			t.Fatalf("archive output did not survive live output failure: %v", err)
		}
		if archiveInfo.Size() == 0 {
			t.Fatal("archive output is empty after live output failure")
		}
		playlistBody, err := os.ReadFile(playlist)
		if err != nil {
			t.Fatalf("preview playlist did not survive live output failure: %v", err)
		}
		if !strings.Contains(string(playlistBody), "#EXT-X-ENDLIST") {
			t.Fatalf("preview playlist is incomplete after live output failure:\n%s", playlistBody)
		}
	})
}

func createFFmpegFixture(t *testing.T, ffmpegBin, output string) {
	t.Helper()
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000",
		"-t", "15",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", output,
	}
	runFFmpeg(t, ffmpegBin, args)
}

func runFFmpeg(t *testing.T, ffmpegBin string, args []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args = append([]string{"-loglevel", "error"}, args...)
	output, err := exec.CommandContext(ctx, ffmpegBin, args...).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("ffmpeg timed out: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("ffmpeg failed: %v\n%s", err, output)
	}
}
