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
		if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
			t.Fatal("evidence directory is not empty; preserve it and choose a fresh evidence root")
		} else if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
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

	makeLayer := func(rgb [3]byte, watermark bool) []byte {
		im := image.NewNRGBA(image.Rect(0, 0, 160, 90))
		for y := 0; y < 90; y++ {
			for x := 0; x < 160; x++ {
				if watermark && (x >= 32 || y >= 24) {
					continue
				}
				i := y*im.Stride + x*4
				copy(im.Pix[i:], []byte{rgb[0], rgb[1], rgb[2], 255})
			}
		}
		var b bytes.Buffer
		if err := png.Encode(&b, im); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	cover, err := imagefeed.New("timeline cover", makeLayer([3]byte{220, 40, 20}, false))
	if err != nil {
		t.Fatal(err)
	}
	defer cover.Close()
	wm, err := imagefeed.New("timeline watermark", makeLayer([3]byte{255, 255, 255}, true))
	if err != nil {
		t.Fatal(err)
	}
	defer wm.Close()
	progressPath := filepath.Join(root, "progress.txt")
	p := ffmpeg.EncoderProfile{Width: 160, Height: 90, FPS: 10, VideoBitrate: "300k", AudioBitrate: "64k", SampleRate: 48000, KeyframeSec: 2}
	mkv, live, hls := filepath.Join(root, "final.mkv"), filepath.Join(root, "live.flv"), filepath.Join(root, "hls/index.m3u8")
	args := ffmpeg.BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettingsAndVisualLayers(video, sdp, live, mkv, hls, progressPath, "", cover.InputURL(), wm.InputURL(), 0, p)
	args = append(append(append([]string{}, args[:len(args)-3]...), "-t", "12"), args[len(args)-3:]...)
	args = append([]string{"-loglevel", "debug"}, args...)
	argv, _ := json.Marshal(args)
	t.Logf("PRODUCTION_ARGV %s", argv)
	timeout, stopCommand := context.WithTimeout(ctx, 35*time.Second)
	command := exec.CommandContext(timeout, "ffmpeg", args...)
	var captured bytes.Buffer
	command.Stdout = &captured
	command.Stderr = &captured
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	progressDeadline := time.Now().Add(6 * time.Second)
	ready := false
	for time.Now().Before(progressDeadline) {
		raw, _ := os.ReadFile(progressPath)
		if bytes.Contains(raw, []byte("progress=continue")) {
			for _, line := range bytes.Split(raw, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("frame=")) {
					n, _ := strconv.Atoi(string(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("frame=")))))
					if n >= 20 {
						ready = true
					}
				}
			}
		}
		if ready {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	switchErr := fmt.Errorf("production progress did not reach frame20 within six seconds")
	if ready {
		switchErr = cover.UpdateAndWait(timeout, makeLayer([3]byte{20, 180, 60}, false))
	}
	runErr := command.Wait()
	out := captured.Bytes()
	t.Logf("COVER_SWITCH pid=%d ready=%t result=%v", pid, ready, switchErr)
	if switchErr != nil {
		t.Error(switchErr)
	}
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
		switchPTS := timelineCoverFrames(t, path)
		timelineOutput(t, path, switchPTS)
	}
}

func timelineOutput(t *testing.T, path string, switchPTS float64) {
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
			TimeBase  string `json:"time_base"`
			StartTime string `json:"start_time"`
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
	pcm, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-xerror", "-i", path, "-map", "0:a:0", "-c:a", "pcm_s16le", "-ar", "48000", "-ac", "2", "-f", "s16le", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".decoded.s16le", pcm, 0600); err != nil {
		t.Fatal(err)
	}

	audioStart, _ := strconv.ParseFloat(p.Streams[0].StartTime, 64)
	switchOffset := switchPTS - audioStart
	mins := [3]float64{math.Inf(1), math.Inf(1), math.Inf(1)}
	counts := [3]int{}
	for at := 48000 * 4; at+960*4 < len(pcm)-48000*4; at += 960 * 4 {
		sum := 0.0
		for j := at; j < at+960*4; j += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(pcm[j : j+2])))
			sum += v * v
		}
		rms := math.Sqrt(sum / 1920)
		sec := float64(at/4) / 48000
		phase := 1
		if sec < switchOffset-0.5 {
			phase = 0
		} else if sec > switchOffset+0.5 {
			phase = 2
		}
		counts[phase]++
		mins[phase] = math.Min(mins[phase], rms)
		if math.IsNaN(rms) || math.IsInf(rms, 0) || rms <= 1 {
			t.Fatalf("silent output path=%s phase=%d seconds=%.3f RMS=%f", path, phase, sec, rms)
		}
	}
	for phase, count := range counts {
		if count == 0 {
			t.Fatalf("missing audio switch phase%d offset=%f", phase, switchOffset)
		}
	}
	if mins[1] < math.Min(mins[0], mins[2])*0.25 {
		t.Fatal("audio collapsed while switching cover")
	}
	t.Logf("AUDIO_SWITCH path=%s offset=%.3f minimum_RMS=%v windows=%v", path, switchOffset, mins, counts)
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

// Inspect every pixel of every decoded production tee frame. The independent
// watermark is part of both expected images, so missing layers also fail.
func timelineCoverFrames(t *testing.T, path string) float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	frames, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-xerror", "-i", path, "-map", "0:v:0", "-fps_mode", "passthrough", "-pix_fmt", "rgb24", "-f", "rawvideo", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	probe, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_frames", "-show_entries", "frame=best_effort_timestamp_time", "-of", "json", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var times struct {
		Frames []struct {
			PTS string `json:"best_effort_timestamp_time"`
		} `json:"frames"`
	}
	if err = json.Unmarshal(probe, &times); err != nil {
		t.Fatal(err)
	}
	size := 160 * 90 * 3
	if len(frames)%size != 0 || len(frames)/size != len(times.Frames) {
		t.Fatal("frame/PTS inventory mismatch")
	}
	counts := [2]int{}
	seenB := false
	firstB := 0.0
	matches := func(frame []byte, base [3]byte) bool {
		for y := 0; y < 90; y++ {
			for x := 0; x < 160; x++ {
				want := base
				if x < 32 && y < 24 {
					want = [3]byte{255, 255, 255}
				}
				for c := 0; c < 3; c++ {
					delta := int(frame[(y*160+x)*3+c]) - int(want[c])
					if delta < -32 || delta > 32 {
						return false
					}
				}
			}
		}
		return true
	}
	for at := 0; at < len(frames); at += size {
		frame := frames[at : at+size]
		if matches(frame, [3]byte{20, 180, 60}) {
			counts[1]++
			if !seenB {
				firstB, _ = strconv.ParseFloat(times.Frames[at/size].PTS, 64)
			}
			seenB = true
		} else if matches(frame, [3]byte{220, 40, 20}) && !seenB {
			counts[0]++
		} else {
			t.Fatalf("base exposure, mixed image or obsolete revision path=%s frame=%d SHA=%x", path, at/size, sha256.Sum256(frame))
		}
	}
	if counts[0] < 10 || counts[1] < 10 {
		t.Fatalf("insufficient A/B frames %v", counts)
	}
	t.Logf("COVER_ALL_FRAMES path=%s counts=%v first_B_PTS=%.6f", path, counts, firstB)
	return firstB
}
