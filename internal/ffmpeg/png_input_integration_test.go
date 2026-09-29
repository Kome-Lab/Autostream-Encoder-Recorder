package ffmpeg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/example/autostream-encoder-recorder/internal/imagefeed"
	"image"
	"image/png"
	"io"
	"net"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPNGInputOptionScope(t *testing.T) {
	want := []string{"-thread_queue_size", "8", "-f", "png_pipe", "-framerate", "2", "-threads:v", "1", "-probesize", "32", "-frame_size", "64", "-i", "tcp://127.0.0.1:1234"}
	if got := watermarkInputArgs(want[len(want)-1]); !reflect.DeepEqual(got, want) {
		t.Fatalf("PNG input scope: %v", got)
	}
	if got := watermarkInputArgs("/tmp/cover.png"); !reflect.DeepEqual(got, []string{"-loop", "1", "-i", "/tmp/cover.png"}) {
		t.Fatal(got)
	}
	p := DefaultProfile()
	args := BuildLiveArchiveArgsToOutputTargetWithRuntimeSettingsAndVisualLayers("fixture.mp4", "rtmp://localhost/synthetic", "/tmp/archive.mkv", "/tmp/preview.m3u8", "/tmp/progress", "", "tcp://127.0.0.1:1234", "tcp://127.0.0.1:1235", 0, p)
	for _, key := range []string{"-probesize", "-frame_size"} {
		count := 0
		for i, v := range args {
			if v == key {
				count++
				if i+1 >= len(args) {
					t.Fatal(args)
				}
			}
		}
		if count != 2 {
			t.Fatalf("%s count=%d", key, count)
		}
	}
	if strings.Contains(strings.Join(args, " "), "-fflags +noparse") {
		t.Fatal("parser disabled")
	}
}

// The oracle is independent unpremultiplied RGBA, including hidden RGB under alpha.
func png079Fixture(w, h, kind int) ([]byte, []byte) {
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	seed := uint32(9173)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*im.Stride + x*4
			switch kind {
			case 0: // transparent
			case 1:
				im.Pix[i] = 240
				im.Pix[i+1] = 31
				im.Pix[i+2] = 17
				im.Pix[i+3] = 255
			case 2:
				im.Pix[i] = 19
				im.Pix[i+1] = 203
				im.Pix[i+2] = 77
				im.Pix[i+3] = 127
			default:
				for j := 0; j < 4; j++ {
					seed ^= seed << 13
					seed ^= seed >> 17
					seed ^= seed << 5
					im.Pix[i+j] = byte(seed)
				}
			}
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		panic(err)
	}
	return b.Bytes(), im.Pix
}

