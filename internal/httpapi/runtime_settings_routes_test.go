package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

func TestVideoCoverPUTRejectsUnknownFieldsAndDisablesStorage(t *testing.T) {
	handler := putVideoCoverState(&streamproc.Manager{OutputRelayMode: outputrelay.ModeDirect}, TokenVerifier{PlainToken: "cover-control-token"})
	body := `{"stream_id":"stream-1","job_generation":1,"expected_generation":1,"revision":2,"active":false,"idempotency_key":"hide-2","hide_confirmed":true,"secret_url":"https://must-not-be-accepted.invalid"}`
	request := httptest.NewRequest(http.MethodPut, "/streams/stream-1/video-cover-state", strings.NewReader(body))
	request.SetPathValue("id", "stream-1")
	request.Header.Set("Authorization", "Bearer cover-control-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), `"code":"bad_request"`) {
		t.Fatalf("response status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if strings.Contains(response.Body.String(), "must-not-be-accepted") {
		t.Fatalf("rejected payload leaked into response: %s", response.Body.String())
	}
}

func TestVideoCoverGraphAndCapabilityRejectionsUseBadGateway(t *testing.T) {
	for _, code := range []videocover.ErrorCode{videocover.ErrorCoverGraphUnavailable, videocover.ErrorCapabilityRequired} {
		if status := coverRejectionStatus(&videocover.SafeError{Code: code}); status != http.StatusBadGateway {
			t.Fatalf("code=%s status=%d want=%d", code, status, http.StatusBadGateway)
		}
	}
}

func TestVideoCoverHTTPAmbiguousHideRequiresGETAndExactReplayDoesNotResend(t *testing.T) {
	profile := ffmpeg.DefaultProfile()
	profile.Width, profile.Height, profile.FPS = 16, 9, 2
	witness := &httpCoverWitness{}
	manager := &streamproc.Manager{
		ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver,
		AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}",
		OutputRelayMode: outputrelay.ModeManagedLiveAPI, OutputRelayBindingID: testStaticRelayBindingID,
		Profile: profile, CoverAssets: videocover.NewLoader(nil, 1, 1<<20), CoverWitness: witness,
	}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "cover-http", Name: "Cover HTTP", InputURL: "srt://input.example.com:9000",
		YouTubeOutputMode: "live_api_relay_static", OutputRelayBindingID: testStaticRelayBindingID, YouTubeOutputReady: true,
		VideoCoverStart: &videocover.StartSnapshot{JobGeneration: 9, Revision: 1, IdempotencyKey: "start-inactive-9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = manager.Stop("cover-http") })
	witness.failNext = true
	body := `{"stream_id":"cover-http","job_generation":9,"expected_generation":1,"revision":2,"active":false,"idempotency_key":"hide-2","hide_confirmed":true}`
	handler := putVideoCoverState(manager, TokenVerifier{PlainToken: "cover-control-token"})
	request := httptest.NewRequest(http.MethodPut, "/streams/cover-http/video-cover-state", strings.NewReader(body))
	request.SetPathValue("id", "cover-http")
	request.Header.Set("Authorization", "Bearer cover-control-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var applied videocover.ApplyResponse
	if err := json.Unmarshal(response.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusAccepted || response.Header().Get("Cache-Control") != "no-store" || applied.Outcome != videocover.OutcomeAmbiguous || !applied.Actual.NoAutomaticResend || witness.calls != 2 {
		t.Fatalf("ambiguous status=%d headers=%v response=%#v witness=%d", response.Code, response.Header(), applied, witness.calls)
	}
	replayRequest := httptest.NewRequest(http.MethodPut, "/streams/cover-http/video-cover-state", strings.NewReader(body))
	replayRequest.SetPathValue("id", "cover-http")
	replayRequest.Header.Set("Authorization", "Bearer cover-control-token")
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replayRequest)
	if replayResponse.Code != http.StatusAccepted || witness.calls != 2 {
		t.Fatalf("exact replay resent cover: status=%d witness=%d body=%s", replayResponse.Code, witness.calls, replayResponse.Body.String())
	}
	getRequest := httptest.NewRequest(http.MethodGet, "/streams/cover-http/video-cover-state", nil)
	getRequest.SetPathValue("id", "cover-http")
	getRequest.Header.Set("Authorization", "Bearer cover-control-token")
	getResponse := httptest.NewRecorder()
	getVideoCoverState(manager, TokenVerifier{PlainToken: "cover-control-token"}).ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK || getResponse.Header().Get("Cache-Control") != "no-store" || !strings.Contains(getResponse.Body.String(), `"readiness":"unknown"`) {
		t.Fatalf("GET reconcile status=%d headers=%v body=%s", getResponse.Code, getResponse.Header(), getResponse.Body.String())
	}
}

func TestVideoCoverPUTStoppedNegotiatedRuntimeReturnsCanonicalGraphRejection(t *testing.T) {
	profile := ffmpeg.DefaultProfile()
	profile.Width, profile.Height, profile.FPS = 16, 9, 2
	manager := &streamproc.Manager{
		ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver,
		AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}",
		OutputRelayMode: outputrelay.ModeManagedLiveAPI, OutputRelayBindingID: testStaticRelayBindingID,
		Profile: profile, CoverAssets: videocover.NewLoader(nil, 1, 1<<20), CoverWitness: &httpCoverWitness{},
	}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "cover-stopped", Name: "Stopped Cover", InputURL: "srt://input.example.com:9000",
		YouTubeOutputMode: "live_api_relay_static", OutputRelayBindingID: testStaticRelayBindingID, YouTubeOutputReady: true,
		VideoCoverStart: &videocover.StartSnapshot{JobGeneration: 9, Revision: 1, IdempotencyKey: "start-inactive-9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop("cover-stopped"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, statusErr := manager.Status("cover-stopped")
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if state.Status == "stopped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream did not stop: %#v", state)
		}
		time.Sleep(time.Millisecond)
	}

	body := `{"stream_id":"cover-stopped","job_generation":9,"expected_generation":1,"revision":2,"active":false,"idempotency_key":"hide-after-stop","hide_confirmed":true}`
	request := httptest.NewRequest(http.MethodPut, "/streams/cover-stopped/video-cover-state", strings.NewReader(body))
	request.SetPathValue("id", "cover-stopped")
	request.Header.Set("Authorization", "Bearer cover-control-token")
	response := httptest.NewRecorder()
	putVideoCoverState(manager, TokenVerifier{PlainToken: "cover-control-token"}).ServeHTTP(response, request)

	var rejected videocover.ApplyResponse
	if err := json.Unmarshal(response.Body.Bytes(), &rejected); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusBadGateway || response.Header().Get("Cache-Control") != "no-store" ||
		rejected.StreamID != "cover-stopped" || rejected.JobGeneration != 9 || rejected.RequestedRevision != 2 ||
		rejected.ActualGeneration == 0 || !rejected.Rejected || rejected.Accepted || rejected.Applied ||
		rejected.Outcome != videocover.OutcomeRejected || rejected.Error == nil || rejected.Error.Code != videocover.ErrorCoverGraphUnavailable ||
		rejected.Actual.Readiness != videocover.ReadinessNotReady || rejected.Actual.Applied.State != "unknown" ||
		rejected.Actual.LastGoodApplied == nil || rejected.Actual.Error == nil || rejected.Actual.Error.Code != videocover.ErrorCoverGraphUnavailable {
		t.Fatalf("stopped rejection status=%d headers=%v response=%#v", response.Code, response.Header(), rejected)
	}

	invalidBody := `{"stream_id":"cover-stopped","job_generation":9,"expected_generation":1,"revision":0,"active":false,"idempotency_key":"invalid-after-stop","hide_confirmed":true}`
	invalidRequest := httptest.NewRequest(http.MethodPut, "/streams/cover-stopped/video-cover-state", strings.NewReader(invalidBody))
	invalidRequest.SetPathValue("id", "cover-stopped")
	invalidRequest.Header.Set("Authorization", "Bearer cover-control-token")
	invalidResponse := httptest.NewRecorder()
	putVideoCoverState(manager, TokenVerifier{PlainToken: "cover-control-token"}).ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusBadRequest || !strings.Contains(invalidResponse.Body.String(), `"code":"invalid_video_cover_request"`) || strings.Contains(invalidResponse.Body.String(), "requested_revision") {
		t.Fatalf("invalid stopped request was not rejected before response construction: status=%d body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}

	staleBody := `{"stream_id":"cover-stopped","job_generation":8,"expected_generation":1,"revision":2,"active":false,"idempotency_key":"stale-after-stop","hide_confirmed":true}`
	staleRequest := httptest.NewRequest(http.MethodPut, "/streams/cover-stopped/video-cover-state", strings.NewReader(staleBody))
	staleRequest.SetPathValue("id", "cover-stopped")
	staleRequest.Header.Set("Authorization", "Bearer cover-control-token")
	staleResponse := httptest.NewRecorder()
	putVideoCoverState(manager, TokenVerifier{PlainToken: "cover-control-token"}).ServeHTTP(staleResponse, staleRequest)
	var staleRejected videocover.ApplyResponse
	if err := json.Unmarshal(staleResponse.Body.Bytes(), &staleRejected); err != nil {
		t.Fatal(err)
	}
	if staleResponse.Code != http.StatusConflict || staleRejected.Error == nil || staleRejected.Error.Code != videocover.ErrorStaleJobGeneration ||
		staleRejected.JobGeneration != 9 || staleRejected.Actual.JobGeneration != 9 || staleRejected.RequestedRevision != 2 {
		t.Fatalf("stale stopped rejection status=%d response=%#v", staleResponse.Code, staleRejected)
	}
}

func TestVideoCoverPUTLegacyRuntimeReturnsNotFoundWithoutFabricatedGeneration(t *testing.T) {
	profile := ffmpeg.DefaultProfile()
	profile.Width, profile.Height, profile.FPS = 16, 9, 2
	manager := &streamproc.Manager{
		ArchiveRoot: t.TempDir(), FFmpegBin: "ffmpeg", Starter: &httpFakeStarter{}, InputResolver: testInputResolver,
		AllowHostnameInputs: true, OutputRelayURL: "rtmp://127.0.0.1/autostream/{stream_id}",
		OutputRelayMode: outputrelay.ModeManagedLiveAPI, OutputRelayBindingID: testStaticRelayBindingID,
		Profile: profile, CoverAssets: videocover.NewLoader(nil, 1, 1<<20), CoverWitness: &httpCoverWitness{},
	}
	_, err := manager.Start(lifecycle.StreamJob{
		StreamID: "cover-legacy", Name: "Legacy Cover", InputURL: "srt://input.example.com:9000",
		YouTubeOutputMode: "live_api_relay_static", OutputRelayBindingID: testStaticRelayBindingID, YouTubeOutputReady: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = manager.Stop("cover-legacy") })

	body := `{"stream_id":"cover-legacy","job_generation":9,"expected_generation":1,"revision":2,"active":false,"idempotency_key":"legacy-hide","hide_confirmed":true}`
	request := httptest.NewRequest(http.MethodPut, "/streams/cover-legacy/video-cover-state", strings.NewReader(body))
	request.SetPathValue("id", "cover-legacy")
	request.Header.Set("Authorization", "Bearer cover-control-token")
	response := httptest.NewRecorder()
	putVideoCoverState(manager, TokenVerifier{PlainToken: "cover-control-token"}).ServeHTTP(response, request)

	if response.Code != http.StatusNotFound || response.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(response.Body.String(), `"code":"capability_required"`) ||
		strings.Contains(response.Body.String(), "job_generation") || strings.Contains(response.Body.String(), "actual_generation") {
		t.Fatalf("legacy rejection fabricated state: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}
