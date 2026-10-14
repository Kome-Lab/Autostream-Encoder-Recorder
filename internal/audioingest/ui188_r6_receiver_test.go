//go:build autostream_media_diagnostics

package audioingest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/imagefeed"
)

// Both real receivers run together. The Worker JPEG is the S0 profile's
// 854x480 scene; managed PNG variants remain 1920x1080. The bridge repeats
// the JPEG at 60fps, matching the consuming graph that lost RTP.
func TestUI188R6ReceiverConsumption(t *testing.T) {
	requireSustainedReceiverResources(t)
	for i := 0; i < 2; i++ {
		t.Run(fmt.Sprintf("stream-%d", i), func(t *testing.T) {
			t.Parallel()
			r6Receiver(t)
		})
	}
}

// R7's successful resource condition reserves no cores. Verify the actual
// Linux capacity before this blocking 720s regression; an unqualified runner
// fails explicitly rather than skipping the original media acceptance.
func requireSustainedReceiverResources(t *testing.T) {
	t.Helper()
	if runtime.NumCPU() < 4 {
		t.Fatalf("sustained receiver needs at least 4 visible CPUs, got %d", runtime.NumCPU())
	}
	available := int64(0)
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) == 3 && fields[0] == "MemAvailable:" {
			available, err = strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			available *= 1024
		}
	}
	if available < 8<<30 {
		t.Fatalf("sustained receiver needs 8GiB available VM memory, got %d", available)
	}
	data, err = os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	current := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::/") {
			current = filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::/"))
		}
	}
	if current == "" {
		t.Fatal("unified cgroup path unavailable")
	}
	for directory := current; ; directory = filepath.Dir(directory) {
		for _, name := range []string{"cpu.max", "memory.max"} {
			data, err := os.ReadFile(filepath.Join(directory, name))
			if os.IsNotExist(err) && directory == "/sys/fs/cgroup" {
				t.Logf("root %s has no quota interface; physical CPU/memory checked", name)
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Fields(string(data))
			if len(fields) == 0 {
				t.Fatalf("missing effective %s", name)
			}
			if fields[0] != "max" {
				limit, err := strconv.ParseInt(fields[0], 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				if name == "memory.max" && limit < 8<<30 {
					t.Fatalf("memory quota below 8GiB: %d", limit)
				}
				if name == "cpu.max" {
					if len(fields) != 2 {
						t.Fatal("invalid CPU quota")
					}
					period, err := strconv.ParseInt(fields[1], 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					if period <= 0 || limit < 4*period {
						t.Fatalf("CPU quota below 4: %s", string(data))
					}
				}
			}
			t.Logf("actual resource %s/%s=%s", directory, name, strings.TrimSpace(string(data)))
		}
		if directory == "/sys/fs/cgroup" {
			break
		}
		if !strings.HasPrefix(directory, "/sys/fs/cgroup/") {
			t.Fatal("cgroup path escaped mount")
		}
	}
	t.Logf("qualified visible CPUs=%d VM available memory=%d", runtime.NumCPU(), available)
}

func r6Receiver(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Fatal("required FFmpeg unavailable")
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 780*time.Second)
	defer cancel()
	manager := NewManager(root)
	bridge, err := manager.StartBridge("receiver-regression")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.StopOwnedBridge(bridge)
	inputErr := make(chan error, 1)
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		var seq uint16
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				seq++
				_, e := manager.Add(IngestRequest{StreamID: bridge.StreamID, Source: "receiver-tone", Packets: []Packet{{SSRC: 81001, UserID: "synthetic", Sequence: seq, Timestamp: uint32(seq) * 960, OpusBase64: base64.StdEncoding.EncodeToString(tone440OpusFrame)}}})
				if e != nil {
					inputErr <- e
					return
				}
			}
		}
	}()
	video := r6JPEG(t, ctx)
	layer := func(watermark bool) []byte {
		im := image.NewNRGBA(image.Rect(0, 0, 1920, 1080))
		for y := 0; y < 1080; y++ {
			for x := 0; x < 1920; x++ {
				if watermark {
					if x < 180 && y < 100 {
						im.SetNRGBA(x, y, color.NRGBA{255, 255, 255, 255})
					}
				} else {
					im.SetNRGBA(x, y, color.NRGBA{220, 40, 20, 255})
				}
			}
		}
		var b bytes.Buffer
		if e := png.Encode(&b, im); e != nil {
			t.Fatal(e)
		}
		return b.Bytes()
	}
	cover, err := imagefeed.New("receiver-cover", layer(false))
	if err != nil {
		t.Fatal(err)
	}
	defer cover.Close()
	wm, err := imagefeed.New("receiver-watermark", layer(true))
	if err != nil {
		t.Fatal(err)
	}
	defer wm.Close()
	if err = os.Mkdir(filepath.Join(root, "hls"), 0700); err != nil {
		t.Fatal(err)
	}
	p := ffmpeg.EncoderProfile{Width: 854, Height: 480, FPS: 15, VideoBitrate: "800k", AudioBitrate: "96k", SampleRate: 48000, KeyframeSec: 2}
	mkv := filepath.Join(root, "recording.mkv")
	hls := filepath.Join(root, "hls", "index.m3u8")
	args := ffmpeg.BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettingsAndVisualLayers(video, bridge.SDPPath, filepath.Join(root, "live.flv"), mkv, hls, filepath.Join(root, "progress"), "", cover.InputURL(), wm.InputURL(), 0, p)
	args = append(append(append([]string{}, args[:len(args)-3]...), "-t", "720"), args[len(args)-3:]...)
	cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-loglevel", "error"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	observed := 0
	maxDrops := uint64(0)
	var runErr error
