package ffmpeg

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildRemuxArgsUsesArgumentArray(t *testing.T) {
	args := BuildRemuxArgs("/tmp/final.mkv", "/tmp/final.mp4")
	for _, arg := range args {
		if arg == "sh" || arg == "-c" && len(args) < 4 {
			t.Fatalf("unexpected shell-style args: %#v", args)
		}
	}
	if !strings.HasSuffix(args[len(args)-1], "final.mp4") {
		t.Fatalf("unexpected output: %#v", args)
	}
}

func TestDefaultProfile(t *testing.T) {
	p := DefaultProfile()
	if p.Width != 1920 || p.Height != 1080 || p.FPS != 60 || p.SampleRate != 48000 {
		t.Fatalf("bad default profile: %#v", p)
	}
}

func TestBuildLiveArchiveArgsWithProgress(t *testing.T) {
	args := BuildLiveArchiveArgsWithTelemetry("srt://input.example.com:9000", "rtmps://youtube.example.com/live2", "secret", "/tmp/final.mkv", "/tmp/progress.txt", "/tmp/audio-stats.txt", DefaultProfile())
	joined := strings.Join(args, " ")
	if !containsArg(args, "-progress") || !strings.Contains(joined, "progress.txt") {
		t.Fatalf("progress output missing: %#v", args)
	}
	if !containsArg(args, "-filter:a") || !strings.Contains(joined, "astats=metadata=1") || !strings.Contains(joined, "audio-stats.txt") || !strings.Contains(joined, "direct=1:enable='not(mod(n,50))'") {
		t.Fatalf("audio stats filter missing: %#v", args)
	}
	if strings.Contains(joined, " sh ") {
		t.Fatalf("unexpected shell-style args: %#v", args)
	}
}

func TestBuildLiveArchiveArgsToOutputTargetKeepsStreamKeyOutOfArgs(t *testing.T) {
	args := BuildLiveArchiveArgsToOutputTargetWithTelemetry("srt://input.example.com:9000", "rtmp://127.0.0.1/autostream/stream-01", "/tmp/final.mkv", "/tmp/progress.txt", "/tmp/audio-stats.txt", DefaultProfile())
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "secret-stream-key") || strings.Contains(joined, "youtube.example.com/live2") {
		t.Fatalf("unexpected upstream RTMPS secret material in relay args: %#v", args)
	}
	if !strings.Contains(joined, "rtmp://127.0.0.1/autostream/stream-01") {
		t.Fatalf("expected local relay output target in args: %#v", args)
	}
}

func TestBuildLiveArchiveArgsWithWatermarkCompositesImageBeforeTee(t *testing.T) {
	watermark := `C:\archives\tmp\.watermark-01.png`
	args := BuildLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark(
		"srt://input.example.com:9000",
		"rtmp://127.0.0.1/autostream/stream-01",
		`C:\archives\final.mkv`,
		`C:\archives\preview\index.m3u8`,
		"",
		"",
		watermark,
		DefaultProfile(),
	)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-loop 1", watermark, "-filter_complex", "[0:v]scale=1920:1080:force_original_aspect_ratio=decrease,pad=1920:1080", "[1:v]format=rgba,scale=1920:1080[wm]", "[base][wm]overlay=0:0", "[v]"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("watermark composition missing %q: %#v", want, args)
		}
	}
	if strings.Contains(joined, "data:image/") {
		t.Fatalf("raw watermark data URL leaked into FFmpeg args: %#v", args)
	}
}

func TestBuildDiscordAudioLiveArchiveArgsToOutputTargetKeepsStreamKeyOutOfArgs(t *testing.T) {
	args := BuildDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetry("/tmp/discord-opus.sdp", "rtmp://127.0.0.1/autostream/stream-01", "/tmp/final.mkv", "/tmp/progress.txt", "/tmp/audio-stats.txt", DefaultProfile())
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "secret-stream-key") || strings.Contains(joined, "youtube.example.com/live2") {
		t.Fatalf("unexpected upstream RTMPS secret material in relay args: %#v", args)
	}
	if !strings.Contains(joined, "rtmp://127.0.0.1/autostream/stream-01") {
		t.Fatalf("expected local relay output target in args: %#v", args)
	}
}

