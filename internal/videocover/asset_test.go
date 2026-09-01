package videocover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
)

type countingFetcher struct {
	mu      sync.Mutex
	data    []byte
	meta    FetchMetadata
	calls   int
	lastRef AssetRef
}

func (f *countingFetcher) Fetch(_ context.Context, ref AssetRef, _ int64) ([]byte, FetchMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastRef = ref
	return append([]byte(nil), f.data...), f.meta, nil
}

func opaquePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 12, G: 34, B: 56, A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func descriptorFor(t *testing.T, body []byte, width, height int) MediaAssetDescriptor {
	t.Helper()
	sum := sha256.Sum256(body)
	ppm := 0
	opaque := true
	return MediaAssetDescriptor{
		AssetID: "asset-cover-1", VariantID: "variant-cover-1", Usage: "video_cover",
		MediaType: "image/png", Width: width, Height: height, ByteSize: int64(len(body)),
		PixelCount: int64(width * height), Animated: false, AspectRatioErrorPPM: &ppm,
		Opaque: &opaque, SHA256: hex.EncodeToString(sum[:]), Revision: 1, Readiness: ReadinessReady,
	}
}

func TestLoaderValidatesAndCachesImmutableVariant(t *testing.T) {
	body := opaquePNG(t, 16, 9)
	desc := descriptorFor(t, body, 16, 9)
	fetcher := &countingFetcher{data: body, meta: FetchMetadata{
		MediaType: "image/png", ByteSize: int64(len(body)), AssetID: desc.AssetID,
		VariantID: desc.VariantID, Width: 16, Height: 9, SHA256: desc.SHA256,
	}}
	loader := NewLoader(fetcher, 4, 1<<20)
	first, err := loader.Load(context.Background(), "stream-01", desc, 16, 9)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loader.Load(context.Background(), "stream-01", desc, 16, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || fetcher.calls != 1 {
		t.Fatalf("immutable cache mismatch: calls=%d equal=%v", fetcher.calls, bytes.Equal(first, second))
	}
	if fetcher.lastRef.StreamID != "stream-01" || fetcher.lastRef.AssetID != desc.AssetID || fetcher.lastRef.VariantID != desc.VariantID {
		t.Fatalf("fetch must use safe identities only: %#v", fetcher.lastRef)
	}
}

func TestLoaderRejectsHashDimensionTransparencyAndDoesNotCacheFailures(t *testing.T) {
	body := opaquePNG(t, 16, 9)
	tests := []struct {
		name     string
		edit     func(*MediaAssetDescriptor)
		editMeta func(*FetchMetadata)
		code     ErrorCode
	}{
		{"hash", func(d *MediaAssetDescriptor) { d.SHA256 = string(bytes.Repeat([]byte{'0'}, 64)) }, nil, ErrorMediaAssetHashMismatch},
		{"dimension", func(*MediaAssetDescriptor) {}, func(m *FetchMetadata) { m.Width = 15 }, ErrorMediaAssetDimensionMismatch},
		{"unsupported", func(d *MediaAssetDescriptor) { d.MediaType = "image/gif" }, nil, ErrorMediaAssetFormatUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desc := descriptorFor(t, body, 16, 9)
			tt.edit(&desc)
			meta := FetchMetadata{MediaType: "image/png", ByteSize: int64(len(body)), AssetID: desc.AssetID, VariantID: desc.VariantID, Width: 16, Height: 9, SHA256: descriptorFor(t, body, 16, 9).SHA256}
			if tt.editMeta != nil {
				tt.editMeta(&meta)
			}
			fetcher := &countingFetcher{data: body, meta: meta}
			loader := NewLoader(fetcher, 4, 1<<20)
			for attempt := 0; attempt < 2; attempt++ {
				_, err := loader.Load(context.Background(), "stream-01", desc, 16, 9)
				if ErrorCodeOf(err) != tt.code {
					t.Fatalf("error=%v code=%q want=%q", err, ErrorCodeOf(err), tt.code)
				}
			}
			if tt.name != "unsupported" && fetcher.calls != 2 {
				t.Fatalf("failed fetch must not be cached: calls=%d", fetcher.calls)
			}
			if tt.name == "unsupported" && fetcher.calls != 0 {
				t.Fatalf("invalid descriptor must fail before fetch: calls=%d", fetcher.calls)
			}
		})
	}
}

func TestLoaderAnimationDetectionParsesChunkTypesWithoutPayloadFalsePositive(t *testing.T) {
	body := pngWithTextChunk(t, opaquePNG(t, 16, 9), []byte("Comment\x00static payload mentioning acTL is not an APNG chunk"))
	desc := descriptorFor(t, body, 16, 9)
	fetcher := &countingFetcher{data: body, meta: FetchMetadata{
		MediaType: desc.MediaType, ByteSize: desc.ByteSize, AssetID: desc.AssetID,
		VariantID: desc.VariantID, Width: desc.Width, Height: desc.Height, SHA256: desc.SHA256,
	}}
	if _, err := NewLoader(fetcher, 1, 1<<20).Load(context.Background(), "stream-01", desc, 16, 9); err != nil {
		t.Fatalf("static ancillary payload was mistaken for animation: %v", err)
	}
}

