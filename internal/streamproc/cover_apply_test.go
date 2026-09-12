package streamproc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/imagefeed"
	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

func TestVideoCoverApplyIsFencedIndependentAndAmbiguousHideIsNotResent(t *testing.T) {
	body, descriptor := coverPNGFixture(t)
	fetcher := &coverTestFetcher{body: body, meta: videocover.FetchMetadata{MediaType: descriptor.MediaType, ByteSize: descriptor.ByteSize, AssetID: descriptor.AssetID, VariantID: descriptor.VariantID, Width: descriptor.Width, Height: descriptor.Height, SHA256: descriptor.SHA256}}
	starter := &fakeStarter{}
	witness := &coverTestWitness{}
	manager := coverTestManager(t, starter, fetcher, witness)
	if _, err := manager.Start(coverStartJob(nil)); err != nil {
		t.Fatal(err)
	}
	if len(starter.processes) != 1 || witness.calls != 1 {
		t.Fatalf("start/process witness counts=(%d,%d)", len(starter.processes), witness.calls)
	}
	if _, err := manager.UpdateRuntimeSettings("stream-cover", RuntimeSettings{EncoderAudioGainDB: 2.5, OverlayConfig: map[string]any{"watermark_enabled": false}}); err != nil {
		t.Fatal(err)
	}
	request := videocover.ApplyRequest{StreamID: "stream-cover", JobGeneration: 7, ExpectedGeneration: 1, Revision: 2, Active: true, IdempotencyKey: "show-cover-2", CoverAsset: &descriptor}
	response, err := manager.ApplyVideoCover(context.Background(), "stream-cover", request)
	if err != nil || !response.Applied || response.Actual.AppliedWitness == nil || !response.Actual.AppliedWitness.GraphApplied {
		t.Fatalf("show response=%#v err=%v", response, err)
	}
	if fetcher.calls != 1 || witness.calls != 2 {
		t.Fatalf("show counts fetch=%d witness=%d", fetcher.calls, witness.calls)
	}
	watermarkBefore := response.Actual.Watermark
	if replay, err := manager.ApplyVideoCover(context.Background(), "stream-cover", request); err != nil || replay.Outcome != videocover.OutcomeApplied || fetcher.calls != 1 || witness.calls != 2 {
		t.Fatalf("exact replay performed side effects: response=%#v err=%v fetch=%d witness=%d", replay, err, fetcher.calls, witness.calls)
	}

	witness.failNext = true
	hide := videocover.ApplyRequest{StreamID: "stream-cover", JobGeneration: 7, ExpectedGeneration: 1, Revision: 3, IdempotencyKey: "hide-cover-3", HideConfirmed: true}
	ambiguous, err := manager.ApplyVideoCover(context.Background(), "stream-cover", hide)
	if err != nil || ambiguous.Outcome != videocover.OutcomeAmbiguous || ambiguous.Applied || ambiguous.Actual.Applied.State != "unknown" || ambiguous.Actual.LastGoodApplied == nil || !*ambiguous.Actual.LastGoodApplied.Active {
		t.Fatalf("ambiguous hide=%#v err=%v", ambiguous, err)
	}
	if ambiguous.Actual.CoverAsset != nil {
		t.Fatalf("ambiguous hide must not retain a desired cover_asset: %#v", ambiguous.Actual.CoverAsset)
	}
	if ambiguous.Actual.Watermark != watermarkBefore || !ambiguous.Actual.NoAutomaticResend {
		t.Fatalf("cover changed independent watermark/no-resend: %#v", ambiguous.Actual)
	}
	if replay, err := manager.ApplyVideoCover(context.Background(), "stream-cover", hide); err != nil || replay.Outcome != videocover.OutcomeAmbiguous || witness.calls != 3 {
		t.Fatalf("ambiguous replay resent feed: response=%#v err=%v witness=%d", replay, err, witness.calls)
	}
	reconciled, err := manager.VideoCoverState("stream-cover")
	if err != nil || reconciled.Readiness != videocover.ReadinessUnknown || reconciled.Error == nil || reconciled.Error.Code != videocover.ErrorCoverApplyAmbiguous {
		t.Fatalf("GET reconcile=%#v err=%v", reconciled, err)
	}
	process := starter.processes[0]
	if len(starter.processes) != 1 || process.terminated || process.killed || len(process.commands) != 1 {
		t.Fatalf("cover mutated process/audio lifecycle: starts=%d terminated=%v killed=%v commands=%v", len(starter.processes), process.terminated, process.killed, process.commands)
	}
	if !zeroAudioContinuity(reconciled.Pipeline.AudioContinuity) {
		t.Fatalf("audio continuity counters changed: %#v", reconciled.Pipeline.AudioContinuity)
	}
}

