//go:build autostream_media_diagnostics

package audioingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/imagefeed"
)

type timelineRTP struct {
	AtNS  int64   `json:"elapsed_ns"`
	Seq   uint16  `json:"sequence"`
	TS    uint32  `json:"rtp_timestamp"`
	Bytes int     `json:"bytes"`
	RMS   float64 `json:"rms"`
	SHA   string  `json:"payload_sha256"`
}
type timelineOpus struct {
	AtNS   int64  `json:"elapsed_ns"`
	Seq    uint16 `json:"sequence"`
	TS     uint32 `json:"timestamp"`
	Hz     int    `json:"marker_hz"`
	Result Result `json:"result"`
}

// Capture the unmodified real mixer output before forwarding each whole UDP
// datagram to FFmpeg. The proxy is a test observer, not a new product transport.
func TestAudioTimelineProductionInput(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	root := t.TempDir()
	if keep := os.Getenv("AUTOSTREAM_TEST_EVIDENCE_ROOT"); keep != "" {
		root = filepath.Join(keep, "audio-timeline")
		if err := os.MkdirAll(root, 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "hls"), 0750); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(root)
	bridge, err := manager.StartBridge("synthetic-audio")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.StopOwnedBridge(bridge)
	tap, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: bridge.Port})
	if err != nil {
		t.Fatal(err)
	}
	defer tap.Close()
	sender, err := openRTPSender()
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	port, err := freeUDPPort()
	if err != nil {
		t.Fatal(err)
	}
	sdp := filepath.Join(root, "observed.sdp")
	if err := os.WriteFile(sdp, []byte(sdpForPort(port)), 0600); err != nil {
		t.Fatal(err)
	}
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	start := time.Now()
	var packets []timelineRTP
	var input []timelineOpus
	var sockets []timelineSocket
	var mu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	inputDone := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		defer close(done)
		buf := make([]byte, 8192)
		for {
			n, _, err := tap.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 12 {
				errs <- fmt.Errorf("short RTP")
				return
			}
			sum := 0.0
			for i := 12; i+1 < n; i += 2 {
				v := float64(int16(binary.BigEndian.Uint16(buf[i : i+2])))
				sum += v * v
			}
			h := sha256.Sum256(buf[12:n])
			r := timelineRTP{time.Since(start).Nanoseconds(), binary.BigEndian.Uint16(buf[2:4]), binary.BigEndian.Uint32(buf[4:8]), n, math.Sqrt(sum / float64((n-12)/2)), fmt.Sprintf("%x", h)}
			mu.Lock()
			packets = append(packets, r)
			if r.Seq%10 == 0 {
				sockets = append(sockets, captureTimelineSocket(port, time.Since(start).Nanoseconds()))
			}
			mu.Unlock()
			if _, err := sender.WriteToUDP(buf[:n], target); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer close(inputDone)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		var seq uint16
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				seq++
				hz, opus := 440, tone440OpusFrame
				if (int(seq)/50)%2 == 1 {
					hz, opus = 880, tone880OpusFrame
				}
				at := time.Since(start).Nanoseconds()
				ts := uint32(seq) * 960
				r, err := manager.Add(IngestRequest{StreamID: bridge.StreamID, Source: "synthetic-markers", Packets: []Packet{{SSRC: 81001, UserID: "synthetic", Sequence: seq, Timestamp: ts, OpusBase64: base64.StdEncoding.EncodeToString(opus)}}})
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				input = append(input, timelineOpus{at, seq, ts, hz, r})
				mu.Unlock()
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	video := timelineJPEG(t, ctx)
	im := image.NewNRGBA(image.Rect(0, 0, 160, 90))
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, im); err != nil {
		t.Fatal(err)
	}
	cover, err := imagefeed.New("timeline cover", pngBytes.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer cover.Close()
	wm, err := imagefeed.New("timeline watermark", pngBytes.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer wm.Close()
	p := ffmpeg.EncoderProfile{Width: 160, Height: 90, FPS: 10, VideoBitrate: "300k", AudioBitrate: "64k", SampleRate: 48000, KeyframeSec: 2}
	mkv, live, hls := filepath.Join(root, "final.mkv"), filepath.Join(root, "live.flv"), filepath.Join(root, "hls/index.m3u8")
	args := ffmpeg.BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettingsAndVisualLayers(video, sdp, live, mkv, hls, "", "", cover.InputURL(), wm.InputURL(), 0, p)
	args = append(append(append([]string{}, args[:len(args)-3]...), "-t", "12"), args[len(args)-3:]...)
	args = append([]string{"-loglevel", "debug"}, args...)
	argv, _ := json.Marshal(args)
	t.Logf("PRODUCTION_ARGV %s", argv)
	timeout, stopCommand := context.WithTimeout(ctx, 35*time.Second)
	out, runErr := exec.CommandContext(timeout, "ffmpeg", args...).CombinedOutput()
	stopCommand()
	close(stop)
	<-inputDone
	tap.Close()
	<-done
	t.Logf("FFMPEG_ORIGINAL_BEGIN\n%s\nFFMPEG_ORIGINAL_END error=%v", out, runErr)
	if err := os.WriteFile(filepath.Join(root, "ffmpeg.stderr"), out, 0600); err != nil {
		t.Fatal(err)
	}
	evidence := map[string]any{"started_utc": start.UTC(), "rtp": packets, "receiver_socket": sockets, "opus": input, "mixer": manager.Status(bridge.StreamID, time.Now()), "opus440_sha256": fmt.Sprintf("%x", sha256.Sum256(tone440OpusFrame)), "opus880_sha256": fmt.Sprintf("%x", sha256.Sum256(tone880OpusFrame)), "observer": "whole-datagram loopback test proxy; real mixer; synthetic producer"}
	raw, _ := json.MarshalIndent(evidence, "", "  ")
	if err := os.WriteFile(filepath.Join(root, "input-rtp.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("INPUT_RTP_ORIGINAL %s", raw)
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	for i := 1; i < len(packets); i++ {
		if packets[i].Seq != packets[i-1].Seq+1 || packets[i].TS != packets[i-1].TS+960 {
			t.Errorf("source RTP discontinuity at %d", i)
		}
	}
	if len(packets) < 600 {
		t.Errorf("insufficient real clock frames: %d", len(packets))
	}
	for _, path := range []string{mkv, live, hls} {
		timelineOutput(t, path)
	}
}

func timelineOutput(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := []string{"-v", "error", "-select_streams", "a:0", "-show_packets", "-show_streams", "-of", "json", path}
	out, err := exec.CommandContext(ctx, "ffprobe", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v %s", err, out)
	}
	if err := os.WriteFile(path+".packets.json", out, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("OUTPUT_PACKETS path=%s\n%s", path, out)
	var p struct {
		Streams []struct {
			TimeBase string `json:"time_base"`
		} `json:"streams"`
		Packets []struct {
			PTS      int64 `json:"pts"`
			Duration int64 `json:"duration"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Streams) != 1 || len(p.Packets) < 2 {
		t.Fatal("missing audio stream/packets")
	}
	var num, den float64
	if _, err := fmt.Sscanf(p.Streams[0].TimeBase, "%f/%f", &num, &den); err != nil {
		t.Fatal(err)
	}
	tick := num / den
	gaps := 0
	maxGap := 0.0
	for i := 1; i < len(p.Packets); i++ {
		d := float64(p.Packets[i].PTS-p.Packets[i-1].PTS) * tick
		gap := d - 1024.0/48000
		if gap > tick+1e-9 {
			gaps++
			maxGap = math.Max(maxGap, gap)
		}
	}
	pcm, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-xerror", "-i", path, "-map", "0:a:0", "-c:a", "pcm_s16le", "-f", "s16le", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".decoded.s16le", pcm, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("AUDIO_RESULT path=%s time_base=%s packets=%d decoded_samples_per_channel=%d gaps_beyond_sample_step_and_mux_rounding=%d max_excess_seconds=%s", path, p.Streams[0].TimeBase, len(p.Packets), len(pcm)/4, gaps, strconv.FormatFloat(maxGap, 'f', 9, 64))
	if gaps > 0 {
		t.Errorf("audio RTP-to-AAC timestamps have %d gaps exceeding one mux tick (sample step 1024/48000, not an operational SLA)", gaps)
	}
}

func timelineJPEG(t *testing.T, ctx context.Context) string {
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
