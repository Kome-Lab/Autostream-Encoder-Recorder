package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

// This matrix fixes the public composition boundary before the domain handlers
// are extracted. Domain tests separately exercise accepted requests and effects.
func TestRouteCompositionPreservesMethodsAuthenticationAndSecurityHeaders(t *testing.T) {
	root := t.TempDir()
	// Pending identity keeps this routing observation independent of platform
	// credential permissions; identity binding has its own transition tests.
	t.Setenv("AUTOSTREAM_NODE_CONFIG", filepath.Join(root, "pending-config.yml"))
	providerCalls, resolverCalls := 0, 0
	handler := NewServerWithManagersAndRuntimeConfig(
		"encoder_recorder",
		&streamproc.Manager{ArchiveRoot: root, OutputRelayMode: outputrelay.ModeDirect},
		workerevents.NewManager(root),
		TokenVerifier{PlainToken: "route-test-token", RequireSignedIngest: true},
		func(context.Context, string, string, string) (string, error) {
			resolverCalls++
			return "", nil
		},
		func(context.Context) (control.RuntimeConfig, error) {
			providerCalls++
			return control.RuntimeConfig{}, nil
		},
	)
	cases := []struct {
		method, path, body, code, cache string
		status                          int
	}{
		{"GET", "/health", "", "", "", 200},
		{"GET", "/status", "", "", "", 200},
		{"GET", "/updater/version", "", "updater_identity_pending", "no-store", 503},
		{"GET", "/preflight", "", "missing_or_invalid_service_token", "", 401},
		{"POST", "/heartbeat", "{}", "missing_or_invalid_service_token", "", 401},
		{"POST", "/streams/dry-run", "{}", "missing_or_invalid_service_token", "", 401},
		{"POST", "/streams/start", "{}", "missing_or_invalid_service_token", "", 401},
		{"PUT", "/streams/stream-01/runtime-settings", "{}", "missing_or_invalid_service_token", "", 401},
		{"GET", "/streams/stream-01/video-cover-state", "", "missing_or_invalid_service_token", "no-store", 401},
		{"PUT", "/streams/stream-01/video-cover-state", "{}", "missing_or_invalid_service_token", "no-store", 401},
		{"POST", "/streams/stream-01/stop", "{}", "missing_or_invalid_service_token", "", 401},
		{"GET", "/streams/stream-01/process-status", "", "missing_or_invalid_service_token", "", 401},
		{"GET", "/streams/stream-01/preview/index.m3u8", "", "missing_or_invalid_service_token", "", 401},
		{"GET", "/streams/stream-01/audio-status", "", "missing_or_invalid_service_token", "", 401},
		{"POST", "/streams/package", "{}", "missing_or_invalid_service_token", "", 401},
		{"GET", "/streams/stream-01/archive-runs/run-01/artifacts/final.mp4", "", "missing_or_invalid_service_token", "", 401},
		{"DELETE", "/streams/stream-01/archive-runs/run-01/artifacts/final.mp4", "", "missing_or_invalid_service_token", "", 401},
		{"PUT", "/streams/stream-01/archive-runs/run-01/artifacts/final.mp4", "{}", "missing_or_invalid_service_token", "", 401},
		{"POST", "/worker-events", "{}", "missing_or_invalid_worker_events_token", "", 401},
		{"GET", "/streams/stream-01/worker-events", "", "missing_or_invalid_service_token", "", 401},
		{"POST", "/streams/stream-01/audio/opus", "{}", "missing_or_invalid_discord_audio_token", "", 401},
		{"GET", "/streams/start", "", "", "", 405},
		{"GET", "/streams/stream-01/stop", "", "", "", 405},
		{"POST", "/streams/stream-01/video-cover-state", "{}", "", "", 405},
		{"GET", "/streams/stream-01/artifacts/final.mp4", "", "", "", 404},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, tc.status, response.Body.String())
			}
			if response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("X-Frame-Options") != "DENY" {
				t.Fatalf("security headers = %v", response.Header())
			}
			if got := response.Header().Get("Cache-Control"); got != tc.cache {
				t.Fatalf("Cache-Control = %q, want %q", got, tc.cache)
			}
			if tc.code != "" {
				var body struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Code != tc.code {
					t.Fatalf("code = %q, want %q; decode error = %v", body.Code, tc.code, err)
				}
			}
		})
	}
	if providerCalls != 0 || resolverCalls != 0 {
		t.Fatalf("unauthenticated routing reached runtime authority: provider = %d, resolver = %d", providerCalls, resolverCalls)
	}
}