func TestBuildDiscordAudioLiveArchiveArgsWithProgress(t *testing.T) {
	args := BuildDiscordAudioLiveArchiveArgsWithTelemetry("/tmp/discord-opus.sdp", "rtmps://youtube.example.com/live2", "secret", "/tmp/final.mkv", "/tmp/progress.txt", "/tmp/audio-stats.txt", DefaultProfile())
	joined := strings.Join(args, " ")
	for _, want := range []string{"lavfi", "color=c=0x0b1020:s=1920x1080:r=60", "showwaves", "protocol_whitelist", "discord-opus.sdp", "-re -f lavfi -i anullsrc=channel_layout=stereo:sample_rate=48000", "amix=inputs=2:duration=longest", "[v]", "[aout]", "yuv420p", "astats=metadata=1", "direct=1:enable='not(mod(n,50))'"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in args: %#v", want, args)
		}
	}
	if !strings.Contains(joined, "eof_action=repeat:repeatlast=1:shortest=0") || strings.Contains(joined, "shortest=1") {
		t.Fatalf("Discord audio waveform must keep the background alive during silence: %#v", args)
	}
	if strings.Contains(joined, "-filter:a") || !strings.Contains(joined, "[aout]astats=metadata=1:reset=1,ametadata=print:file=/tmp/audio-stats.txt:direct=1:enable='not(mod(n,50))'[aout_stats]") || !strings.Contains(joined, "-map [aout_stats]") {
		t.Fatalf("Discord audio stats must stay inside the complex filtergraph: %#v", args)
	}
	if strings.Contains(joined, " sh ") {
		t.Fatalf("unexpected shell-style args: %#v", args)
	}
}

func TestBuildDiscordAudioLiveArchiveArgsResolvesInternalInputTarget(t *testing.T) {
	args := BuildDiscordAudioLiveArchiveArgsWithTelemetry("internal_discord_audio:C:/tmp/discord-opus.sdp", "rtmps://youtube.example.com/live2", "secret", "/tmp/final.mkv", "/tmp/progress.txt", "/tmp/audio-stats.txt", DefaultProfile())
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "internal_discord_audio") {
		t.Fatalf("internal input marker leaked into ffmpeg args: %#v", args)
	}
	if !strings.Contains(joined, "discord-opus.sdp") {
		t.Fatalf("resolved SDP input missing: %#v", args)
	}
}

func TestBuildWorkerVideoDiscordAudioArgsKeepsCredentialOutOfFFmpegAndAppliesWatermarkLast(t *testing.T) {
	args := BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark(
		"internal_worker_video:tcp://127.0.0.1:41001",
		"internal_discord_audio:C:/tmp/discord-opus.sdp",
		"rtmp://127.0.0.1/autostream/stream-01",
		"C:/tmp/final.mkv",
		"C:/tmp/preview/index.m3u8",
		"C:/tmp/progress.txt",
		"C:/tmp/audio-stats.txt",
		"C:/tmp/watermark.png",
		DefaultProfile(),
	)
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"internal_worker_video:", "passphrase", "worker-video-token"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("FFmpeg args expose internal credential material %q: %s", forbidden, joined)
		}
	}
	for _, required := range []string{
		"-f image2pipe -framerate 60 -c:v mjpeg -i tcp://127.0.0.1:41001",
		"-protocol_whitelist file,udp,rtp -i " + filepath.Clean("C:/tmp/discord-opus.sdp"),
		"[0:v]scale=1920:1080",
		"[3:v]format=rgba,scale=1920:1080[wm]",
		"[base][wm]overlay=0:0",
		"-map [v] -map [aout_stats]",
		"-minrate:v 8000k -maxrate:v 8000k -bufsize:v 16000k",
		"-f tee",
		"[aout]astats=metadata=1:reset=1,ametadata=print:file=C:/tmp/audio-stats.txt:direct=1:enable='not(mod(n,50))'[aout_stats]",
		"-map [aout_stats]",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %q in args: %s", required, joined)
		}
	}
	if strings.Contains(joined, "-filter:a") {
		t.Fatalf("Worker video audio stats must not use a simple filter with complex output: %s", joined)
	}
	if strings.Index(joined, "[0:v]scale=1920:1080") > strings.Index(joined, "[base][wm]overlay=0:0") {
		t.Fatalf("watermark must be applied after the Worker scene normalization: %s", joined)
	}
}