func TestVideoCoverStateBindsIndependentCurrentWatermarkIntoWitness(t *testing.T) {
	starter := &fakeStarter{}
	witness := &coverTestWitness{}
	manager := coverTestManager(t, starter, &coverTestFetcher{}, witness)
	if _, err := manager.Start(coverStartJob(nil)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = manager.Stop("stream-cover") })

	before, err := manager.VideoCoverState("stream-cover")
	if err != nil || before.AppliedWitness == nil {
		t.Fatalf("initial state=%#v err=%v", before, err)
	}
	if _, err := manager.UpdateRuntimeSettings("stream-cover", RuntimeSettings{
		EncoderAudioGainDB: 1.5,
		OverlayConfig:      map[string]any{"watermark_enabled": false},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := manager.VideoCoverState("stream-cover")
	if err != nil || after.AppliedWitness == nil {
		t.Fatalf("updated state=%#v err=%v", after, err)
	}
	if after.Cover != before.Cover || after.Desired != before.Desired || after.Applied.Revision != before.Applied.Revision {
		t.Fatalf("Watermark update changed Cover-owned state: before=%#v after=%#v", before, after)
	}
	if after.Watermark.Revision != before.Watermark.Revision+1 || after.AppliedWitness.Watermark != after.Watermark {
		t.Fatalf("current Watermark was not linked into witness: before=%#v after=%#v", before.Watermark, after)
	}
	watermarkWitness := manager.WatermarkWitness.(*watermarkTestWitness)
	if watermarkWitness.calls != 1 {
		t.Fatalf("Watermark state advanced without one graph witness: calls=%d", watermarkWitness.calls)
	}
	if len(starter.processes) != 1 || len(starter.processes[0].commands) != 1 {
		t.Fatalf("Watermark observation restarted Cover graph/process: starts=%d commands=%v", len(starter.processes), starter.processes[0].commands)
	}

	watermarkWitness.failNext = true
	if _, err := manager.UpdateRuntimeSettings("stream-cover", RuntimeSettings{
		EncoderAudioGainDB: 1.5,
		OverlayConfig:      map[string]any{"watermark_enabled": false},
	}); videocover.ErrorCodeOf(err) != videocover.ErrorCoverGraphUnavailable {
		t.Fatalf("missing Watermark graph witness error=%v", err)
	}
	unknown, err := manager.VideoCoverState("stream-cover")
	if err != nil || unknown.Readiness != videocover.ReadinessUnknown || unknown.AppliedWitness != nil || unknown.LastGoodApplied == nil || unknown.Error == nil || unknown.Error.Code != videocover.ErrorCoverGraphUnavailable {
		t.Fatalf("failed Watermark witness did not invalidate graph state: state=%#v err=%v", unknown, err)
	}
}

func TestVideoCoverAssetRejectionUsesContractSafeNotReadyActualWithoutMutatingGraph(t *testing.T) {
	body, descriptor := coverPNGFixture(t)
	badMeta := videocover.FetchMetadata{
		MediaType: descriptor.MediaType, ByteSize: descriptor.ByteSize,
		AssetID: descriptor.AssetID, VariantID: descriptor.VariantID,
		Width: descriptor.Width - 1, Height: descriptor.Height, SHA256: descriptor.SHA256,
	}
	fetcher := &coverTestFetcher{body: body, meta: badMeta}
	witness := &coverTestWitness{}
	manager := coverTestManager(t, &fakeStarter{}, fetcher, witness)
	if _, err := manager.Start(coverStartJob(nil)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = manager.Stop("stream-cover") })

	request := videocover.ApplyRequest{
		StreamID: "stream-cover", JobGeneration: 7, ExpectedGeneration: 1,
		Revision: 2, Active: true, IdempotencyKey: "show-invalid-fetch-2", CoverAsset: &descriptor,
	}
	response, err := manager.ApplyVideoCover(context.Background(), "stream-cover", request)
	if err != nil {
		t.Fatal(err)
	}
	if !response.Rejected || response.Outcome != videocover.OutcomeRejected || response.Error == nil || response.Error.Code != videocover.ErrorMediaAssetDimensionMismatch {
		t.Fatalf("asset rejection=%#v", response)
	}
	if response.Actual.Readiness != videocover.ReadinessNotReady || response.Actual.Error == nil || response.Actual.Error.Code != response.Error.Code {
		t.Fatalf("502 contract actual/error linkage=%#v", response)
	}
	if response.Actual.AppliedWitness == nil || response.Actual.Applied.State != "known" || witness.calls != 1 || fetcher.calls != 1 {
		t.Fatalf("rejection mutated graph or performed unexpected work: response=%#v witness=%d fetch=%d", response, witness.calls, fetcher.calls)
	}
	current, err := manager.VideoCoverState("stream-cover")
	if err != nil || current.Readiness != videocover.ReadinessReady || current.Error != nil || current.Desired.Active {
		t.Fatalf("response-only rejection state mutated authoritative graph: state=%#v err=%v", current, err)
	}

	manager.CoverAssets = nil
	capability := request
	capability.IdempotencyKey = "show-without-capability-2"
	capabilityResponse, err := manager.ApplyVideoCover(context.Background(), "stream-cover", capability)
	if err != nil || !capabilityResponse.Rejected || capabilityResponse.Error == nil || capabilityResponse.Error.Code != videocover.ErrorCapabilityRequired ||
		capabilityResponse.Actual.Readiness != videocover.ReadinessNotReady || capabilityResponse.Actual.Error == nil || capabilityResponse.Actual.Error.Code != videocover.ErrorCapabilityRequired {
		t.Fatalf("capability rejection did not carry 502-safe actual state: response=%#v err=%v", capabilityResponse, err)
	}
}

