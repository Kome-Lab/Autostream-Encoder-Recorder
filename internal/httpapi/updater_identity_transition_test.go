package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

func TestUpdaterVersionPendingThenBindsAuthoritativeIdentityAndRejectsDrift(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv("AUTOSTREAM_NODE_CONFIG", configPath)
	t.Setenv("SERVICE_ID", "placeholder-encoder")
	t.Setenv("AUTOSTREAM_CONFIG_REVISION", "13")

	latch := NewUpdaterIdentityLatch(control.ServiceType)
	handler := NewServerWithManagersAndRuntimeConfigAndUpdaterIdentity(
		control.ServiceType,
		nil,
		workerevents.NewManager(t.TempDir()),
		TokenVerifier{},
		nil,
		nil,
		latch,
	)
	assertUpdaterIdentityStatus(t, handler, http.StatusServiceUnavailable, "")

	writeUpdaterIdentityNodeConfig(t, configPath, "encoder-authoritative", control.ServiceType)
	identity, err := latch.ResolveFromEnv()
	if err != nil {
		t.Fatalf("registration identity resolve failed: %v", err)
	}
	if identity.ServiceID != "encoder-authoritative" {
		t.Fatalf("registration identity = %q, want encoder-authoritative", identity.ServiceID)
	}
	assertUpdaterIdentityStatus(t, handler, http.StatusOK, "encoder-authoritative")

	t.Setenv("AUTOSTREAM_CONFIG_REVISION", "14")
	assertUpdaterIdentityStatus(t, handler, http.StatusServiceUnavailable, "")
	t.Setenv("AUTOSTREAM_CONFIG_REVISION", "13")

	writeUpdaterIdentityNodeConfig(t, configPath, "encoder-drifted", control.ServiceType)
	assertUpdaterIdentityStatus(t, handler, http.StatusServiceUnavailable, "")
}

func assertUpdaterIdentityStatus(t *testing.T, handler http.Handler, wantStatus int, wantServiceID string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/updater/version", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != wantStatus {
		t.Fatalf("status = %d body = %s, want %d", res.Code, res.Body.String(), wantStatus)
	}
	if wantServiceID != "" && !strings.Contains(res.Body.String(), `"service_id":"`+wantServiceID+`"`) {
		t.Fatalf("response does not contain authoritative service id %q: %s", wantServiceID, res.Body.String())
	}
	if wantServiceID == "" && strings.Contains(res.Body.String(), "service_id") {
		t.Fatalf("unavailable response leaked a service identity: %s", res.Body.String())
	}
}

func writeUpdaterIdentityNodeConfig(t *testing.T, path, serviceID, serviceType string) {
	t.Helper()
	body := fmt.Sprintf(`panel:
  url: "https://panel.example.com"
node:
  id: %q
  name: "Updater Probe"
  type: %q
api:
  host: "127.0.0.1"
  port: 8081
  ssl_enabled: false
auth:
  token: "runtime-token"
stream_ingest:
  signing_key: "signing-key"
`, serviceID, serviceType)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
