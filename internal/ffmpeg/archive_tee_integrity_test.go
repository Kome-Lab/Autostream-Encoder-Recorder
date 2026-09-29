package ffmpeg

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The regression consumes the production builder verbatim. Only input fixtures
// and a finite duration for otherwise continuous inputs belong to the test.
func TestArchiveTeeIntegrity(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	root := t.TempDir()
	if keep := os.Getenv("AUTOSTREAM_TEST_EVIDENCE_ROOT"); keep != "" {
		root = filepath.Join(keep, "tee-integrity")
		if err := os.MkdirAll(root, 0750); err != nil {
			t.Fatal(err)
		}
	}
	input := filepath.Join(root, "source.mkv")
	createFFmpegFixture(t, bin, input)
	p := EncoderProfile{160, 90, 10, "300k", "64k", 48000, 2}
	for _, kind := range []string{"normal", "required-open-failure", "live-open-failure", "preview-open-failure"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(root, kind)
			if err := os.MkdirAll(filepath.Join(dir, "hls"), 0750); err != nil {
				t.Fatal(err)
			}
			live, mkv, hls := filepath.Join(dir, "live.flv"), filepath.Join(dir, "final.mkv"), filepath.Join(dir, "hls/index.m3u8")
			switch kind {
			case "required-open-failure":
				mkv = filepath.Join(dir, "absent/final.mkv")
			case "live-open-failure":
				live = filepath.Join(dir, "absent/live.flv")
			case "preview-open-failure":
				hls = filepath.Join(dir, "absent/index.m3u8")
			}
			args := BuildLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(input, live, mkv, hls, "", "", p)
			err := archiveRun(t, bin, args)
			if kind == "required-open-failure" {
				if err == nil {
					t.Fatal("mandatory recording failure returned success")
				}
				if _, e := os.Stat(mkv); !os.IsNotExist(e) {
					t.Fatalf("unexpected primary: %v", e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			archiveAssertMedia(t, mkv)
			if kind != "live-open-failure" {
				archiveAssertMedia(t, live)
			}
			if kind != "preview-open-failure" {
				archiveAssertMedia(t, hls)
				archiveAssertSegments(t, hls)
			}
			if kind == "normal" {
				mp4 := filepath.Join(dir, "final.mp4")
				if err := archiveRun(t, bin, BuildRemuxArgs(mkv, mp4)); err != nil {
					t.Fatal(err)
				}
				archiveAssertMedia(t, mp4)
				original, err := os.ReadFile(mkv)
				if err != nil {
					t.Fatal(err)
				}
				for _, n := range []int{32, 291} {
					bad := filepath.Join(dir, fmt.Sprintf("header-only-%d.mkv", n))
					if err := os.WriteFile(bad, original[:n], 0600); err != nil {
						t.Fatal(err)
					}
					if err := archiveCheckMedia(t, bad); err == nil {
						t.Fatalf("header-only %d accepted", n)
					}
				}
				bad := filepath.Join(dir, "corrupt.mkv")
				damaged := append([]byte(nil), original...)
				for i := 0; i < 1024 && i < len(damaged); i++ {
					damaged[i] = 0
				}
				if err := os.WriteFile(bad, damaged, 0600); err != nil {
					t.Fatal(err)
				}
				if err := archiveCheckMedia(t, bad); err == nil {
					t.Fatal("corrupt primary accepted")
				}
			}
		})
	}
}

func archiveRun(t *testing.T, bin string, args []string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	argv, _ := json.Marshal(append([]string{bin}, args...))
	t.Logf("COMMAND %s", argv)
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	t.Logf("ORIGINAL_OUTPUT_BEGIN\n%s\nORIGINAL_OUTPUT_END error=%v", out, err)
	if ctx.Err() != nil {
		t.Fatalf("command exceeded bounded test window: %v", ctx.Err())
	}
	return err
}

type archiveProbe struct {
	Streams []struct {
		Codec    string `json:"codec_name"`
		Kind     string `json:"codec_type"`
		Rate     string `json:"sample_rate"`
		Frames   string `json:"nb_read_frames"`
		TimeBase string `json:"time_base"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func archiveCheckMedia(t *testing.T, path string) error {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("not a nonempty media file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	t.Logf("MEDIA path=%q bytes=%d sha256=%x", path, len(data), sha256.Sum256(data))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"-v", "error", "-count_frames", "-show_streams", "-show_format", "-of", "json", path}
	out, err := exec.CommandContext(ctx, "ffprobe", args...).CombinedOutput()
	t.Logf("FFPROBE path=%q error=%v\n%s", path, err, out)
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	var p archiveProbe
	if err := json.Unmarshal(out, &p); err != nil {
		return err
	}
	video, audio := false, false
	for _, s := range p.Streams {
		n, _ := strconv.Atoi(s.Frames)
		if s.Kind == "video" && s.Codec == "h264" && n > 0 {
			video = true
		}
		if s.Kind == "audio" && s.Codec == "aac" && s.Rate == "48000" && n > 0 {
			audio = true
		}
	}
	duration, _ := strconv.ParseFloat(p.Format.Duration, 64)
	if !video || !audio || duration <= 0 {
		return fmt.Errorf("invalid decoded stream identity/duration: video=%t audio=%t duration=%f", video, audio, duration)
	}
	args = []string{"-hide_banner", "-nostdin", "-v", "error", "-xerror", "-i", path, "-map", "0:v:0", "-map", "0:a:0", "-f", "null", "-"}
	return archiveRun(t, "ffmpeg", args)
}

func archiveAssertMedia(t *testing.T, path string) {
	t.Helper()
	if err := archiveCheckMedia(t, path); err != nil {
		t.Fatalf("invalid primary/sink %q: %v", path, err)
	}
}
func archiveAssertSegments(t *testing.T, playlist string) {
	t.Helper()
	body, err := os.ReadFile(playlist)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasSuffix(line, ".ts") {
			archiveAssertMedia(t, filepath.Join(filepath.Dir(playlist), line))
			count++
		}
	}
	if count == 0 {
		t.Fatal("no HLS segments to verify")
	}
	t.Logf("all %d independent HLS segments decoded", count)
}