run:
	for {
		select {
		case runErr = <-done:
			break run
		case <-tick.C:
			s := captureTimelineSocket(bridge.Port, 0)
			if s.Observed {
				observed++
				n, e := strconv.ParseUint(s.Drops, 10, 64)
				if e != nil {
					t.Error(e)
				}
				if n > maxDrops {
					maxDrops = n
				}
			}
		}
	}
	if runErr != nil {
		t.Errorf("real FFmpeg receiver: %v; %s", runErr, stderr.String())
	}
	if observed == 0 || maxDrops != 0 {
		t.Errorf("receiver observed=%d kernel drops=%d", observed, maxDrops)
	}
	select {
	case e := <-inputErr:
		t.Error(e)
	default:
	}
	for _, path := range []string{mkv, hls} {
		data, e := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "a:0", "-show_packets", "-show_entries", "packet=pts_time,duration_time", "-of", "json", path).Output()
		if e != nil {
			t.Error(e)
			continue
		}
		var value struct {
			Packets []struct {
				PTS      string `json:"pts_time"`
				Duration string `json:"duration_time"`
			} `json:"packets"`
		}
		if e = json.Unmarshal(data, &value); e != nil {
			t.Error(e)
			continue
		}
		if len(value.Packets) < 33500 {
			t.Errorf("audio denominator %d", len(value.Packets))
		}
		maxGap := 0.0
		var previous, duration float64
		for i, packet := range value.Packets {
			pts, _ := strconv.ParseFloat(packet.PTS, 64)
			d, _ := strconv.ParseFloat(packet.Duration, 64)
			if i > 0 {
				gap := pts - previous - duration
				if gap > maxGap {
					maxGap = gap
				}
			}
			previous, duration = pts, d
		}
		if maxGap > .05 {
			t.Errorf("original 50ms uncovered bound: %f", maxGap)
		}
		if previous < 719 {
			t.Errorf("same-run AAC/HLS duration: %f", previous)
		}
		t.Logf("real receiver path=%s packets=%d max_uncovered=%f observed=%d drops=%d", filepath.Base(path), len(value.Packets), maxGap, observed, maxDrops)
	}
}

func r6JPEG(t *testing.T, ctx context.Context) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	im := image.NewRGBA(image.Rect(0, 0, 854, 480))
	var b bytes.Buffer
	if err = jpeg.Encode(&b, im, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	go func() {
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		tick := time.NewTicker(time.Second / 60)
		defer tick.Stop()
		for {
			c.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if _, e = c.Write(b.Bytes()); e != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return "tcp://" + ln.Addr().String()
}
