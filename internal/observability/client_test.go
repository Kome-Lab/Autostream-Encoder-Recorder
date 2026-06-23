package observability

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReportPostsSignal(t *testing.T) {
	var gotAuth string
	var got Signal
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/signals" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := Client{Config: Config{URL: server.URL, Token: "secret-token", ServiceID: "enc-01", ServiceName: "Encoder 01", Timeout: time.Second}}
	if err := client.Event(t.Context(), "stream-01", "encoder.process.started", "running", map[string]any{"pid": 1234}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("unexpected auth header: %s", gotAuth)
	}
	if got.Type != "event" || got.Name != "encoder.process.started" || got.ServiceType != serviceType || got.StreamID != "stream-01" {
		t.Fatalf("unexpected signal: %#v", got)
	}
}

func TestReportErrorDoesNotLeakTokenOrResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret-token", http.StatusForbidden)
	}))
	defer server.Close()

	client := Client{Config: Config{URL: server.URL, Token: "secret-token", ServiceID: "enc-01", Timeout: time.Second}}
	err := client.Event(t.Context(), "stream-01", "encoder.process.started", "running", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestReportDoesNotFollowRedirectsWithBearerToken(t *testing.T) {
	var redirectedAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusFound)
	}))
	defer server.Close()

	client := Client{Config: Config{URL: server.URL, Token: "secret-token", ServiceID: "enc-01", Timeout: time.Second}}
	err := client.Report(t.Context(), Signal{Type: "metric", StreamID: "stream-01", Name: "encoder.process_alive", Status: "ok"})
	if err == nil {
		t.Fatal("expected redirect response to fail")
	}
	if redirectedAuth != "" {
		t.Fatalf("authorization header followed redirect: %q", redirectedAuth)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("OBSERVABILITY_URL", "https://observability.example.com")
	t.Setenv("OBSERVABILITY_TOKEN", "<SERVICE_TOKEN>")
	t.Setenv("SERVICE_ID", "enc-01")
	t.Setenv("SERVICE_NAME", "Encoder 01")
	t.Setenv("OBSERVABILITY_TIMEOUT_SEC", "3")

	cfg := ConfigFromEnv()
	if err := (Client{Config: cfg}).Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 3*time.Second || cfg.ServiceName != "Encoder 01" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestValidateRejectsNonHTTPObservabilityURL(t *testing.T) {
	client := Client{Config: Config{URL: "ftp://observability.example.com/signals", Token: "<SERVICE_TOKEN>", ServiceID: "enc-01", Timeout: time.Second}}
	err := client.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsRemoteHTTPObservabilityURL(t *testing.T) {
	client := Client{Config: Config{URL: "http://observability.example.com", Token: "<SERVICE_TOKEN>", ServiceID: "enc-01", Timeout: time.Second}}
	err := client.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "OBSERVABILITY_URL") || !strings.Contains(err.Error(), "https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAllowsLocalHTTPObservabilityURL(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:8082", "http://localhost:8082", "http://host.docker.internal:8082"} {
		client := Client{Config: Config{URL: raw, Token: "<SERVICE_TOKEN>", ServiceID: "enc-01", Timeout: time.Second}}
		if err := client.Validate(); err != nil {
			t.Fatalf("local URL %s rejected: %v", raw, err)
		}
	}
}

func TestValidateRejectsObservabilityURLQueryOrFragment(t *testing.T) {
	client := Client{Config: Config{URL: "https://observability.example.com#frag", Token: "<SERVICE_TOKEN>", ServiceID: "enc-01", Timeout: time.Second}}
	err := client.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "query or fragment") {
		t.Fatalf("unexpected error: %v", err)
	}
}