func TestVideoCoverTypedValidationPrecedesCapabilityAndRejectsInvalidUTF8(t *testing.T) {
	_, descriptor := coverPNGFixture(t)
	descriptor.MediaType = "image/gif"
	manager := coverTestManager(t, &fakeStarter{}, &coverTestFetcher{}, &coverTestWitness{})
	manager.CoverAssets = nil
	job := coverStartJob(&descriptor)
	if _, err := manager.Start(job); videocover.ErrorCodeOf(err) != videocover.ErrorInvalidRequest {
		t.Fatalf("malformed descriptor must precede capability failure: %v", err)
	}
	job = coverStartJob(nil)
	job.VideoCoverStart.IdempotencyKey = string([]byte{0xff})
	if _, err := manager.Start(job); videocover.ErrorCodeOf(err) != videocover.ErrorInvalidRequest {
		t.Fatalf("invalid UTF-8 idempotency key accepted: %v", err)
	}
	job = coverStartJob(nil)
	job.VideoCoverStart.IdempotencyKey = " padded-idempotency "
	if _, err := manager.Start(job); videocover.ErrorCodeOf(err) != videocover.ErrorInvalidRequest {
		t.Fatalf("non-canonical idempotency key accepted: %v", err)
	}
	for _, key := range []string{"\u0085padded-idempotency", "\ufeffpadded-idempotency"} {
		job.VideoCoverStart.IdempotencyKey = key
		if _, err := manager.Start(job); videocover.ErrorCodeOf(err) != videocover.ErrorInvalidRequest {
			t.Fatalf("Unicode edge-space idempotency key accepted: %q: %v", key, err)
		}
	}
}

