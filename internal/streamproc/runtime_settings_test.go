package streamproc

import (
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
)

func TestManagerUpdatesAudioGainAndWatermarkWithoutRestart(t *testing.T) {
	starter := &fakeStarter{}
	reporter := &fakeReporter{}
	manager := &Manager{ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: starter, InputResolver: testInputResolver, AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}", OutputRelayMode: outputrelay.ModeManagedLiveAPI, OutputRelayBindingID: staticRelayBindingID, Reporter: reporter, WatermarkWitness: &watermarkTestWitness{}}
	before, err := manager.Start(lifecycle.StreamJob{StreamID: "stream-runtime", Name: "Runtime", InputURL: "rtsp://input.example.com/live", YouTubeOutputMode: "live_api_relay_static", OutputRelayBindingID: staticRelayBindingID, YouTubeOutputReady: true})
	if err != nil {
		t.Fatal(err)
	}
	after, err := manager.UpdateRuntimeSettings("stream-runtime", RuntimeSettings{EncoderAudioGainDB: 6.5, OverlayProfileID: "overlay-02", OverlayConfig: map[string]any{"watermark_enabled": false}})
	if err != nil {
		t.Fatal(err)
	}
	if before.PID != after.PID || after.EncoderAudioGainDB != 6.5 || after.OverlayProfileID != "overlay-02" {
		t.Fatalf("runtime settings restarted or were not reflected: before=%#v after=%#v", before, after)
	}
	if got := starter.process.commands; len(got) != 1 || got[0] != "volume@gain volume 6.5dB" {
		t.Fatalf("FFmpeg runtime command=%#v", got)
	}
	signal, ok := reporter.find("encoder.runtime_settings.dispatched")
	if !ok || signal.Status != "dispatched" || signal.Attributes["audio_gain_db"] != 6.5 || signal.Attributes["overlay_profile_id"] != "overlay-02" || signal.Attributes["audio_command_written"] != true || signal.Attributes["watermark_frame_updated"] != true {
		t.Fatalf("runtime settings dispatch diagnostic=%#v, found=%v", signal, ok)
	}
	if _, leaked := signal.Attributes["overlay_config"]; leaked {
		t.Fatalf("raw overlay configuration leaked into diagnostic: %#v", signal.Attributes)
	}
}