func TestBuildWorkerVideoRuntimeSettingsKeepStableNamedGainAndDynamicWatermarkInput(t *testing.T) {
	args := BuildWorkerVideoDiscordAudioLiveArchiveArgsToOutputTargetWithRuntimeSettings(
		"internal_worker_video:tcp://127.0.0.1:41001",
		"internal_discord_audio:C:/tmp/discord-opus.sdp",
		"rtmp://127.0.0.1/autostream/stream-01",
		"C:/tmp/final.mkv",
		"C:/tmp/preview/index.m3u8",
		"C:/tmp/progress.txt",
		"C:/tmp/audio-stats.txt",
		"tcp://127.0.0.1:42001",
		4.5,
		DefaultProfile(),
	)
	joined := strings.Join(args, " ")
	for _, required := range []string{
		"-f image2pipe -framerate 2 -c:v png -i tcp://127.0.0.1:42001",
		"volume@gain=4.5dB[aout]",
		"[base][wm]overlay=0:0",
		"-re -f lavfi -i anullsrc=channel_layout=stereo:sample_rate=48000",
		"direct=1:enable='not(mod(n,50))'",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %q in args: %s", required, joined)
		}
	}
}

func TestBuildLiveArchiveArgsWithPreviewKeepsStartToNowDVR(t *testing.T) {
	preview := `C:\Auto Stream\preview\index.m3u8`
	args := BuildLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(
		"srt://input.example.com:9000",
		"rtmp://127.0.0.1/autostream/stream-01",
		`C:\Auto Stream\final.mkv`,
		preview,
		"",
		"",
		DefaultProfile(),
	)
	teeOutput := args[len(args)-1]
	for _, want := range []string{
		"[f=flv:onfail=ignore]",
		"[f=matroska]",
		"f=hls",
		"onfail=ignore",
		"use_fifo=1",
		"fifo_options=",
		"queue_size=1200",
		"drop_pkts_on_overflow=1",
		"hls_time=2",
		"hls_list_size=0",
		"hls_flags=independent_segments+temp_file",
		"hls_segment_filename=",
		"segment-%06d.ts",
	} {
		if !strings.Contains(teeOutput, want) {
			t.Fatalf("missing %q in tee output: %s", want, teeOutput)
		}
	}
	if strings.Count(teeOutput, "onfail=ignore") != 2 {
		t.Fatalf("live and preview slaves must ignore isolated output failures: %s", teeOutput)
	}
	if strings.Contains(teeOutput, "omit_endlist") {
		t.Fatalf("preview must allow ENDLIST on graceful shutdown: %s", teeOutput)
	}
	assertBrowserCompatiblePreviewVideoArgs(t, args)
}

func TestBuildLiveArchiveArgsUsesConfiguredVideoBitrateAsCBR(t *testing.T) {
	args := BuildLiveArchiveArgsToOutputTargetWithTelemetry(
		"srt://input.example.com:9000",
		"rtmp://127.0.0.1/autostream/stream-01",
		"/tmp/final.mkv", "", "", EncoderProfile{Width: 1920, Height: 1080, FPS: 60, VideoBitrate: "6800k", AudioBitrate: "160k", SampleRate: 48000, KeyframeSec: 2},
	)
	assertArgValue(t, args, "-b:v", "6800k")
	assertArgValue(t, args, "-minrate:v", "6800k")
	assertArgValue(t, args, "-maxrate:v", "6800k")
	assertArgValue(t, args, "-bufsize:v", "13600k")
	assertArgValue(t, args, "-x264-params", "repeat-headers=1:open-gop=0:nal-hrd=cbr")
}

func TestBuildDiscordAudioLiveArchiveArgsWithPreviewAddsHLS(t *testing.T) {
	args := BuildDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreview(
		`C:\tmp\discord-opus.sdp`,
		"rtmp://127.0.0.1/autostream/stream-01",
		`C:\archives\final.mkv`,
		`C:\archives\preview\index.m3u8`,
		"",
		"",
		DefaultProfile(),
	)
	if teeOutput := args[len(args)-1]; !strings.Contains(teeOutput, "[f=hls:onfail=ignore:") {
		t.Fatalf("discord audio output is missing isolated HLS preview: %s", teeOutput)
	}
	assertBrowserCompatiblePreviewVideoArgs(t, args)
}

