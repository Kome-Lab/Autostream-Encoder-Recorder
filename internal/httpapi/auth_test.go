package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/ingesttoken"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

func TestControlEndpointsFailClosedWhenTokenIsNotConfigured(t *testing.T) {
	handler := NewServer("encoder_recorder")
	req := httptest.NewRequest(http.MethodPost, "/streams/dry-run", bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer anything")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when token is not configured, got %d body = %s", res.Code, res.Body.String())
	}
}

func TestControlEndpointsRejectInvalidToken(t *testing.T) {
	handler := NewServerWithManagers("encoder_recorder", &streamproc.Manager{OutputRelayMode: outputrelay.ModeDirect}, workerevents.NewManager(t.TempDir()), TokenVerifier{PlainToken: "expected-token"})
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "heartbeat", method: http.MethodPost, path: "/heartbeat", body: `{}`},
		{name: "preflight", method: http.MethodGet, path: "/preflight"},
		{name: "dry run", method: http.MethodPost, path: "/streams/dry-run", body: `{}`},
		{name: "start", method: http.MethodPost, path: "/streams/start", body: `{}`},
		{name: "stop", method: http.MethodPost, path: "/streams/stream-01/stop", body: `{}`},
		{name: "status", method: http.MethodGet, path: "/streams/stream-01/process-status"},
		{name: "audio status", method: http.MethodGet, path: "/streams/stream-01/audio-status"},
		{name: "package", method: http.MethodPost, path: "/streams/package", body: `{}`},
		{name: "audio opus", method: http.MethodPost, path: "/streams/stream-01/audio/opus", body: `{}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			req.Header.Set("Authorization", "Bearer wrong-token")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d body = %s", res.Code, res.Body.String())
			}
		})
	}
}

func TestStreamControlEndpointsRejectOversizedJSONBodies(t *testing.T) {
	t.Setenv("SERVICE_CONTROL_TOKEN", "service-token")
	handler := NewServerWithManagers("encoder_recorder", &streamproc.Manager{OutputRelayMode: outputrelay.ModeDirect}, workerevents.NewManager(t.TempDir()), TokenVerifier{PlainToken: "service-token"})
	tests := []string{"/streams/dry-run", "/streams/start", "/streams/package"}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			body := `{}` + strings.Repeat(" ", maxControlBodyBytes)
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer service-token")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("expected oversized request to be rejected with 413, got %d body = %s", res.Code, res.Body.String())
			}
			if !strings.Contains(res.Body.String(), `"code":"request_body_too_large"`) {
				t.Fatalf("unexpected oversized request response: %s", res.Body.String())
			}
		})
	}
}

func TestRequireSignedIngestTokensRejectsStaticFallback(t *testing.T) {
	signingKey := "test-stream-ingest-signing-key"
	verifier := TokenVerifier{
		WorkerEventsPlainToken: "static-worker-token",
		DiscordAudioPlainToken: "static-audio-token",
		IngestTokenSigningKey:  signingKey,
		RequireSignedIngest:    true,
	}

	if verifier.VerifyWorkerEvents("Bearer static-worker-token", "stream-01") {
		t.Fatal("static worker ingest token must be rejected when signed ingest is required")
	}
	if verifier.VerifyDiscordAudio("Bearer static-audio-token", "stream-01") {
		t.Fatal("static discord audio token must be rejected when signed ingest is required")
	}

	workerToken, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-01",
		ServiceID:   "worker-01",
		ServiceType: "worker",
		Purpose:     "worker_events",
		Audience:    "encoder_recorder",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.VerifyWorkerEvents("Bearer "+workerToken, "stream-01") {
		t.Fatal("signed worker ingest token must be accepted when signed ingest is required")
	}

	audioToken, err := ingesttoken.Issue(signingKey, ingesttoken.Claims{
		StreamID:    "stream-01",
		ServiceID:   "discord-bot-01",
		ServiceType: "discord_bot",
		Purpose:     "discord_audio",
		Audience:    "encoder_recorder",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.VerifyDiscordAudio("Bearer "+audioToken, "stream-01") {
		t.Fatal("signed discord audio ingest token must be accepted when signed ingest is required")
	}
}

func TestTokenVerifierFromEnvRequiresSignedIngestByDefault(t *testing.T) {
	t.Setenv("ENCODER_WORKER_EVENTS_TOKEN", "static-worker-token")
	t.Setenv("ENCODER_DISCORD_AUDIO_TOKEN", "static-audio-token")
	verifier := TokenVerifierFromEnv()
	if !verifier.RequireSignedIngest {
		t.Fatal("signed ingest must be required by default")
	}
	if verifier.VerifyWorkerEvents("Bearer static-worker-token", "stream-01") {
		t.Fatal("static worker token must not be accepted by default")
	}
	if verifier.VerifyDiscordAudio("Bearer static-audio-token", "stream-01") {
		t.Fatal("static discord audio token must not be accepted by default")
	}
}

func TestTokenVerifierFromEnvAllowsStaticFallbackOnlyWhenExplicitlyDisabled(t *testing.T) {
	t.Setenv("AUTOSTREAM_REQUIRE_SIGNED_INGEST_TOKENS", "false")
	t.Setenv("ENCODER_WORKER_EVENTS_TOKEN", "static-worker-token")
	t.Setenv("ENCODER_DISCORD_AUDIO_TOKEN", "static-audio-token")
	verifier := TokenVerifierFromEnv()
	if verifier.RequireSignedIngest {
		t.Fatal("signed ingest should be disabled only when explicitly configured")
	}
	if !verifier.VerifyWorkerEvents("Bearer static-worker-token", "stream-01") {
		t.Fatal("static worker token should be accepted when fallback is explicitly enabled")
	}
	if !verifier.VerifyDiscordAudio("Bearer static-audio-token", "stream-01") {
		t.Fatal("static discord audio token should be accepted when fallback is explicitly enabled")
	}
}

func TestTokenVerifierReadsNodeRuntimeTokenAfterStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("AUTOSTREAM_NODE_CONFIG", path)
	verifier := TokenVerifierFromEnv()
	if !verifier.UseNodeRuntimeToken || verifier.PlainToken != "" || verifier.SHA256Hex != "" {
		t.Fatalf("expected config-backed runtime token verifier: %#v", verifier)
	}
	if verifier.Verify("Bearer runtime-secret") {
		t.Fatal("runtime token should not verify before config exists")
	}
	if check := serviceTokenPreflight(verifier); check.Status != "missing" || !strings.Contains(check.Message, "Auto Configure") {
		t.Fatalf("pending node config should be actionable in preflight: %#v", check)
	}
	writeNodeConfigForVerifierTest(t, path, "encoder_recorder")
	if !verifier.Verify("Bearer runtime-secret") {
		t.Fatal("runtime token should verify after config is written")
	}
	if check := serviceTokenPreflight(verifier); check.Status != "ok" || !strings.Contains(check.Message, "Node Runtime Token") || strings.Contains(check.Message, "SERVICE_CONTROL_TOKEN") {
		t.Fatalf("node runtime token should be identified accurately in preflight: %#v", check)
	}
	handler := NewServerWithManagers("encoder_recorder", nil, workerevents.NewManager(t.TempDir()), verifier)
	statusReq := httptest.NewRequest(http.MethodGet, "/status", nil)
	statusRes := httptest.NewRecorder()
	handler.ServeHTTP(statusRes, statusReq)
	var status Status
	if err := json.NewDecoder(statusRes.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if statusRes.Code != http.StatusOK || status.ServiceID != "encoder-recorder-01" {
		t.Fatalf("status should use node config identity: status=%d body=%s", statusRes.Code, statusRes.Body.String())
	}
	signedToken, err := ingesttoken.Issue("node-config-signing-key", ingesttoken.Claims{StreamID: "stream-01", ServiceID: "worker-01", ServiceType: "worker", Purpose: "worker_events", Audience: "encoder_recorder", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.VerifyWorkerEvents("Bearer "+signedToken, "stream-01") {
		t.Fatal("stream ingest signing key should be reloaded from node config")
	}
	writeNodeConfigForVerifierTestWithValues(t, path, "encoder_recorder", "rotated-runtime-secret", "rotated-node-config-signing-key")
	if verifier.Verify("Bearer runtime-secret") || !verifier.Verify("Bearer rotated-runtime-secret") {
		t.Fatal("Node Runtime Token rotation should take effect without accepting the previous token")
	}
	if verifier.VerifyWorkerEvents("Bearer "+signedToken, "stream-01") {
		t.Fatal("previous stream ingest signing key must stop working after config rotation")
	}
	rotatedSignedToken, err := ingesttoken.Issue("rotated-node-config-signing-key", ingesttoken.Claims{StreamID: "stream-01", ServiceID: "worker-01", ServiceType: "worker", Purpose: "worker_events", Audience: "encoder_recorder", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.VerifyWorkerEvents("Bearer "+rotatedSignedToken, "stream-01") {
		t.Fatal("rotated stream ingest signing key should be loaded from node config")
	}
}

func TestNodeConfigPathDisablesLegacyControlTokenEvenWhilePending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("AUTOSTREAM_NODE_CONFIG", path)
	t.Setenv("SERVICE_CONTROL_TOKEN", "legacy-control-token")
	verifier := TokenVerifierFromEnv()
	if !verifier.UseNodeRuntimeToken || verifier.Verify("Bearer legacy-control-token") {
		t.Fatal("configured node path must fail closed instead of accepting the legacy control token")
	}
	if check := serviceTokenPreflight(verifier); check.Status != "missing" || !strings.Contains(check.Message, "Auto Configure") {
		t.Fatalf("pending node config must not report the legacy token as ready: %#v", check)
	}
	writeNodeConfigForVerifierTest(t, path, "encoder_recorder")
	if verifier.Verify("Bearer legacy-control-token") {
		t.Fatal("legacy control token must be disabled after node config becomes available")
	}
	if !verifier.Verify("Bearer runtime-secret") {
		t.Fatal("Node Runtime Token should take precedence after Auto Configure")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if verifier.Verify("Bearer legacy-control-token") {
		t.Fatal("legacy control token must not reactivate when node config disappears")
	}
}
