package httpapi

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"

	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/audioingest"
	"github.com/example/autostream-encoder-recorder/internal/lifecycleparticipant"
)

const LifecycleMetadataHeader = "X-Autostream-Lifecycle"
const MaxLifecycleMetadataHeaderBytes = 32768

// NewV3AudioIngressHandler is explicit DI for isolated integration. It does not
// install a synthetic participant in ordinary startup or adopt a production wire.
func NewV3AudioIngressHandler(manager *audioingest.Manager, participant *lifecycleparticipant.Participant, verifier TokenVerifier, maxBodyBytes int64) http.Handler {
	mux := http.NewServeMux()
	registerV3AudioIngressRoutes(mux, manager, participant, verifier, maxBodyBytes)
	return securityHeaders(mux)
}
func registerV3AudioIngressRoutes(mux *http.ServeMux, manager *audioingest.Manager, participant *lifecycleparticipant.Participant, verifier TokenVerifier, maxBodyBytes int64) {
	mux.HandleFunc("POST /v3/streams/{id}/audio/opus", v3AudioIngress(manager, participant, verifier, maxBodyBytes, false))
	mux.HandleFunc("POST /v3/streams/{id}/audio/opus/inspect", v3AudioIngress(manager, participant, verifier, maxBodyBytes, true))
}
func v3AudioIngress(manager *audioingest.Manager, participant *lifecycleparticipant.Participant, verifier TokenVerifier, maxBodyBytes int64, inspect bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result := func(kind lifecycle.ResultKind, code string, status int) {
			writeJSON(w, status, lifecycle.Result{Kind: kind, Issues: []lifecycle.Issue{{Code: code, Path: "V3AudioIngress"}}})
		}
		if manager == nil || participant == nil || maxBodyBytes <= 0 {
			result(lifecycle.NeedsEvidence, "QUALIFIED_PARTICIPANT_NOT_CONFIGURED", http.StatusServiceUnavailable)
			return
		}
		serviceID, ok := v3AudioClaims(verifier, r.Header.Get("Authorization"), r.PathValue("id"))
		if !ok {
			result(lifecycle.RefusedUnchanged, "SERVICE_AUTH_REQUIRED", http.StatusUnauthorized)
			return
		}
		snapshot := participant.Snapshot()
		if serviceID != "" && serviceID != string(snapshot.IngressProducer.ServiceID) {
			result(lifecycle.Conflict, "INGRESS_SERVICE_MISMATCH", http.StatusUnauthorized)
			return
		}
		values := r.Header.Values(LifecycleMetadataHeader)
		if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > MaxLifecycleMetadataHeaderBytes {
			result(lifecycle.RefusedUnchanged, "BOUNDED_SHARED_METADATA_REQUIRED", http.StatusBadRequest)
			return
		}
		raw, err := base64.RawURLEncoding.Strict().DecodeString(values[0])
		if err != nil {
			result(lifecycle.RefusedUnchanged, "INVALID_SHARED_METADATA_ENCODING", http.StatusBadRequest)
			return
		}
		var record lifecycle.Record
		var check lifecycle.Result
		if inspect {
			// Metadata-only lookup: never decode, forward or append raw packet bytes.
			var probe []byte
			var probeErr error
			if r.Body != nil {
				probe, probeErr = io.ReadAll(io.LimitReader(r.Body, 1))
			}
			if r.ContentLength > 0 || probeErr != nil || len(probe) > 0 {
				result(lifecycle.RefusedUnchanged, "INSPECT_BODY_FORBIDDEN", http.StatusBadRequest)
				return
			}
			record, check = participant.Inspect(r.PathValue("id"), raw)
		} else {
			var request audioingest.IngestRequest
			if status, err := decodeLimitedStrictJSON(w, r, maxBodyBytes, &request); err != nil {
				result(lifecycle.RefusedUnchanged, "BOUNDED_PACKET_BODY_REQUIRED", status)
				return
			}
			if request.StreamID != r.PathValue("id") {
				result(lifecycle.Conflict, "STREAM_ID_MISMATCH", http.StatusBadRequest)
				return
			}
			record, check = manager.AddV3(r.Context(), participant, raw, request)
		}
		if record.Inspection != nil {
			writeJSON(w, http.StatusAccepted, record)
			return
		}
		status := http.StatusBadRequest
		switch check.Kind {
		case lifecycle.Stale, lifecycle.Conflict:
			status = http.StatusConflict
		case lifecycle.Unknown, lifecycle.NeedsEvidence, lifecycle.NeedsDecision:
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, check)
	}
}

// The opt-in v3 route uses explicit verifier DI only. It must not discover a
// runtime node credential or signing key through a v2 environment fallback.
func v3AudioClaims(v TokenVerifier, authorization, stream string) (string, bool) {
	if v.UseNodeRuntimeToken {
		return "", false
	}
	if strings.TrimSpace(v.IngestTokenSigningKey) != "" {
		claims, ok := v.DiscordAudioClaims(authorization, stream)
		return claims.ServiceID, ok
	}
	if v.RequireSignedIngest {
		return "", false
	}
	return "", verifyBearerToken(authorization, v.DiscordAudioPlainToken, v.DiscordAudioSHA256Hex)
}