func TestBuildDiscordAudioLiveArchiveArgsWithWatermarkAddsFourthInputAfterSilence(t *testing.T) {
	args := BuildDiscordAudioLiveArchiveArgsToOutputTargetWithTelemetryAndPreviewAndWatermark(
		`C:\tmp\discord-opus.sdp`,
		"rtmp://127.0.0.1/autostream/stream-01",
		`C:\archives\final.mkv`,
		"",
		"",
		"",
		`C:\archives\tmp\.watermark-01.webp`,
		DefaultProfile(),
	)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-loop 1", ".watermark-01.webp", "anullsrc=channel_layout=stereo:sample_rate=48000", "[3:v]format=rgba,scale=1920:1080[wm]", "[base][wm]overlay=0:0", "[v]", "[aout]"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Discord watermark composition missing %q: %#v", want, args)
		}
	}
}

func assertBrowserCompatiblePreviewVideoArgs(t *testing.T, args []string) {
	t.Helper()
	for _, want := range [][2]string{
		{"-c:v", "libx264"},
		{"-pix_fmt", "yuv420p"},
		{"-g", "120"},
		{"-keyint_min", "120"},
		{"-sc_threshold", "0"},
		{"-x264-params", "repeat-headers=1:open-gop=0:nal-hrd=cbr"},
	} {
		assertArgValue(t, args, want[0], want[1])
	}
}

func TestTeeEscapingSurvivesBothParserLevels(t *testing.T) {
	value := `C:/Auto Stream/[preview]/O'Brien/segment-%06d.ts`
	encoded := escapeTeeOptionValue(value)
	if got := decodeBackslashEscapes(t, decodeBackslashEscapes(t, encoded)); got != value {
		t.Fatalf("second-level tee option escaping changed path: got %q want %q (encoded %q)", got, value, encoded)
	}
	if strings.Contains(encoded, "C:/") || strings.Contains(encoded, "O'Brien") {
		t.Fatalf("tee option special characters were not escaped at both levels: %q", encoded)
	}

	urlEncoded := escapeTeeSlaveURL(value)
	if got := decodeBackslashEscapes(t, urlEncoded); got != value {
		t.Fatalf("tee slave URL escaping changed path: got %q want %q (encoded %q)", got, value, urlEncoded)
	}
}

func TestRedactTeePathHandlesWindowsAndSecondLevelEscaping(t *testing.T) {
	path := `C:\Auto Stream\preview\segment-%06d.ts`
	argument := "prefix=" + escapeTeeOptionValue(filepath.ToSlash(filepath.Clean(path)))
	redacted := RedactTeePath(argument, path, "segment-%06d.ts")
	if strings.Contains(redacted, "Auto") || strings.Contains(redacted, "C:") {
		t.Fatalf("tee-escaped absolute path leaked after redaction: %q", redacted)
	}
	if !strings.Contains(redacted, "segment-%06d.ts") {
		t.Fatalf("logical preview name missing after redaction: %q", redacted)
	}
}

func TestRedactTeeValueHandlesEscapedQuote(t *testing.T) {
	sensitive := "stream-key-with-'quote"
	argument := "[f=flv]rtmps://youtube.example.com/live2/" + escapeTeeSlaveURL(sensitive)
	redacted := RedactTeeValue(argument, sensitive, "<REDACTED>")
	if strings.Contains(redacted, "stream-key") || strings.Contains(redacted, "quote") {
		t.Fatalf("tee-escaped sensitive value leaked after redaction: %q", redacted)
	}
	if !strings.Contains(redacted, "<REDACTED>") {
		t.Fatalf("redaction marker missing: %q", redacted)
	}
}

func decodeBackslashEscapes(t *testing.T, value string) string {
	t.Helper()
	var decoded strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			decoded.WriteByte(value[index])
			continue
		}
		index++
		if index >= len(value) {
			t.Fatalf("dangling escape in %q", value)
		}
		decoded.WriteByte(value[index])
	}
	return decoded.String()
}

func TestValidateInputTargetAllowsExpectedProtocols(t *testing.T) {
	tests := []string{
		"rtsp://camera.example.com/live",
		"rtmp://source.example.com/live",
		"rtmps://source.example.com/live",
		"srt://source.example.com:9000?mode=caller",
		"udp://239.1.1.1:1234",
		"rtp://239.1.1.1:5004",
		"https://cdn.example.com/live/index.m3u8",
		"http://cdn.example.com/live?format=m3u8",
		"internal_discord_audio:C:/tmp/discord-opus.sdp",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			if err := ValidateInputTarget(input); err != nil {
				t.Fatalf("expected input to be accepted: %v", err)
			}
		})
	}
}

