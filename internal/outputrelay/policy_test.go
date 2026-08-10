package outputrelay

import (
	"errors"
	"testing"
)

func TestPolicyConfigurationAndCanonicalCapabilityModes(t *testing.T) {
	for _, tt := range []struct {
		name           string
		url            string
		mode           string
		binding        string
		requireRelay   bool
		composeRelay   bool
		wantMode       string
		wantCapability string
		wantAdvertised bool
		wantErr        error
	}{
		{name: "url absent is direct", wantMode: ModeDirect, wantCapability: ModeDirect, wantAdvertised: true},
		{name: "relay required without URL is not direct", requireRelay: true, wantMode: ModeDirect, wantErr: ErrRelayRequired},
		{name: "url only preserves legacy stream key relay", url: "rtmp://127.0.0.1/autostream/{stream_id}", wantMode: ModeLegacyStreamKey, wantCapability: ModeLegacyStreamKey, wantAdvertised: true},
		{name: "legacy may be explicit", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: ModeLegacyStreamKey, wantMode: ModeLegacyStreamKey, wantCapability: ModeLegacyStreamKey, wantAdvertised: true},
		{name: "static requires binding", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: ModeLiveAPIStatic, wantMode: ModeLiveAPIStatic, wantErr: ErrStaticBindingRequired},
		{name: "static rejects generic binding", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: ModeLiveAPIStatic, binding: "relay-binding-static", wantMode: ModeLiveAPIStatic, wantErr: ErrInvalidRelayBindingID},
		{name: "static rejects a raw stream key as binding", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: ModeLiveAPIStatic, binding: "youtube-stream-key-secret", wantMode: ModeLiveAPIStatic, wantErr: ErrInvalidRelayBindingID},
		{name: "static rejects uppercase UUID binding", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: ModeLiveAPIStatic, binding: "relay-AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA", wantMode: ModeLiveAPIStatic, wantErr: ErrInvalidRelayBindingID},
		{name: "static rejects surrounding binding whitespace", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: ModeLiveAPIStatic, binding: " relay-11111111-1111-1111-1111-111111111111", wantMode: ModeLiveAPIStatic, wantErr: ErrInvalidRelayBindingID},
		{name: "static has canonical capability", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: ModeLiveAPIStatic, binding: "relay-11111111-1111-1111-1111-111111111111", wantMode: ModeLiveAPIStatic, wantCapability: ModeLiveAPIStatic, wantAdvertised: true},
		{name: "non-loopback relay is not advertised", url: "rtmp://relay.example.com/autostream/{stream_id}", wantMode: ModeLegacyStreamKey, wantErr: ErrUnsafeRelayTarget},
		{name: "compose relay without explicit identity is not advertised", url: "rtmp://output-relay:1935/autostream/{stream_id}", wantMode: ModeLegacyStreamKey, wantErr: ErrUnsafeRelayTarget},
		{name: "compose relay with explicit identity is advertised", url: "rtmp://output-relay:1935/autostream/{stream_id}", composeRelay: true, wantMode: ModeLegacyStreamKey, wantCapability: ModeLegacyStreamKey, wantAdvertised: true},
		{name: "url and direct mode conflict", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: ModeDirect, wantMode: ModeDirect, wantErr: ErrInvalidConfiguration},
		{name: "unknown mode is not advertised", url: "rtmp://127.0.0.1/autostream/{stream_id}", mode: "managed", wantMode: "managed", wantErr: ErrInvalidConfiguration},
		{name: "url-free static is invalid", mode: ModeLiveAPIStatic, wantMode: ModeDirect, wantErr: ErrInvalidConfiguration},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.composeRelay {
				t.Setenv("AUTOSTREAM_COMPOSE_OUTPUT_RELAY", "1")
			} else {
				t.Setenv("AUTOSTREAM_COMPOSE_OUTPUT_RELAY", "")
			}
			policy := NewWithRequireRelay(tt.url, tt.mode, tt.binding, tt.requireRelay)
			if policy.Mode != tt.wantMode {
				t.Fatalf("mode=%q want %q", policy.Mode, tt.wantMode)
			}
			if got, advertised := policy.CapabilityMode(); got != tt.wantCapability || advertised != tt.wantAdvertised {
				t.Fatalf("capability mode=%q advertised=%t want mode=%q advertised=%t", got, advertised, tt.wantCapability, tt.wantAdvertised)
			}
			if err := policy.ValidateConfiguration(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("configuration error=%v want %v", err, tt.wantErr)
			}
		})
	}
}

