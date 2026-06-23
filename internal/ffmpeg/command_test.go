package ffmpeg

import (
	"context"
	"net"
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
	if !containsArg(args, "-filter:a") || !strings.Contains(joined, "astats=metadata=1") || !strings.Contains(joined, "audio-stats.txt") {
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
	for _, want := range []string{"lavfi", "color=c=black:s=1920x1080:r=60", "protocol_whitelist", "discord-opus.sdp", "1:a:0", "yuv420p", "astats=metadata=1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in args: %#v", want, args)
		}
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

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
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