func TestValidateInputTargetRejectsUnsafeTargets(t *testing.T) {
	tests := []string{
		"",
		"input-without-scheme",
		"file:///etc/passwd",
		"concat:http://example.com/a|http://example.com/b",
		"https://example.com/index.html",
		"https://user@cdn.example.com/live/index.m3u8",
		"https://cdn.example.com/live/index.m3u8?sig=secret-signature",
		"https://cdn.example.com/live?format=m3u8&sig=secret-signature",
		"rtsp://example.com/live#fragment",
		"internal_discord_audio://host/tmp/discord-opus.sdp",
		"internal_discord_audio:C:/tmp/discord-opus.txt",
		"rtsp://example.com/live\nrtmp://evil.example.com/live",
		"rtsp://127.0.0.1/live",
		"rtsp://10.0.0.5/live",
		"rtsp://172.16.0.10/live",
		"rtsp://192.168.1.10/live",
		"https://169.254.169.254/latest/meta-data?format=m3u8",
		"https://metadata.google.internal/live/index.m3u8",
		"https://localhost/live/index.m3u8",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			if err := ValidateInputTarget(input); err == nil {
				t.Fatalf("expected input to be rejected")
			}
		})
	}
}

func TestValidateInputTargetAllowsMulticastOnlyForUDPRTP(t *testing.T) {
	if err := ValidateInputTarget("udp://239.1.1.1:1234"); err != nil {
		t.Fatalf("expected UDP multicast to be accepted: %v", err)
	}
	if err := ValidateInputTarget("rtp://239.1.1.1:5004"); err != nil {
		t.Fatalf("expected RTP multicast to be accepted: %v", err)
	}
	if err := ValidateInputTarget("rtsp://239.1.1.1/live"); err == nil {
		t.Fatal("expected RTSP multicast host to be rejected")
	}
}

func TestValidateInputTargetWithAllowedHosts(t *testing.T) {
	if err := ValidateInputTargetWithAllowedHosts("rtsp://camera.example.com/live", []string{"camera.example.com"}); err != nil {
		t.Fatalf("expected exact allowed host to pass: %v", err)
	}
	if err := ValidateInputTargetWithAllowedHosts("rtsp://edge.video.example.com/live", []string{"*.video.example.com"}); err != nil {
		t.Fatalf("expected wildcard allowed host to pass: %v", err)
	}
	if err := ValidateInputTargetWithAllowedHosts("rtsp://camera.example.com/live", []string{"other.example.com"}); err == nil {
		t.Fatal("expected host outside allowlist to be rejected")
	}
	if err := ValidateInputTargetWithAllowedHosts("internal_discord_audio:C:/tmp/discord-opus.sdp", []string{"camera.example.com"}); err != nil {
		t.Fatalf("expected internal Discord audio to bypass network host allowlist: %v", err)
	}
}

func TestValidateInputTargetWithResolverRejectsDNSRebindTargets(t *testing.T) {
	tests := []struct {
		name string
		url  string
		ips  []net.IP
	}{
		{name: "loopback", url: "https://cdn.example.com/live/index.m3u8", ips: []net.IP{net.ParseIP("127.0.0.1")}},
		{name: "link local metadata", url: "https://cdn.example.com/live/index.m3u8", ips: []net.IP{net.ParseIP("169.254.169.254")}},
		{name: "private rtsp", url: "rtsp://camera.example.com/live", ips: []net.IP{net.ParseIP("10.0.0.5")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInputTargetWithResolver(context.Background(), tc.url, nil, resolverFor(tc.ips...))
			if err == nil {
				t.Fatal("expected resolved unsafe input to be rejected")
			}
		})
	}
}

func TestValidateInputTargetWithResolverAllowsPublicTargetsAndInternalAudio(t *testing.T) {
	if err := ValidateInputTargetWithResolver(context.Background(), "https://cdn.example.com/live/index.m3u8", []string{"cdn.example.com"}, resolverFor(net.ParseIP("93.184.216.34"))); err != nil {
		t.Fatalf("expected public resolved HLS target to pass: %v", err)
	}
	called := false
	resolver := func(ctx context.Context, host string) ([]net.IP, error) {
		called = true
		return nil, nil
	}
	if err := ValidateInputTargetWithResolver(context.Background(), "internal_discord_audio:C:/tmp/discord-opus.sdp", []string{"cdn.example.com"}, resolver); err != nil {
		t.Fatalf("expected internal audio target to bypass DNS: %v", err)
	}
	if called {
		t.Fatal("internal audio target must not call network resolver")
	}
}

