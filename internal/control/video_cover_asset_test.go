package control

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/videocover"
)

func TestVideoCoverFetchIsAuthenticatedBoundedAndIdentityOnly(t *testing.T) {
	body := []byte("processed-cover")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer node-token"; got != want {
			t.Fatalf("authorization=%q want=%q", got, want)
		}
		if got, want := r.URL.EscapedPath(), "/internal/streams/stream-1/media-assets/variant-1"; got != want {
			t.Fatalf("path=%q want=%q", got, want)
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-AutoStream-Asset-ID", "asset-1")
		w.Header().Set("X-AutoStream-Variant-ID", "variant-1")
		w.Header().Set("X-AutoStream-Width", "16")
		w.Header().Set("X-AutoStream-Height", "9")
		digest := sha256.Sum256(body)
		w.Header().Set("Digest", "sha-256="+base64.StdEncoding.EncodeToString(digest[:]))
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "node-token", ServiceID: "encoder-1", ServiceName: "Encoder", ServicePublicURL: server.URL}}
	got, meta, err := client.Fetch(context.Background(), videocover.AssetRef{StreamID: "stream-1", AssetID: "asset-1", VariantID: "variant-1"}, int64(len(body)))
	if err != nil || string(got) != string(body) || meta.AssetID != "asset-1" || meta.VariantID != "variant-1" || meta.Width != 16 || meta.Height != 9 {
		t.Fatalf("fetch=(%q,%#v,%v)", got, meta, err)
	}
	if _, _, err := client.Fetch(context.Background(), videocover.AssetRef{StreamID: "stream-1", AssetID: "asset-1", VariantID: "variant-1"}, int64(len(body)-1)); videocover.ErrorCodeOf(err) != videocover.ErrorMediaAssetTooLarge {
		t.Fatalf("bounded fetch error=%v", err)
	}
}

func TestVideoCoverFetchAcceptsChunkedBodyAndUsesActualByteSize(t *testing.T) {
	body := []byte("chunked-processed-cover")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-AutoStream-Asset-ID", "asset-1")
		w.Header().Set("X-AutoStream-Variant-ID", "variant-1")
		w.Header().Set("X-AutoStream-Width", "16")
		w.Header().Set("X-AutoStream-Height", "9")
		digest := sha256.Sum256(body)
		w.Header().Set("Digest", "sha-256="+base64.StdEncoding.EncodeToString(digest[:]))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "node-token", ServiceID: "encoder-1", ServiceName: "Encoder", ServicePublicURL: server.URL}}
	got, meta, err := client.Fetch(context.Background(), videocover.AssetRef{StreamID: "stream-1", AssetID: "asset-1", VariantID: "variant-1"}, int64(len(body)))
	if err != nil || string(got) != string(body) || meta.ByteSize != int64(len(body)) {
		t.Fatalf("chunked fetch=(%q,%#v,%v)", got, meta, err)
	}
}

func TestVideoCoverFetchRejectsZeroLengthBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := Client{Config: Config{ControlPanelURL: server.URL, Token: "node-token", ServiceID: "encoder-1", ServiceName: "Encoder", ServicePublicURL: server.URL}}
	_, _, err := client.Fetch(context.Background(), videocover.AssetRef{StreamID: "stream-1", AssetID: "asset-1", VariantID: "variant-1"}, 1024)
	if got := videocover.ErrorCodeOf(err); got != videocover.ErrorMediaAssetVariantFailed {
		t.Fatalf("zero-length fetch code=%q err=%v", got, err)
	}
}

func TestVideoCoverFetchRejectsRedirectWithoutForwardingBearer(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()
	client := Client{Config: Config{ControlPanelURL: source.URL, Token: "redirect-secret", ServiceID: "encoder-1", ServiceName: "Encoder", ServicePublicURL: source.URL}, HTTP: &http.Client{}}
	_, _, err := client.Fetch(context.Background(), videocover.AssetRef{StreamID: "stream-1", AssetID: "asset-1", VariantID: "variant-1"}, 1024)
	if videocover.ErrorCodeOf(err) != videocover.ErrorMediaAssetVariantFailed || redirected.Load() != 0 {
		t.Fatalf("redirect error=%v target_calls=%d", err, redirected.Load())
	}
	if strings.Contains(err.Error(), "redirect-secret") || strings.Contains(err.Error(), source.URL) || strings.Contains(err.Error(), target.URL) {
		t.Fatalf("safe error leaked token/url: %v", err)
	}
}