func TestVideoCoverActiveStartRequiresAssetAndGraphWitness(t *testing.T) {
	body, descriptor := coverPNGFixture(t)
	newFetcher := func() *coverTestFetcher {
		return &coverTestFetcher{body: body, meta: videocover.FetchMetadata{MediaType: descriptor.MediaType, ByteSize: descriptor.ByteSize, AssetID: descriptor.AssetID, VariantID: descriptor.VariantID, Width: 16, Height: 9, SHA256: descriptor.SHA256}}
	}
	fetcher, starter, witness := newFetcher(), &fakeStarter{}, &coverTestWitness{}
	manager := coverTestManager(t, starter, fetcher, witness)
	if _, err := manager.Start(coverStartJob(&descriptor)); err != nil {
		t.Fatal(err)
	}
	state, err := manager.VideoCoverState("stream-cover")
	if err != nil || state.Readiness != videocover.ReadinessReady || state.AppliedWitness == nil || !state.Cover.Enabled || state.CoverAsset == nil || fetcher.calls != 1 || witness.calls != 1 {
		t.Fatalf("active start state=%#v err=%v fetch=%d witness=%d", state, err, fetcher.calls, witness.calls)
	}

	failingFetcher, failingStarter := newFetcher(), &fakeStarter{}
	failingWitness := &coverTestWitness{failNext: true}
	failing := coverTestManager(t, failingStarter, failingFetcher, failingWitness)
	if _, err := failing.Start(coverStartJob(&descriptor)); videocover.ErrorCodeOf(err) != videocover.ErrorCoverGraphUnavailable {
		t.Fatalf("missing start witness error=%v", err)
	}
	if failingFetcher.calls != 1 || failingWitness.calls != 1 || len(failingStarter.processes) != 1 || !failingStarter.processes[0].killed {
		t.Fatalf("failed start cleanup fetch=%d witness=%d processes=%d killed=%v", failingFetcher.calls, failingWitness.calls, len(failingStarter.processes), len(failingStarter.processes) == 1 && failingStarter.processes[0].killed)
	}
}