func TestPolicyAuthorizesOnlySupportedOutputBeforeSecretResolution(t *testing.T) {
	legacy := New("rtmp://127.0.0.1/autostream/{stream_id}", "", "stale-binding")
	usesLocalRelay, err := legacy.AuthorizeYouTubeOutput("stream_key", true, "")
	if err != nil || !usesLocalRelay {
		t.Fatalf("legacy stream_key authorization local=%t err=%v", usesLocalRelay, err)
	}
	for _, mode := range []string{"live_api", "live_api_dry_run", "", "live_api_relay_static"} {
		usesLocalRelay, err = legacy.AuthorizeYouTubeOutput(mode, true, "relay-binding-static")
		if !errors.Is(err, ErrLiveAPIRequiresManagedOutputRelay) || usesLocalRelay {
			t.Fatalf("legacy mode %q local=%t err=%v", mode, usesLocalRelay, err)
		}
	}

	staticBinding := "relay-11111111-1111-1111-1111-111111111111"
	static := New("rtmp://127.0.0.1/autostream/{stream_id}", ModeLiveAPIStatic, staticBinding)
	usesLocalRelay, err = static.AuthorizeYouTubeOutput("live_api_relay_static", true, staticBinding)
	if err != nil || !usesLocalRelay {
		t.Fatalf("static authorization local=%t err=%v", usesLocalRelay, err)
	}
	if _, err := static.AuthorizeYouTubeOutput("live_api_relay_static", false, staticBinding); !errors.Is(err, ErrLiveAPIRelayStaticNotReady) {
		t.Fatalf("static unready error=%v", err)
	}
	if _, err := static.AuthorizeYouTubeOutput("live_api_relay_static", true, "other-binding"); !errors.Is(err, ErrLiveAPIRelayBindingMismatch) {
		t.Fatalf("static binding error=%v", err)
	}
	if _, err := static.AuthorizeYouTubeOutput("live_api_relay_static", true, " "+staticBinding); !errors.Is(err, ErrLiveAPIRelayBindingMismatch) {
		t.Fatalf("static whitespace binding error=%v", err)
	}
	if _, err := static.AuthorizeYouTubeOutput("stream_key", true, "relay-binding-static"); !errors.Is(err, ErrLiveAPIRequiresManagedOutputRelay) {
		t.Fatalf("static stream-key error=%v", err)
	}

	direct := New("", "", "stale-binding")
	usesLocalRelay, err = direct.AuthorizeYouTubeOutput("live_api", false, "")
	if err != nil || usesLocalRelay {
		t.Fatalf("direct authorization local=%t err=%v", usesLocalRelay, err)
	}
}

func TestRequireRelayFromEnvDefaultsToDirectAndHonorsExplicitOverride(t *testing.T) {
	t.Setenv("AUTOSTREAM_ENV", "production")

	t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "")
	if RequireRelayFromEnv() {
		t.Fatal("production must allow direct output when no explicit Relay requirement is configured")
	}

	t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "false")
	if RequireRelayFromEnv() {
		t.Fatal("explicit false must allow a direct production output configuration")
	}

	t.Setenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY", "true")
	if !RequireRelayFromEnv() {
		t.Fatal("explicit true must require a Relay")
	}
}