func TestValidateInputTargetWithRuntimePolicyRejectsDirectHLSByDefault(t *testing.T) {
	err := ValidateInputTargetWithRuntimePolicy(context.Background(), "https://cdn.example.com/live/index.m3u8", []string{"cdn.example.com"}, resolverFor(net.ParseIP("93.184.216.34")), RuntimeInputPolicy{})
	if err == nil {
		t.Fatal("expected direct HLS runtime input to be rejected by default")
	}
}

func TestValidateInputTargetWithRuntimePolicyAllowsDirectHLSWhenExplicitlyEnabled(t *testing.T) {
	err := ValidateInputTargetWithRuntimePolicy(context.Background(), "https://cdn.example.com/live/index.m3u8", []string{"cdn.example.com"}, resolverFor(net.ParseIP("93.184.216.34")), RuntimeInputPolicy{AllowDirectHLS: true, AllowHostnameInputs: true})
	if err != nil {
		t.Fatalf("expected direct HLS runtime input to pass when explicitly enabled: %v", err)
	}
}

func TestValidateInputTargetWithRuntimePolicyRejectsHostnameByDefault(t *testing.T) {
	err := ValidateInputTargetWithRuntimePolicy(context.Background(), "srt://source.example.com:9000", []string{"source.example.com"}, resolverFor(net.ParseIP("93.184.216.34")), RuntimeInputPolicy{RequireAllowedHosts: true})
	if err == nil {
		t.Fatal("expected hostname input to be rejected without explicit opt-in")
	}
}

func TestValidateInputTargetWithRuntimePolicyRequiresAllowedHostsWhenConfigured(t *testing.T) {
	err := ValidateInputTargetWithRuntimePolicy(context.Background(), "srt://source.example.com:9000", nil, resolverFor(net.ParseIP("93.184.216.34")), RuntimeInputPolicy{RequireAllowedHosts: true, AllowHostnameInputs: true})
	if err == nil {
		t.Fatal("expected external runtime input to require an allowed host when policy is enabled")
	}
	err = ValidateInputTargetWithRuntimePolicy(context.Background(), "srt://source.example.com:9000", []string{"source.example.com"}, resolverFor(net.ParseIP("93.184.216.34")), RuntimeInputPolicy{RequireAllowedHosts: true, AllowHostnameInputs: true})
	if err != nil {
		t.Fatalf("expected allowlisted runtime input to pass: %v", err)
	}
}

func TestValidateInputTargetWithRuntimePolicyAllowsInternalAudioWithoutAllowedHosts(t *testing.T) {
	err := ValidateInputTargetWithRuntimePolicy(context.Background(), "internal_discord_audio:C:/tmp/discord-opus.sdp", nil, resolverFor(net.ParseIP("93.184.216.34")), RuntimeInputPolicy{RequireAllowedHosts: true})
	if err != nil {
		t.Fatalf("expected internal audio target to bypass host allowlist requirement: %v", err)
	}
}

func resolverFor(ips ...net.IP) HostResolver {
	return func(ctx context.Context, host string) ([]net.IP, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return ips, nil
	}
}

func TestValidateOutputTargetRejectsTeeMetacharacters(t *testing.T) {
	tests := []struct {
		name      string
		rtmpURL   string
		streamKey string
	}{
		{name: "url pipe", rtmpURL: "rtmps://youtube.example.com/live2|[f=matroska]/tmp/evil.mkv", streamKey: "secret"},
		{name: "url bracket", rtmpURL: "rtmps://youtube.example.com/live2[select=v]", streamKey: "secret"},
		{name: "key pipe", rtmpURL: "rtmps://youtube.example.com/live2", streamKey: "secret|[f=matroska]/tmp/evil.mkv"},
		{name: "key slash", rtmpURL: "rtmps://youtube.example.com/live2", streamKey: "secret/extra"},
		{name: "userinfo", rtmpURL: "rtmps://user:pass@youtube.example.com/live2", streamKey: "secret"},
		{name: "query", rtmpURL: "rtmps://youtube.example.com/live2?x=1", streamKey: "secret"},
		{name: "plain rtmp output", rtmpURL: "rtmp://youtube.example.com/live2", streamKey: "secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateOutputTarget(tc.rtmpURL, tc.streamKey); err == nil {
				t.Fatalf("expected target to be rejected")
			}
		})
	}
	if err := ValidateOutputTarget("rtmps://youtube.example.com/live2", "abcd-efgh-ijkl-mnop"); err != nil {
		t.Fatalf("expected normal RTMPS target to be accepted: %v", err)
	}
}