// Each frame starts at the unchanged 500ms cadence; fragmented writes are not frame bursts.
func png079Feed(t *testing.T, ctx context.Context, frames [][]byte, chunk int, closeAfter bool) (string, func()) {
	t.Helper()
	ctx, stop := context.WithCancel(ctx)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conn net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, e := ln.Accept()
		if e != nil {
			return
		}
		mu.Lock()
		conn = c
		mu.Unlock()
		defer c.Close()
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for n := 0; ; n++ {
			if len(frames) > 0 {
				frame := frames[n%len(frames)]
				if n >= len(frames) {
					frame = frames[len(frames)-1]
				}
				for off := 0; off < len(frame); {
					end := off + chunk
					if chunk == 0 || end > len(frame) {
						end = len(frame)
					}
					_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
					w, e := c.Write(frame[off:end])
					off += w
					if e != nil {
						return
					}
				}
				if closeAfter && n+1 == len(frames) {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	cleanup := func() {
		stop()
		_ = ln.Close()
		mu.Lock()
		if conn != nil {
			_ = conn.Close()
		}
		mu.Unlock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("feed cleanup exceeded bound")
		}
	}
	return "tcp://" + ln.Addr().String(), cleanup
}

func png079FFmpeg(t *testing.T, args []string, limit time.Duration) ([]byte, string, error, time.Duration, int) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Fatal("required candidate FFmpeg missing; compile test executable and run in candidate OS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Start()
	if err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	err = cmd.Wait()
	elapsed := time.Since(start)
	t.Logf("PID=%d elapsed=%s exit=%d argv=%q stderr=%s", pid, elapsed, cmd.ProcessState.ExitCode(), args, stderr.String())
	return out.Bytes(), stderr.String(), err, elapsed, cmd.ProcessState.ExitCode()
}

func TestPNG079ExactDecode(t *testing.T) {
	for _, tc := range []struct {
		name              string
		w, h, kind, chunk int
	}{{"small-transparent", 320, 180, 0, 0}, {"profile-transparent", 854, 480, 0, 0}, {"profile-active", 854, 480, 1, 0}, {"partial-alpha-fragmented", 320, 180, 2, 17}, {"large-valid-high-entropy", 1920, 1080, 3, 4093}} {
		t.Run(tc.name, func(t *testing.T) {
			data, raw := png079Fixture(tc.w, tc.h, tc.kind)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			url, closeFeed := png079Feed(t, ctx, [][]byte{data}, tc.chunk, false)
			defer closeFeed()
			args := append([]string{"-hide_banner", "-nostdin", "-loglevel", "info"}, watermarkInputArgs(url)...)
			args = append(args, "-threads:v", "1", "-pix_fmt", "rgba", "-frames:v", "1", "-f", "rawvideo", "pipe:1")
			out, _, err, elapsed, _ := png079FFmpeg(t, args, 6*time.Second)
			t.Logf("PNG bytes=%d RGBA bytes=%d expected=%x actual=%x", len(data), len(raw), sha256.Sum256(raw), sha256.Sum256(out))
			if err != nil || elapsed >= 6*time.Second || !bytes.Equal(out, raw) {
				t.Fatalf("decode mismatch or timeout err=%v bytes=%d/%d", err, len(out), len(raw))
			}
		})
	}
}

// Exercise the real full-write/revision transport with two independent imagefeeds.
func TestPNG079ProductionFeedsRevision(t *testing.T) {
	transparent, _ := png079Fixture(320, 180, 0)
	active, _ := png079Fixture(320, 180, 1)
	marker := func(blue bool) []byte {
		im := image.NewNRGBA(image.Rect(0, 0, 320, 180))
		for y := 0; y < 30; y++ {
			for x := 0; x < 40; x++ {
				i := y*im.Stride + x*4
				im.Pix[i+3] = 255
				if blue {
					im.Pix[i+2] = 255
				} else {
					im.Pix[i+1] = 255
				}
			}
		}
		var b bytes.Buffer
		_ = png.Encode(&b, im)
		return b.Bytes()
	}
	cover, err := imagefeed.New("cover", transparent)
	if err != nil {
		t.Fatal(err)
	}
	defer cover.Close()
	watermark, err := imagefeed.New("watermark", marker(false))
	if err != nil {
		t.Fatal(err)
	}
	defer watermark.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-filter_complex_threads", "1", "-re", "-f", "lavfi", "-i", "color=c=black:s=320x180:r=15"}
	args = append(args, watermarkInputArgs(cover.InputURL())...)
	args = append(args, watermarkInputArgs(watermark.InputURL())...)
	args = append(args, "-filter_complex", "[0:v]format=rgba[b];[b][1:v]overlay=format=rgb[c];[c][2:v]overlay=format=rgb,format=rgba[v]", "-map", "[v]", "-threads:v", "1", "-pix_fmt", "rgba", "-f", "rawvideo", "pipe:1")
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	frames := make(chan []byte, 1)
	go func() {
		defer close(frames)
		for {
			f := make([]byte, 320*180*4)
			if _, e := io.ReadFull(pipe, f); e != nil {
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		cancel()
		_ = cmd.Wait()
		t.Logf("real feeds PID=%d elapsed=%s exit=%d stderr=%s", pid, time.Since(start), cmd.ProcessState.ExitCode(), stderr.String())
	}()
	check := func(stage string, wantCover, wantBlue bool) {
		t.Helper()
		deadline := time.NewTimer(6 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					t.Fatalf("%s process exited", stage)
				}
				base := f[(90*320+160)*4:][:4]
				mark := f[(10*320+10)*4:][:4]
				expected := []byte{0, 0, 0, 255}
				if wantCover {
					expected = []byte{240, 31, 17, 255}
				}
				wm := []byte{0, 255, 0, 255}
				if wantBlue {
					wm = []byte{0, 0, 255, 255}
				}
				if bytes.Equal(base, expected) && bytes.Equal(mark, wm) {
					t.Logf("stage=%s PID=%d elapsed=%s centre=%v marker=%v", stage, pid, time.Since(start), base, mark)
					return
				}
			case <-deadline.C:
				t.Fatalf("%s no decoded expected revision inside unchanged six-second bound", stage)
			}
		}
	}
	check("initial-transparent", false, false)
	if err := cover.UpdateAndWait(ctx, active); err != nil {
		t.Fatal(err)
	}
	check("cover-active-watermark-retained", true, false)
	if err := watermark.UpdateAndWait(ctx, marker(true)); err != nil {
		t.Fatal(err)
	}
	check("independent-watermark-revision", true, true)
	if err := cover.UpdateAndWait(ctx, transparent); err != nil {
		t.Fatal(err)
	}
	check("cover-hidden-watermark-retained", false, true)
}

func TestPNG079Sequence(t *testing.T) {
	var frames [][]byte
	var expected []byte
	for _, kind := range []int{0, 1, 2, 0, 2, 1} {
		p, r := png079Fixture(320, 180, kind)
		frames = append(frames, p)
		expected = append(expected, r...)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, closeFeed := png079Feed(t, ctx, frames, 17, false)
	defer closeFeed()
	args := append([]string{"-hide_banner", "-nostdin"}, watermarkInputArgs(url)...)
	args = append(args, "-threads:v", "1", "-pix_fmt", "rgba", "-fps_mode", "passthrough", "-frames:v", "6", "-f", "rawvideo", "pipe:1")
	out, _, err, _, _ := png079FFmpeg(t, args, 6*time.Second)
	if err != nil || !bytes.Equal(out, expected) {
		t.Fatalf("sequence/alpha mismatch %v %x/%x", err, sha256.Sum256(out), sha256.Sum256(expected))
	}
}

func TestPNG079TwinInputComparison(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacy-%t", legacy), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p, _ := png079Fixture(320, 180, 0)
			q, _ := png079Fixture(320, 180, 2)
			u, close1 := png079Feed(t, ctx, [][]byte{p}, 17, false)
			defer close1()
			v, close2 := png079Feed(t, ctx, [][]byte{q}, 37, false)
			defer close2()
			input := func(url string) []string {
				if legacy {
					return []string{"-thread_queue_size", "8", "-f", "png_pipe", "-framerate", "2", "-i", url}
				}
				return watermarkInputArgs(url)
			}
			args := []string{"-hide_banner", "-nostdin", "-loglevel", "debug", "-filter_complex_threads", "1", "-f", "lavfi", "-i", "testsrc2=s=320x180:r=15"}
			args = append(args, input(u)...)
			args = append(args, input(v)...)
			args = append(args, "-filter_complex", "[0:v]format=rgba[b];[b][1:v]overlay=format=auto[c];[c][2:v]overlay=format=auto,format=yuv420p[v]", "-map", "[v]", "-threads:v", "1", "-frames:v", "1", "-f", "rawvideo", "pipe:1")
			out, stderr, err, _, _ := png079FFmpeg(t, args, 6*time.Second)
			t.Logf("legacy=%t input-info-completed=%d output_bytes=%d", legacy, strings.Count(stderr, "After avformat_find_stream_info"), len(out))
			if !legacy && (err != nil || len(out) != 320*180*3/2) {
				t.Fatalf("new twin input failed %v", err)
			}
		})
	}
}

func TestPNG079InvalidAndCancellation(t *testing.T) {
	pngBytes, _ := png079Fixture(320, 180, 1)
	for _, tc := range []struct {
		name   string
		frames [][]byte
		close  bool
	}{{"no-frame", nil, false}, {"truncated-disconnect", [][]byte{pngBytes[:len(pngBytes)/2]}, true}, {"bad-signature", [][]byte{[]byte("not a png\n")}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			url, closeFeed := png079Feed(t, ctx, tc.frames, 17, tc.close)
			defer closeFeed()
			args := append([]string{"-hide_banner", "-nostdin", "-xerror"}, watermarkInputArgs(url)...)
			args = append(args, "-threads:v", "1", "-pix_fmt", "rgba", "-frames:v", "1", "-f", "rawvideo", "pipe:1")
			out, _, err, _, _ := png079FFmpeg(t, args, 2*time.Second)
			if err == nil || len(out) != 0 {
				t.Fatalf("invalid input yielded false success, bytes=%d err=%v", len(out), err)
			}
		})
	}
}
