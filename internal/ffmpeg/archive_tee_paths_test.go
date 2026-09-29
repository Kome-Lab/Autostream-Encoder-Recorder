package ffmpeg

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/audioingest"
)

func TestArchiveTeeProductionPaths(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	p := EncoderProfile{160, 90, 10, "300k", "64k", 48000, 2}
	root := t.TempDir()
	if keep := os.Getenv("AUTOSTREAM_TEST_EVIDENCE_ROOT"); keep != "" {
		root = filepath.Join(keep, "production-paths")
		if err := os.MkdirAll(root, 0750); err != nil {
			t.Fatal(err)
		}
	}
	input := filepath.Join(root, "source.mkv")
	createFFmpegFixture(t, "ffmpeg", input)
	for _, kind := range []string{"external", "external-runtime", "external-visual", "discord", "discord-watermark", "discord-visual", "worker", "worker-watermark", "worker-visual"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(root, kind)
			if err := os.MkdirAll(filepath.Join(dir, "hls"), 0750); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			live, mkv, hls := filepath.Join(dir, "live.flv"), filepath.Join(dir, "final.mkv"), filepath.Join(dir, "hls/index.m3u8")
			manager := audioingest.NewManager(dir)
			bridge, err := manager.StartBridge("synthetic-source")
			if err != nil {
				t.Fatal(err)
			}
			defer manager.StopBridge("synthetic-source")
			frame, _ := png079Fixture(p.Width, p.Height, 2)
			cover, closeCover := png079Feed(t, ctx, [][]byte{frame}, 0, false)
			defer closeCover()
			watermark, closeWatermark := png079Feed(t, ctx, [][]byte{frame}, 0, false)
			defer closeWatermark()
			var args []string
			switch kind {
			case "external":
				args = BuildLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(input, live, mkv, hls, "", "", p)
			case "external-runtime":
				args = BuildLiveArchiveArgsToOutputTargetWithRuntimeSettings(input, live, mkv, hls, "", "", "", 0, p)
			case "external-visual":
				args = BuildLiveArchiveArgsToOutputTargetWithRuntimeSettingsAndVisualLayers(input, live, mkv, hls, "", "", cover, watermark, 0, p)
			case "discord":
				args = BuildDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(bridge.SDPPath, live, mkv, hls, "", "", p)
			case "discord-watermark":
				args = BuildDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(bridge.SDPPath, live, mkv, hls, "", "", watermark, 0, p)
			case "discord-visual":
				args = BuildDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettingsAndVisualLayers(bridge.SDPPath, live, mkv, hls, "", "", cover, watermark, 0, p)
			default:
				video := archiveJPEGFeed(t, ctx)
				switch kind {
				case "worker":
					args = BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(video, bridge.SDPPath, live, mkv, hls, "", "", "", 0, p)
				case "worker-watermark":
					args = BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark(video, bridge.SDPPath, live, mkv, hls, "", "", watermark, p)
				case "worker-visual":
					args = BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettingsAndVisualLayers(video, bridge.SDPPath, live, mkv, hls, "", "", cover, watermark, 0, p)
				}
			}
			archiveOutputScope(t, args)
			// Bound this synthetic continuous source without replacing any builder flag.
			args = append(append(append([]string{}, args[:len(args)-3]...), "-t", "4"), args[len(args)-3:]...)
			if err := archiveRun(t, "ffmpeg", args); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{mkv, live, hls} {
				archiveAssertMedia(t, path)
			}
			archiveAssertSegments(t, hls)
		})
	}
}

func archiveOutputScope(t *testing.T, args []string) {
	t.Helper()
	lastInput, header, headers := -1, -1, 0
	for i, arg := range args {
		if arg == "-i" {
			lastInput = i
		}
		if arg == "-flags:v" {
			headers++
			header = i
			if i+1 >= len(args) || args[i+1] != "+global_header" {
				t.Fatal("incorrect video header flags")
			}
		}
	}
	if headers != 1 || header <= lastInput {
		t.Fatalf("header is not a single video output option: %v", args)
	}
	tee := args[len(args)-1]
	for _, required := range []string{"[f=matroska:onfail=abort]", "[f=flv:onfail=ignore]", "f=hls:onfail=ignore"} {
		if !strings.Contains(tee, required) {
			t.Fatalf("missing required/optional policy %q", required)
		}
	}
}

// A paced synthetic Worker transport fixture; it does not claim a real Worker.
func archiveJPEGFeed(t *testing.T, ctx context.Context) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	im := image.NewRGBA(image.Rect(0, 0, 160, 90))
	for y := 0; y < 90; y++ {
		for x := 0; x < 160; x++ {
			im.SetRGBA(x, y, color.RGBA{20, 40, 220, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conn net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		mu.Lock()
		conn = c
		mu.Unlock()
		defer c.Close()
		tick := time.NewTicker(time.Second / 60)
		defer tick.Stop()
		for {
			_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.Write(b.Bytes()); err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		if conn != nil {
			_ = conn.Close()
		}
		mu.Unlock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("JPEG fixture did not close")
		}
	})
	return "tcp://" + ln.Addr().String()
}
