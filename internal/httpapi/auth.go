package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"os"
	"strings"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ingesttoken"
)

type TokenVerifier struct {
	PlainToken             string
	SHA256Hex              string
	UseNodeRuntimeToken    bool
	WorkerEventsPlainToken string
	WorkerEventsSHA256Hex  string
	DiscordAudioPlainToken string
	DiscordAudioSHA256Hex  string
	IngestTokenSigningKey  string
	RequireSignedIngest    bool
}

func TokenVerifierFromEnv() TokenVerifier {
	verifier := TokenVerifier{
		PlainToken:             os.Getenv("SERVICE_CONTROL_TOKEN"),
		SHA256Hex:              os.Getenv("SERVICE_CONTROL_TOKEN_SHA256"),
		WorkerEventsPlainToken: os.Getenv("ENCODER_WORKER_EVENTS_TOKEN"),
		WorkerEventsSHA256Hex:  os.Getenv("ENCODER_WORKER_EVENTS_TOKEN_SHA256"),
		DiscordAudioPlainToken: os.Getenv("ENCODER_DISCORD_AUDIO_TOKEN"),
		DiscordAudioSHA256Hex:  os.Getenv("ENCODER_DISCORD_AUDIO_TOKEN_SHA256"),
		IngestTokenSigningKey:  control.StreamIngestSigningKey(),
		RequireSignedIngest:    envBool("AUTOSTREAM_REQUIRE_SIGNED_INGEST_TOKENS", true),
	}
	verifier.UseNodeRuntimeToken = control.NodeConfigPathFromEnv() != ""
	if verifier.UseNodeRuntimeToken {
		// Read the signing key from config.yml for each verification so a
		// Panel-issued config rotation takes effect without a process restart.
		verifier.IngestTokenSigningKey = ""
	}
	return verifier
}

func (v TokenVerifier) Verify(header string) bool {
	if v.UseNodeRuntimeToken {
		return verifyBearerToken(header, control.NodeRuntimeTokenFromEnv(), "")
	}
	if verifyBearerToken(header, v.PlainToken, v.SHA256Hex) {
		return true
	}
	return false
}

func (v TokenVerifier) VerifyWorkerEvents(header, streamID string) bool {
	_, ok := v.WorkerEventsClaims(header, streamID)
	return ok
}

func (v TokenVerifier) VerifyDiscordAudio(header, streamID string) bool {
	_, ok := v.DiscordAudioClaims(header, streamID)
	return ok
}

func (v TokenVerifier) WorkerEventsClaims(header, streamID string) (ingesttoken.Claims, bool) {
	if claims, ok := v.verifySignedIngest(header, ingesttoken.Expected{StreamID: streamID, ServiceType: "worker", Purpose: "worker_events", Audience: "encoder_recorder"}); ok {
		return claims, true
	}
	if !v.RequireSignedIngest && tokenConfigured(v.WorkerEventsPlainToken, v.WorkerEventsSHA256Hex) && verifyBearerToken(header, v.WorkerEventsPlainToken, v.WorkerEventsSHA256Hex) {
		return ingesttoken.Claims{}, true
	}
	return ingesttoken.Claims{}, false
}

func (v TokenVerifier) DiscordAudioClaims(header, streamID string) (ingesttoken.Claims, bool) {
	if claims, ok := v.verifySignedIngest(header, ingesttoken.Expected{StreamID: streamID, ServiceType: "discord_bot", Purpose: "discord_audio", Audience: "encoder_recorder"}); ok {
		return claims, true
	}
	if !v.RequireSignedIngest && tokenConfigured(v.DiscordAudioPlainToken, v.DiscordAudioSHA256Hex) && verifyBearerToken(header, v.DiscordAudioPlainToken, v.DiscordAudioSHA256Hex) {
		return ingesttoken.Claims{}, true
	}
	return ingesttoken.Claims{}, false
}

func (v TokenVerifier) WorkerVideoClaims(token, streamID string) (ingesttoken.Claims, bool) {
	return v.verifySignedIngestToken(strings.TrimSpace(token), ingesttoken.Expected{StreamID: streamID, ServiceType: "worker", Purpose: "worker_video", Audience: "encoder_recorder"})
}

func (v TokenVerifier) verifySignedIngest(header string, expected ingesttoken.Expected) (ingesttoken.Claims, bool) {
	return v.verifySignedIngestToken(bearerToken(header), expected)
}

func (v TokenVerifier) verifySignedIngestToken(token string, expected ingesttoken.Expected) (ingesttoken.Claims, bool) {
	signingKey := strings.TrimSpace(v.IngestTokenSigningKey)
	if signingKey == "" {
		signingKey = control.StreamIngestSigningKey()
	}
	if token == "" || !ingesttoken.IsSigned(token) || signingKey == "" {
		return ingesttoken.Claims{}, false
	}
	claims, err := ingesttoken.Verify(signingKey, token, expected)
	return claims, err == nil
}

func tokenConfigured(plain, sha256Hex string) bool {
	return strings.TrimSpace(plain) != "" || strings.TrimSpace(sha256Hex) != ""
}

func envBool(key string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if raw == "" {
		return fallback
	}
	return raw == "1" || raw == "true" || raw == "yes" || raw == "on"
}

func verifyBearerToken(header, plainToken, sha256Hex string) bool {
	token := bearerToken(header)
	if token == "" {
		return false
	}
	if sha256Hex != "" {
		sum := sha256.Sum256([]byte(token))
		got := hex.EncodeToString(sum[:])
		return subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(sha256Hex))) == 1
	}
	if plainToken != "" {
		return subtle.ConstantTimeCompare([]byte(token), []byte(plainToken)) == 1
	}
	return false
}

func bearerToken(header string) string {
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
}

func requireServiceToken(w http.ResponseWriter, r *http.Request, verifier TokenVerifier) bool {
	if verifier.Verify(r.Header.Get("Authorization")) {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "missing_or_invalid_service_token"})
	return false
}
