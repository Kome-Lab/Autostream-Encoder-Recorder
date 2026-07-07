package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/observability"
	"github.com/example/autostream-encoder-recorder/internal/version"
)

const ServiceType = "encoder_recorder"
const RuntimeSecretLeaseActiveCode = "runtime_secret_lease_active"

var ErrRuntimeSecretLeaseActive = errors.New(RuntimeSecretLeaseActiveCode)

type ControlPanelError struct {
	Endpoint   string
	StatusCode int
	Code       string
}

func (e ControlPanelError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("control panel %s failed with status %d code %s", e.Endpoint, e.StatusCode, e.Code)
	}
	return fmt.Sprintf("control panel %s failed with status %d", e.Endpoint, e.StatusCode)
}

func (e ControlPanelError) Is(target error) bool {
	return target == ErrRuntimeSecretLeaseActive && e.Code == RuntimeSecretLeaseActiveCode
}

func (e ControlPanelError) ControlPanelCode() string {
	return e.Code
}

type Config struct {
	ControlPanelURL  string
	Token            string
	ServiceID        string
	ServiceName      string
	ServicePublicURL string
	Version          string
	HeartbeatEvery   time.Duration
	ConfigError      string
}

type Client struct {
	Config Config
	HTTP   *http.Client
}

type Registration struct {
	ServiceID    string         `json:"service_id"`
	ServiceType  string         `json:"service_type"`
	ServiceName  string         `json:"service_name"`
	PublicURL    string         `json:"public_url"`
	Version      string         `json:"version"`
	Capabilities map[string]any `json:"capabilities"`
}

type Heartbeat struct {
	ServiceID       string             `json:"service_id"`
	Status          string             `json:"status"`
	CurrentStreamID string             `json:"current_stream_id,omitempty"`
	Version         string             `json:"version,omitempty"`
	Metrics         map[string]float64 `json:"metrics,omitempty"`
}

type Artifact struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	RelativePath string `json:"relative_path"`
	SizeBytes    int64  `json:"size_bytes"`
}

type ArtifactReport struct {
	ServiceID string     `json:"service_id"`
	StreamID  string     `json:"stream_id"`
	Artifacts []Artifact `json:"artifacts"`
}

type RuntimeSecretResolveRequest struct {
	ServiceID        string `json:"service_id"`
	StreamID         string `json:"stream_id,omitempty"`
	ArchiveProfileID string `json:"archive_profile_id,omitempty"`
	SecretName       string `json:"secret_name"`
}

type RuntimeSecretResolveResponse struct {
	SecretName   string `json:"secret_name"`
	Value        string `json:"value"`
	ExpiresInSec int    `json:"expires_in_sec"`
}

type RuntimeConfig struct {
	Service              RegisteredService            `json:"service"`
	Assignments          []StreamServiceAssignment    `json:"assignments"`
	Profiles             map[string][]RuntimeProfile  `json:"profiles"`
	StreamArchiveConfigs []RuntimeArchiveStreamConfig `json:"stream_archive_configs,omitempty"`
	StreamYouTubeConfigs []RuntimeYouTubeStreamConfig `json:"stream_youtube_configs,omitempty"`
}

