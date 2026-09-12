package httpapi

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/audioingest"
	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/version"
	"github.com/example/autostream-encoder-recorder/internal/videoingest"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
)

type Status struct {
	ServiceType string    `json:"service_type"`
	ServiceID   string    `json:"service_id"`
	Status      string    `json:"status"`
	CheckedAt   time.Time `json:"checked_at"`
}

type updaterVersionResponse struct {
	Version        string `json:"version"`
	ServiceID      string `json:"service_id"`
	ServiceType    string `json:"service_type"`
	ConfigRevision int64  `json:"config_revision"`
}

func NewServer(serviceType string) http.Handler {
	return NewServerWithProcessManager(serviceType, streamproc.NewManagerFromEnv())
}

func NewServerWithProcessManager(serviceType string, processManager *streamproc.Manager) http.Handler {
	archiveRoot := os.Getenv("AUTOSTREAM_ARCHIVE_DIR")
	if archiveRoot == "" && processManager != nil {
		archiveRoot = processManager.ArchiveRoot
	}
	if archiveRoot == "" {
		archiveRoot = "/var/lib/autostream/archives"
	}
	return NewServerWithManagers(serviceType, processManager, workerevents.NewManager(archiveRoot), TokenVerifierFromEnv())
}

func NewServerWithManagers(serviceType string, processManager *streamproc.Manager, eventManager *workerevents.Manager, verifier TokenVerifier) http.Handler {
	return NewServerWithManagersAndSecretResolver(serviceType, processManager, eventManager, verifier, nil)
}

func NewServerWithManagersAndSecretResolver(serviceType string, processManager *streamproc.Manager, eventManager *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver) http.Handler {
	return NewServerWithManagersAndRuntimeConfig(serviceType, processManager, eventManager, verifier, resolver, nil)
}

func NewServerWithManagersAndRuntimeConfig(serviceType string, processManager *streamproc.Manager, eventManager *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, runtimeConfig RuntimeConfigProvider) http.Handler {
	return NewServerWithManagersAndRuntimeConfigAndUpdaterIdentity(serviceType, processManager, eventManager, verifier, resolver, runtimeConfig, NewUpdaterIdentityLatch(serviceType))
}

func NewServerWithManagersAndRuntimeConfigAndUpdaterIdentity(serviceType string, processManager *streamproc.Manager, eventManager *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, runtimeConfig RuntimeConfigProvider, updaterIdentity *UpdaterIdentityLatch) http.Handler {
	return newServerWithManagersAndRuntimeConfigAndUpdaterIdentity(serviceType, processManager, eventManager, verifier, resolver, runtimeConfig, updaterIdentity, videoingest.NewManagerFromEnv())
}

func newServerWithManagersAndRuntimeConfigAndUpdaterIdentity(serviceType string, processManager *streamproc.Manager, eventManager *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, runtimeConfig RuntimeConfigProvider, updaterIdentity *UpdaterIdentityLatch, videoManager *videoingest.Manager) http.Handler {
	if updaterIdentity == nil {
		panic("encoder recorder updater identity latch is required")
	}
	if _, err := updaterIdentity.ResolveFromEnv(); err != nil && !errors.Is(err, ErrUpdaterIdentityPending) {
		panic(err)
	}
	processArchiveRoot := "/var/lib/autostream/archives"
	if processManager != nil && strings.TrimSpace(processManager.ArchiveRoot) != "" {
		processArchiveRoot = processManager.ArchiveRoot
	}
	if eventManager == nil {
		eventManager = workerevents.NewManager(processArchiveRoot)
	}
	eventArchiveRoot := processArchiveRoot
	if strings.TrimSpace(eventManager.ArchiveRoot) != "" {
		eventArchiveRoot = eventManager.ArchiveRoot
	}
	audioManager := audioingest.NewManager(eventArchiveRoot)
	audioManager.MaxPackets = envInt("AUDIO_INGEST_MAX_PACKETS", defaultDiscordAudioMaxPackets)
	audioManager.MaxOpusSize = envInt("AUDIO_INGEST_MAX_OPUS_BYTES", defaultDiscordAudioMaxOpusBytes)
	if processManager != nil {
		previousProcessExitHook := processManager.ProcessExitHook
		processManager.ProcessExitHook = func(streamID string) {
			if previousProcessExitHook != nil {
				previousProcessExitHook(streamID)
			}
			audioManager.StopBridge(streamID)
			if videoManager != nil {
				videoManager.StopBridge(streamID)
			}
		}
	}
	if processManager != nil && videoManager != nil && videoManager.Reporter == nil {
		videoManager.Reporter = processManager.Reporter
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /updater/version", func(w http.ResponseWriter, r *http.Request) {
		identity, err := updaterIdentity.ResolveFromEnv()
		if err != nil {
			code := "updater_identity_invalid"
			if errors.Is(err, ErrUpdaterIdentityPending) {
				code = "updater_identity_pending"
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": code})
			return
		}
		writeJSON(w, http.StatusOK, updaterVersionResponse{
			Version:        version.Current(),
			ServiceID:      identity.ServiceID,
			ServiceType:    identity.ServiceType,
			ConfigRevision: identity.ConfigRevision,
		})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Status{ServiceType: serviceType, ServiceID: control.ConfigFromEnv().ServiceID, Status: "ready", CheckedAt: time.Now().UTC()})
	})
	mux.HandleFunc("GET /preflight", servicePreflight(verifier))
	mux.HandleFunc("POST /heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	})
	mux.HandleFunc("POST /streams/dry-run", dryRunStream(verifier, runtimeConfig))
	mux.HandleFunc("POST /streams/start", startStream(processManager, audioManager, videoManager, verifier, resolver, runtimeConfig))
	mux.HandleFunc("PUT /streams/{id}/runtime-settings", updateStreamRuntimeSettings(processManager, verifier, runtimeConfig))
	mux.HandleFunc("GET /streams/{id}/video-cover-state", getVideoCoverState(processManager, verifier))
	mux.HandleFunc("PUT /streams/{id}/video-cover-state", putVideoCoverState(processManager, verifier))
	mux.HandleFunc("POST /streams/{id}/stop", stopStream(processManager, audioManager, videoManager, verifier))
	mux.HandleFunc("GET /streams/{id}/process-status", streamProcessStatus(processManager, verifier))
	mux.HandleFunc("GET /streams/{id}/preview/{name}", streamPreview(processArchiveRoot, verifier))
	mux.HandleFunc("GET /streams/{id}/audio-status", discordAudioStatus(audioManager, verifier))
	mux.HandleFunc("POST /streams/package", packageStream(verifier, resolver, runtimeConfig))
	mux.HandleFunc("GET /streams/{id}/archive-runs/{run_id}/artifacts/{name}", downloadArchiveArtifact(eventManager.ArchiveRoot, verifier))
	mux.HandleFunc("DELETE /streams/{id}/archive-runs/{run_id}/artifacts/{name}", deleteArchiveArtifact(eventManager.ArchiveRoot, verifier))
	mux.HandleFunc("PUT /streams/{id}/archive-runs/{run_id}/artifacts/{name}", renameArchiveArtifact(eventManager.ArchiveRoot, verifier))
	mux.HandleFunc("POST /worker-events", workerEvents(eventManager, processManager, verifier))
	mux.HandleFunc("GET /streams/{id}/worker-events", recentWorkerEvents(eventManager, verifier))
	mux.HandleFunc("POST /streams/{id}/audio/opus", discordOpusAudio(audioManager, processManager, verifier))
	return securityHeaders(mux)
}