func TestValidateRelayOutputTargetAllowsOnlyLoopbackRTMP(t *testing.T) {
	for _, target := range []string{
		"rtmp://127.0.0.1/autostream/stream-01",
		"rtmp://localhost/autostream/stream-01",
	} {
		if err := ValidateRelayOutputTarget(target); err != nil {
			t.Fatalf("expected relay target %q to pass: %v", target, err)
		}
	}
	for _, target := range []string{
		"rtmps://youtube.example.com/live2/secret",
		"rtmp://10.0.0.2/autostream/stream-01",
		"rtmp://user:pass@127.0.0.1/autostream/stream-01",
		"rtmp://127.0.0.1/autostream/stream-01?token=secret",
		"rtmp://127.0.0.1/autostream/stream-01|[f=matroska]/tmp/evil.mkv",
		"tcp://127.0.0.1:9000",
		"rtmp://127.0.0.1/",
	} {
		if err := ValidateRelayOutputTarget(target); err == nil {
			t.Fatalf("expected relay target %q to be rejected", target)
		}
	}
}

func TestValidateRelayOutputTargetAllowsExplicitComposeOwnedRelayOnly(t *testing.T) {
	t.Setenv("AUTOSTREAM_COMPOSE_OUTPUT_RELAY", "1")
	if err := ValidateRelayOutputTarget("rtmp://output-relay:1935/autostream/stream-01"); err != nil {
		t.Fatalf("expected explicitly configured Compose relay target to pass: %v", err)
	}
	for _, target := range []string{
		"rtmp://output-relay/autostream/stream-01",
		"rtmps://output-relay:1935/autostream/stream-01",
		"rtmp://output-relay:1936/autostream/stream-01",
		"rtmp://output-relay.example.com:1935/autostream/stream-01",
		"rtmp://other-relay:1935/autostream/stream-01",
	} {
		if err := ValidateRelayOutputTarget(target); err == nil {
			t.Fatalf("expected non-Compose relay target %q to be rejected", target)
		}
	}
}

func TestValidateRelayOutputTargetRejectsComposeRelayWithoutExplicitIdentity(t *testing.T) {
	t.Setenv("AUTOSTREAM_COMPOSE_OUTPUT_RELAY", "")
	if err := ValidateRelayOutputTarget("rtmp://output-relay:1935/autostream/stream-01"); err == nil {
		t.Fatal("expected Compose relay hostname to require explicit Compose identity")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func assertArgValue(t *testing.T, args []string, flag, want string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == want {
			return
		}
	}
	t.Fatalf("missing %s %s in args: %#v", flag, want, args)
}

func TestParseProgress(t *testing.T) {
	progress := ParseProgress("frame=100\nfps=59.94\nbitrate=8123.4kbits/s\ndrop_frames=2\nprogress=continue\n")
	if progress.FPS != 59.94 || progress.BitrateKbps != 8123.4 || progress.DroppedFrames != 2 {
		t.Fatalf("unexpected progress: %#v", progress)
	}
}

func TestParseAudioStats(t *testing.T) {
	stats := ParseAudioStats("frame:1\nlavfi.astats.Overall.RMS_level=-54.25\nlavfi.astats.Overall.Peak_level=-0.5\n")
	if !stats.HasRMS || !stats.HasPeak || stats.RMSLevelDB != -54.25 || stats.PeakLevelDB != -0.5 {
		t.Fatalf("unexpected audio stats: %#v", stats)
	}
	silent := ParseAudioStats("lavfi.astats.Overall.RMS_level=-inf\n")
	if !silent.HasRMS || silent.RMSLevelDB != -120 {
		t.Fatalf("unexpected -inf handling: %#v", silent)
	}
}