type RegisteredService struct {
	ServiceID       string         `json:"service_id"`
	ServiceType     string         `json:"service_type"`
	ServiceName     string         `json:"service_name"`
	PublicURL       string         `json:"public_url"`
	Version         string         `json:"version"`
	Status          string         `json:"status"`
	AssignmentRole  string         `json:"assignment_role,omitempty"`
	CurrentStreamID string         `json:"current_stream_id,omitempty"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
}

type StreamServiceAssignment struct {
	StreamID       string    `json:"stream_id"`
	ServiceID      string    `json:"service_id"`
	ServiceType    string    `json:"service_type"`
	AssignmentRole string    `json:"assignment_role"`
	AssignedAt     time.Time `json:"assigned_at"`
}

type RuntimeProfile struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Config    map[string]any `json:"config"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type RuntimeYouTubeStreamConfig struct {
	StreamID         string         `json:"stream_id"`
	AssignmentRole   string         `json:"assignment_role"`
	YouTubeOutputID  string         `json:"youtube_output_id"`
	Ready            bool           `json:"ready"`
	ReadinessCode    string         `json:"readiness_code,omitempty"`
	ReadinessMessage string         `json:"readiness_message,omitempty"`
	YouTubeConfig    map[string]any `json:"youtube_config,omitempty"`
	ActiveRuntime    map[string]any `json:"active_runtime,omitempty"`
}

type RuntimeArchiveStreamConfig struct {
	StreamID         string         `json:"stream_id"`
	AssignmentRole   string         `json:"assignment_role"`
	ArchiveProfileID string         `json:"archive_profile_id"`
	Ready            bool           `json:"ready"`
	ReadinessCode    string         `json:"readiness_code,omitempty"`
	ReadinessMessage string         `json:"readiness_message,omitempty"`
	ArchiveConfig    map[string]any `json:"archive_config,omitempty"`
}

func (cfg RuntimeConfig) YouTubeConfigForStream(streamID string) (RuntimeYouTubeStreamConfig, bool) {
	streamID = strings.TrimSpace(streamID)
	var fallback RuntimeYouTubeStreamConfig
	for _, item := range cfg.StreamYouTubeConfigs {
		if streamID != "" && strings.TrimSpace(item.StreamID) != streamID {
			continue
		}
		if !item.Ready {
			continue
		}
		if strings.TrimSpace(item.AssignmentRole) == "primary" {
			return item, true
		}
		if fallback.StreamID == "" {
			fallback = item
		}
	}
	if fallback.StreamID != "" {
		return fallback, true
	}
	return RuntimeYouTubeStreamConfig{}, false
}

func (cfg RuntimeConfig) ArchiveConfigForStream(streamID string) (RuntimeArchiveStreamConfig, bool) {
	streamID = strings.TrimSpace(streamID)
	var fallback RuntimeArchiveStreamConfig
	for _, item := range cfg.StreamArchiveConfigs {
		if streamID != "" && strings.TrimSpace(item.StreamID) != streamID {
			continue
		}
		if !item.Ready {
			continue
		}
		if strings.TrimSpace(item.AssignmentRole) == "primary" {
			return item, true
		}
		if fallback.StreamID == "" {
			fallback = item
		}
	}
	if fallback.StreamID != "" {
		return fallback, true
	}
	return RuntimeArchiveStreamConfig{}, false
}

func (cfg RuntimeYouTubeStreamConfig) Mode() string {
	return runtimeMapString(cfg.YouTubeConfig, "mode")
}

func (cfg RuntimeYouTubeStreamConfig) RTMPURL() string {
	if value := runtimeMapString(cfg.YouTubeConfig, "rtmp_url"); value != "" {
		return value
	}
	return runtimeMapString(cfg.ActiveRuntime, "rtmp_url")
}

func (cfg RuntimeYouTubeStreamConfig) StreamKeySecretName() string {
	if value := runtimeMapString(cfg.ActiveRuntime, "stream_key_secret_name"); value != "" {
		return value
	}
	return runtimeMapString(cfg.YouTubeConfig, "stream_key_secret_name")
}

func (cfg RuntimeYouTubeStreamConfig) CompleteOnStop() bool {
	if value, ok := runtimeMapBool(cfg.ActiveRuntime, "complete_on_stop"); ok {
		return value
	}
	if value, ok := runtimeMapBool(cfg.YouTubeConfig, "complete_on_stop"); ok {
		return value
	}
	return false
}

func (cfg RuntimeArchiveStreamConfig) DriveDestinationID() string {
	return runtimeMapString(cfg.ArchiveConfig, "drive_destination_id")
}

func (cfg RuntimeArchiveStreamConfig) ArchiveProfileIDValue() string {
	if value := runtimeMapString(cfg.ArchiveConfig, "archive_profile_id"); value != "" {
		return value
	}
	return strings.TrimSpace(cfg.ArchiveProfileID)
}

func (cfg RuntimeArchiveStreamConfig) AuthMode() string {
	return runtimeMapString(cfg.ArchiveConfig, "auth_mode")
}

func (cfg RuntimeArchiveStreamConfig) OAuthAccountID() string {
	return runtimeMapString(cfg.ArchiveConfig, "oauth_account_id")
}

func (cfg RuntimeArchiveStreamConfig) OAuthProviderID() string {
	return runtimeMapString(cfg.ArchiveConfig, "oauth_provider_id")
}

func (cfg RuntimeArchiveStreamConfig) FolderIDSecretName() string {
	return runtimeMapString(cfg.ArchiveConfig, "folder_id_secret_name")
}

func (cfg RuntimeArchiveStreamConfig) ServiceAccountSecretName() string {
	return runtimeMapString(cfg.ArchiveConfig, "service_account_json_secret_name")
}

func (cfg RuntimeArchiveStreamConfig) ServiceAccountCredentialsSecretName() string {
	return runtimeMapString(cfg.ArchiveConfig, "service_account_credentials_secret_name")
}

func (cfg RuntimeArchiveStreamConfig) BasePath() string {
	return runtimeMapString(cfg.ArchiveConfig, "base_path")
}

func (cfg RuntimeArchiveStreamConfig) SharedDrive() (bool, bool) {
	return runtimeMapBool(cfg.ArchiveConfig, "shared_drive")
}

func (cfg RuntimeArchiveStreamConfig) ClientID() string {
	return runtimeMapString(cfg.ArchiveConfig, "client_id")
}

func (cfg RuntimeArchiveStreamConfig) ClientSecretSecretName() string {
	return runtimeMapString(cfg.ArchiveConfig, "client_secret_secret_name")
}

func (cfg RuntimeArchiveStreamConfig) RefreshTokenSecretName() string {
	return runtimeMapString(cfg.ArchiveConfig, "refresh_token_secret_name")
}

func runtimeMapString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, ok := values[key]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return ""
	}
}