func pngWithTextChunk(t *testing.T, body, data []byte) []byte {
	t.Helper()
	iend := bytes.LastIndex(body, []byte("IEND"))
	if iend < 4 {
		t.Fatal("PNG IEND chunk not found")
	}
	insertAt := iend - 4
	typeBytes := []byte("tEXt")
	chunk := make([]byte, 12+len(data))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(data)))
	copy(chunk[4:8], typeBytes)
	copy(chunk[8:8+len(data)], data)
	crcBody := append(append([]byte(nil), typeBytes...), data...)
	binary.BigEndian.PutUint32(chunk[8+len(data):], crc32.ChecksumIEEE(crcBody))
	out := make([]byte, 0, len(body)+len(chunk))
	out = append(out, body[:insertAt]...)
	out = append(out, chunk...)
	return append(out, body[insertAt:]...)
}

func TestPipelineInvariantFreezesVisualOrderAndAudioContinuity(t *testing.T) {
	p := FixedPipelineInvariant()
	want := []string{"base_or_worker_scene", "video_cover", "watermark", "video_encode", "tee_live_archive_preview"}
	if len(p.Layers) != len(want) {
		t.Fatalf("layers=%v", p.Layers)
	}
	for i := range want {
		if p.Layers[i] != want[i] {
			t.Fatalf("layers=%v", p.Layers)
		}
	}
	if !p.WatermarkTopmost || !p.CoverWatermarkIndependent || len(p.OutputParity) != 3 || p.AudioContinuity != (VisualAudioContinuity{}) {
		t.Fatalf("pipeline invariant=%#v", p)
	}
}

func TestStartSnapshotAllowsInactiveEpochButRejectsAssetAndUnknownFields(t *testing.T) {
	var snapshot StartSnapshot
	if err := json.Unmarshal([]byte(`{"job_generation":4,"revision":1,"active":false,"idempotency_key":"start-inactive"}`), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.JobGeneration != 4 || snapshot.Active || snapshot.CoverAsset != nil {
		t.Fatalf("inactive snapshot=%#v", snapshot)
	}
	if err := json.Unmarshal([]byte(`{"job_generation":4,"revision":1,"active":false,"idempotency_key":"start-inactive","storage_path":"/secret"}`), &snapshot); err == nil {
		t.Fatal("unknown or storage fields must be rejected inside video_cover_start")
	}
	if err := json.Unmarshal([]byte(`{"job_generation":4,"revision":1,"idempotency_key":"start-inactive"}`), &snapshot); err == nil {
		t.Fatal("active must be explicitly present in video_cover_start")
	}
	if err := json.Unmarshal([]byte(`{"job_generation":4,"revision":1,"active":null,"idempotency_key":"start-inactive"}`), &snapshot); err == nil {
		t.Fatal("active:null must not be decoded as inactive video_cover_start")
	}
	if err := json.Unmarshal([]byte(`{"job_generation":4,"revision":1,"active":false,"idempotency_key":"start-inactive","cover_asset":null}`), &snapshot); err == nil {
		t.Fatal("inactive video_cover_start must reject present cover_asset even when null")
	}
	if err := json.Unmarshal([]byte(`{"job_generation":4,"revision":1,"active":false,"active":true,"idempotency_key":"duplicate-active","cover_asset":null}`), &snapshot); err == nil {
		t.Fatal("duplicate DTO fields must be rejected before typed decoding")
	}
	var request ApplyRequest
	if err := json.Unmarshal([]byte(`{"stream_id":"stream-1","job_generation":4,"expected_generation":1,"revision":2,"idempotency_key":"hide-2","hide_confirmed":true}`), &request); err == nil {
		t.Fatal("active must be explicitly present in an apply request")
	}
	if err := json.Unmarshal([]byte(`{"stream_id":"stream-1","job_generation":4,"expected_generation":1,"revision":2,"active":null,"idempotency_key":"hide-2","hide_confirmed":true}`), &request); err == nil {
		t.Fatal("active:null must not be decoded as an inactive apply request")
	}
	if err := json.Unmarshal([]byte(`{"stream_id":"stream-1","job_generation":4,"expected_generation":1,"revision":2,"active":true,"idempotency_key":"show-2","cover_asset":null,"hide_confirmed":false}`), &request); err == nil {
		t.Fatal("active apply must reject forbidden hide_confirmed presence and null cover_asset")
	}
	body := opaquePNG(t, 16, 9)
	descriptorBody, err := json.Marshal(descriptorFor(t, body, 16, 9))
	if err != nil {
		t.Fatal(err)
	}
	var descriptorFields map[string]any
	if err := json.Unmarshal(descriptorBody, &descriptorFields); err != nil {
		t.Fatal(err)
	}
	delete(descriptorFields, "animated")
	descriptorBody, _ = json.Marshal(descriptorFields)
	var descriptor MediaAssetDescriptor
	if err := json.Unmarshal(descriptorBody, &descriptor); err == nil {
		t.Fatal("required false-valued animated field must not be silently defaulted")
	}
	descriptorBody, _ = json.Marshal(descriptorFor(t, body, 16, 9))
	if err := json.Unmarshal(descriptorBody, &descriptorFields); err != nil {
		t.Fatal(err)
	}
	descriptorFields["animated"] = nil
	descriptorBody, _ = json.Marshal(descriptorFields)
	if err := json.Unmarshal(descriptorBody, &descriptor); err == nil {
		t.Fatal("animated:null must not be decoded as the required false value")
	}
	descriptorBody, _ = json.Marshal(descriptorFor(t, body, 16, 9))
	if err := json.Unmarshal(descriptorBody, &descriptorFields); err != nil {
		t.Fatal(err)
	}
	delete(descriptorFields, "opaque")
	descriptorBody, _ = json.Marshal(descriptorFields)
	if err := json.Unmarshal(descriptorBody, &descriptor); err == nil {
		t.Fatal("video_cover descriptor must preserve opaque field presence")
	}
}