func TestVideoCoverFetchMapsControlPlaneFaultsToContractSafeCodes(t *testing.T) {
	tests := []struct {
		status int
		code   videocover.ErrorCode
	}{
		{http.StatusUnauthorized, videocover.ErrorMediaAssetUnauthorized},
		{http.StatusForbidden, videocover.ErrorMediaAssetUnauthorized},
		{http.StatusNotFound, videocover.ErrorMediaAssetNotFound},
		{http.StatusRequestTimeout, videocover.ErrorMediaAssetTimeout},
		{http.StatusGatewayTimeout, videocover.ErrorMediaAssetTimeout},
		{http.StatusRequestEntityTooLarge, videocover.ErrorMediaAssetTooLarge},
		{http.StatusUnsupportedMediaType, videocover.ErrorMediaAssetFormatUnsupported},
		{http.StatusConflict, videocover.ErrorMediaAssetHashMismatch},
		{http.StatusInternalServerError, videocover.ErrorMediaAssetVariantFailed},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			client := Client{Config: Config{ControlPanelURL: server.URL, Token: "node-token", ServiceID: "encoder-1", ServiceName: "Encoder", ServicePublicURL: server.URL}}
			_, _, err := client.Fetch(context.Background(), videocover.AssetRef{StreamID: "stream-1", AssetID: "asset-1", VariantID: "variant-1"}, 1024)
			if got := videocover.ErrorCodeOf(err); got != test.code {
				t.Fatalf("status=%d code=%q want=%q", test.status, got, test.code)
			}
			if strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "node-token") {
				t.Fatalf("safe fault exposed transport data: %v", err)
			}
		})
	}
}

func TestVideoCoverFetchFailsClosedOnMissingOrMismatchedResponseIdentity(t *testing.T) {
	body := []byte(strings.Repeat("x", 1024))
	tests := []struct {
		name      string
		assetID   string
		variantID string
		digest    string
	}{
		{name: "missing asset", variantID: "variant-1", digest: "valid"},
		{name: "wrong variant", assetID: "asset-1", variantID: "variant-other", digest: "valid"},
		{name: "missing digest", assetID: "asset-1", variantID: "variant-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "image/png")
				w.Header().Set("X-AutoStream-Asset-ID", test.assetID)
				w.Header().Set("X-AutoStream-Variant-ID", test.variantID)
				w.Header().Set("X-AutoStream-Width", "16")
				w.Header().Set("X-AutoStream-Height", "9")
				if test.digest == "valid" {
					digest := sha256.Sum256(body)
					w.Header().Set("Digest", "sha-256="+base64.StdEncoding.EncodeToString(digest[:]))
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()
			client := Client{Config: Config{ControlPanelURL: server.URL, Token: "node-token", ServiceID: "encoder-1", ServiceName: "Encoder", ServicePublicURL: server.URL}}
			_, _, err := client.Fetch(context.Background(), videocover.AssetRef{StreamID: "stream-1", AssetID: "asset-1", VariantID: "variant-1"}, 1024)
			if got := videocover.ErrorCodeOf(err); got != videocover.ErrorMediaAssetVariantFailed {
				t.Fatalf("identity fault code=%q err=%v", got, err)
			}
		})
	}
}

func TestVideoCoverCapabilityIsAdvertisedOnlyWithValidatedControlPlane(t *testing.T) {
	t.Setenv("AUTOSTREAM_NODE_CONFIG", "")
	t.Setenv("CONTROL_PANEL_URL", "http://127.0.0.1:3000")
	t.Setenv("CONTROL_PANEL_TOKEN", "node-token")
	t.Setenv("SERVICE_ID", "encoder-1")
	t.Setenv("SERVICE_NAME", "Encoder")
	t.Setenv("SERVICE_PUBLIC_URL", "")
	if _, ok := serviceCapabilities()[videocover.Capability]; ok {
		t.Fatal("incomplete asset delivery configuration must not advertise Video Cover")
	}
	t.Setenv("AUTOSTREAM_NODE_CONFIG", writeNodeConfigForTest(t, "encoder_recorder"))
	if got := serviceCapabilities()[videocover.Capability]; got != true {
		t.Fatalf("validated Video Cover capability=%#v", got)
	}
}