func runtimeMapBool(values map[string]any, key string) (bool, bool) {
	if values == nil {
		return false, false
	}
	value, ok := values[key]
	if !ok {
		return false, false
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	default:
		return false, false
	}
}

func ConfigFromEnv() Config {
	cfg := Config{
		ControlPanelURL:  os.Getenv("CONTROL_PANEL_URL"),
		Token:            os.Getenv("CONTROL_PANEL_TOKEN"),
		ServiceID:        envDefault("SERVICE_ID", "encoder-recorder-01"),
		ServiceName:      envDefault("SERVICE_NAME", "Encoder Recorder"),
		ServicePublicURL: os.Getenv("SERVICE_PUBLIC_URL"),
		Version:          envDefault("SERVICE_VERSION", version.Current()),
		HeartbeatEvery:   envDuration("CONTROL_PANEL_HEARTBEAT_INTERVAL_SEC", 30*time.Second),
	}
	applyNodeConfigFromEnv(&cfg, ServiceType)
	return cfg
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.ConfigError) != "" {
		return errors.New(c.ConfigError)
	}
	if strings.TrimSpace(c.ControlPanelURL) == "" {
		return errors.New("CONTROL_PANEL_URL is required")
	}
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("CONTROL_PANEL_TOKEN is required")
	}
	if strings.TrimSpace(c.ServiceID) == "" {
		return errors.New("SERVICE_ID is required")
	}
	if strings.TrimSpace(c.ServiceName) == "" {
		return errors.New("SERVICE_NAME is required")
	}
	if err := validateHTTPURL(c.ControlPanelURL, "CONTROL_PANEL_URL"); err != nil {
		return err
	}
	if err := validateHTTPURL(c.ServicePublicURL, "SERVICE_PUBLIC_URL"); err != nil {
		return err
	}
	return nil
}