func TestVideoCoverGenerationRestartCleanupAndLegacyFailClosed(t *testing.T) {
	body, descriptor := coverPNGFixture(t)
	fetcher := &coverTestFetcher{body: body, meta: videocover.FetchMetadata{MediaType: descriptor.MediaType, ByteSize: descriptor.ByteSize, AssetID: descriptor.AssetID, VariantID: descriptor.VariantID, Width: 16, Height: 9, SHA256: descriptor.SHA256}}
	starter, witness := &fakeStarter{}, &coverTestWitness{}
	manager := coverTestManager(t, starter, fetcher, witness)
	if _, err := manager.Start(coverStartJob(nil)); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	firstTracked := manager.processes["stream-cover"]
	firstCover := firstTracked.cover
	firstWatermark := firstTracked.watermark
	manager.mu.Unlock()
	if _, err := manager.Stop("stream-cover"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if err := firstCover.Update([]byte("after-stop")); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cover feed was not closed during process cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := firstWatermark.Update([]byte("after-stop")); err == nil {
		t.Fatal("watermark feed was not closed during process cleanup")
	}
	replacementStarter := &fakeStarter{}
	replacement := coverTestManager(t, replacementStarter, fetcher, &coverTestWitness{})
	replacement.ArchiveRoot = manager.ArchiveRoot
	if _, err := replacement.Start(coverStartJob(nil)); err != nil {
		t.Fatal(err)
	}
	state, err := replacement.VideoCoverState("stream-cover")
	if err != nil || state.Generation != 2 {
		t.Fatalf("restart state=%#v err=%v", state, err)
	}
	if response, err := replacement.ApplyVideoCover(context.Background(), "stream-cover", videocover.ApplyRequest{StreamID: "stream-cover", JobGeneration: 7, ExpectedGeneration: 1, Revision: 2, Active: true, IdempotencyKey: "stale-generation", CoverAsset: &descriptor}); err != nil || response.Error == nil || response.Error.Code != videocover.ErrorStaleCoverGeneration || fetcher.calls != 0 {
		t.Fatalf("stale generation reached fetch: response=%#v err=%v fetch=%d", response, err, fetcher.calls)
	}

	legacyStarter := &fakeStarter{}
	legacy := coverTestManager(t, legacyStarter, fetcher, &coverTestWitness{})
	job := coverStartJob(nil)
	job.StreamID = "legacy-cover"
	job.VideoCoverStart = nil
	if _, err := legacy.Start(job); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.VideoCoverState("legacy-cover"); videocover.ErrorCodeOf(err) != videocover.ErrorCapabilityRequired {
		t.Fatalf("legacy GET error=%v", err)
	}
	if _, err := legacy.ApplyVideoCover(context.Background(), "legacy-cover", videocover.ApplyRequest{}); videocover.ErrorCodeOf(err) != videocover.ErrorInvalidRequest {
		t.Fatalf("invalid legacy PUT error=%v", err)
	}
	validLegacyRequest := videocover.ApplyRequest{
		StreamID: "legacy-cover", JobGeneration: 7, ExpectedGeneration: 1,
		Revision: 2, IdempotencyKey: "legacy-hide-2", HideConfirmed: true,
	}
	if _, err := legacy.ApplyVideoCover(context.Background(), "legacy-cover", validLegacyRequest); videocover.ErrorCodeOf(err) != videocover.ErrorCapabilityRequired {
		t.Fatalf("valid legacy PUT error=%v", err)
	}
	if joined := strings.Join(legacyStarter.args, " "); !strings.Contains(joined, "[base][cover]overlay=0:0") || !strings.Contains(joined, "[covered][wm]overlay=0:0") {
		t.Fatalf("legacy start did not retain fixed transparent graph: %s", joined)
	}
}

func TestAudioContinuityOracleDetectsEveryNegativeMutation(t *testing.T) {
	mutations := []videocover.VisualAudioContinuity{
		{ProcessRestart: 1}, {AudioEncoderRestart: 1}, {AudioMuxRestart: 1},
		{GraphRebuild: 1}, {Reconnect: 1}, {SequenceLoss: 1},
		{TimestampDiscontinuity: 1}, {IntentionalMuteInsertion: 1},
	}
	for _, mutation := range mutations {
		if zeroAudioContinuity(mutation) {
			t.Fatalf("negative mutation escaped continuity oracle: %#v", mutation)
		}
	}
}

func TestVideoCoverReplayCacheIsBounded(t *testing.T) {
	tracked := &trackedProcess{}
	for index := 0; index < maxCoverReplayEntries+1; index++ {
		key := "request-" + strconv.Itoa(index)
		tracked.storeCoverReplay(key, coverReplay{})
	}
	if len(tracked.coverReplay) != maxCoverReplayEntries || len(tracked.coverReplayOrder) != maxCoverReplayEntries {
		t.Fatalf("replay cache size=(%d,%d)", len(tracked.coverReplay), len(tracked.coverReplayOrder))
	}
	if _, exists := tracked.coverReplay["request-0"]; exists {
		t.Fatal("oldest replay was not evicted")
	}
}

func TestProgressCoverWitnessRequiresExactFeedDeliveryAndOutputAdvance(t *testing.T) {
	source, err := imagefeed.New("video cover", []byte("initial!"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(source.InputURL(), "tcp://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	initial := make([]byte, len("initial!"))
	if _, err := io.ReadFull(conn, initial); err != nil {
		t.Fatal(err)
	}
	progressPath := filepath.Join(t.TempDir(), "progress.txt")
	if err := os.WriteFile(progressPath, []byte("frame=1\nout_time_us=1000\nprogress=continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	next := []byte("updated!")
	readerDone := make(chan error, 1)
	go func() {
		got := make([]byte, len(next))
		if _, err := io.ReadFull(conn, got); err != nil {
			readerDone <- err
			return
		}
		if !bytes.Equal(got, next) {
			readerDone <- errors.New("wrong delivered cover version")
			return
		}
		time.Sleep(50 * time.Millisecond)
		readerDone <- os.WriteFile(progressPath, []byte("frame=2\nout_time_us=2000\nprogress=continue\n"), 0o600)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (progressCoverWitness{}).Apply(ctx, source, next, false, progressPath); err != nil {
		t.Fatal(err)
	}
	if err := <-readerDone; err != nil {
		t.Fatal(err)
	}

	unconnected, err := imagefeed.New("video cover", []byte("initial!"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unconnected.Close() })
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer timeoutCancel()
	if err := (progressCoverWitness{}).Apply(timeoutCtx, unconnected, next, false, progressPath); err == nil {
		t.Fatal("missing feed delivery must not produce a graph witness")
	}
}