func validateHTTPURL(raw, name string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New(name + " must be an absolute URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New(name + " must use http or https")
	}
	if parsed.User != nil {
		return errors.New(name + " must not include credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New(name + " must not include query or fragment")
	}
	if parsed.Scheme == "http" && !isLocalDevHost(parsed.Hostname()) {
		return errors.New(name + " must use https for remote hosts")
	}
	return nil
}

func isLocalDevHost(host string) bool {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	return normalized == "localhost" || normalized == "127.0.0.1" || normalized == "host.docker.internal"
}

func (c Client) Register(ctx context.Context) error {
	body := Registration{
		ServiceID:   c.Config.ServiceID,
		ServiceType: ServiceType,
		ServiceName: c.Config.ServiceName,
		PublicURL:   c.Config.ServicePublicURL,
		Version:     c.Config.Version,
		Capabilities: map[string]any{
			"ffmpeg":             true,
			"record_final_mkv":   true,
			"remux_final_mp4":    true,
			"google_drive_api":   true,
			"archive_retry":      true,
			"audio_ingest_opus":  true,
			"health_endpoint":    true,
			"package_endpoint":   true,
			"default_resolution": "1920x1080",
			"default_fps":        60,
		},
	}
	return c.post(ctx, "/services/register", body)
}

func (c Client) Heartbeat(ctx context.Context, status, currentStreamID string) error {
	return c.HeartbeatWithMetrics(ctx, status, currentStreamID, nil)
}

func (c Client) HeartbeatWithMetrics(ctx context.Context, status, currentStreamID string, metrics map[string]float64) error {
	if status == "" {
		status = "online"
	}
	body := Heartbeat{ServiceID: c.Config.ServiceID, Status: status, CurrentStreamID: currentStreamID, Version: c.Config.Version, Metrics: metrics}
	return c.post(ctx, "/services/heartbeat", body)
}

func (c Client) ReportArtifacts(ctx context.Context, streamID string, artifacts []Artifact) error {
	if strings.TrimSpace(streamID) == "" {
		return errors.New("stream id is required")
	}
	if len(artifacts) == 0 {
		return errors.New("artifacts are required")
	}
	reportCtx, cancel := context.WithTimeout(ctx, envDuration("CONTROL_PANEL_ARTIFACT_REPORT_TIMEOUT_SEC", 5*time.Second))
	defer cancel()
	body := ArtifactReport{ServiceID: c.Config.ServiceID, StreamID: streamID, Artifacts: artifacts}
	return c.post(reportCtx, "/services/stream-artifacts", body)
}

func (c Client) Report(ctx context.Context, signal observability.Signal) error {
	if strings.TrimSpace(signal.Type) == "" || strings.TrimSpace(signal.Name) == "" {
		return errors.New("signal type and name are required")
	}
	if signal.Timestamp.IsZero() {
		signal.Timestamp = time.Now().UTC()
	}
	return c.post(ctx, "/services/observability/signals", signal)
}

func (c Client) RuntimeConfig(ctx context.Context) (RuntimeConfig, error) {
	endpoint := "/services/runtime-config?service_id=" + url.QueryEscape(c.Config.ServiceID)
	var cfg RuntimeConfig
	if err := c.get(ctx, endpoint, &cfg); err != nil {
		return RuntimeConfig{}, err
	}
	return cfg, nil
}

func (c Client) ResolveRuntimeSecret(ctx context.Context, streamID, archiveProfileID, secretName string) (string, error) {
	if strings.TrimSpace(secretName) == "" {
		return "", errors.New("secret name is required")
	}
	var out RuntimeSecretResolveResponse
	err := c.postJSON(ctx, "/services/runtime-secrets/resolve", RuntimeSecretResolveRequest{
		ServiceID:        c.Config.ServiceID,
		StreamID:         strings.TrimSpace(streamID),
		ArchiveProfileID: strings.TrimSpace(archiveProfileID),
		SecretName:       strings.TrimSpace(secretName),
	}, &out)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Value) == "" {
		return "", errors.New("runtime secret value is empty")
	}
	return out.Value, nil
}

func (c Client) RunHeartbeatLoop(ctx context.Context, currentStreamID func() string, onError func(error)) {
	c.RunHeartbeatLoopWithMetrics(ctx, currentStreamID, nil, onError)
}

func (c Client) RunHeartbeatLoopWithMetrics(ctx context.Context, currentStreamID func() string, metrics func() map[string]float64, onError func(error)) {
	if currentStreamID == nil {
		currentStreamID = func() string { return "" }
	}
	if metrics == nil {
		metrics = func() map[string]float64 { return nil }
	}
	interval := c.Config.HeartbeatEvery
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := c.HeartbeatWithMetrics(ctx, "online", currentStreamID(), metrics()); err != nil && onError != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c Client) get(ctx context.Context, endpoint string, out any) error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(c.Config.ControlPanelURL, endpoint), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Config.Token)
	client := c.HTTP
	if client == nil {
		client = noRedirectClient()
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return controlPanelErrorFromResponse(endpoint, res)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (c Client) post(ctx context.Context, endpoint string, payload any) error {
	return c.postJSON(ctx, endpoint, payload, nil)
}

func (c Client) postJSON(ctx context.Context, endpoint string, payload any, out any) error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(c.Config.ControlPanelURL, endpoint), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Config.Token)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = noRedirectClient()
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return controlPanelErrorFromResponse(endpoint, res)
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

func controlPanelErrorFromResponse(endpoint string, res *http.Response) error {
	out := ControlPanelError{Endpoint: endpoint, StatusCode: res.StatusCode}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&body); err == nil {
		out.Code = strings.TrimSpace(body.Code)
	}
	return out
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func joinURL(baseURL, endpoint string) string {
	base := strings.TrimRight(baseURL, "/")
	path := "/" + strings.TrimLeft(endpoint, "/")
	return base + path
}

func envDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	seconds, err := time.ParseDuration(value + "s")
	if err != nil || seconds <= 0 {
		return fallback
	}
	return seconds
}
