package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/control"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/httpapi"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "configure" {
		if err := control.RunConfigureCommand(os.Args[2:], control.ServiceType, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "configure failed: %v\n", err)
			os.Exit(2)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("AUTOSTREAM_BIND_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	processManager := streamproc.NewManagerFromEnv()
	controlClient := control.Client{Config: control.ConfigFromEnv()}
	var runtimeConfigProvider httpapi.RuntimeConfigProvider
	if controlClient.Config.ControlPanelURL != "" && controlClient.Config.Token != "" {
		if err := controlClient.Register(ctx); err != nil {
			if requireControlPanelRuntimeConfig() {
				log.Fatalf("control panel registration is required in this environment: %v", err)
			}
			log.Printf("control panel registration failed: %v", err)
		} else {
			log.Printf("registered with control panel as %s", controlClient.Config.ServiceID)
			if cfg, ok := logRuntimeConfig(ctx, controlClient); ok {
				if profile, applied := encoderProfileFromRuntimeConfig(cfg); applied {
					processManager.Profile = profile
					log.Printf("applied control panel encoder profile for %s: %dx%d %dfps", controlClient.Config.ServiceID, profile.Width, profile.Height, profile.FPS)
				}
			} else if requireControlPanelRuntimeConfig() {
				log.Fatal("control panel runtime config is required in this environment")
			}
		}
		runtimeConfigProvider = controlClient.RuntimeConfig
		go controlClient.RunHeartbeatLoopWithMetrics(ctx, processManager.CurrentStreamID, processManager.HeartbeatMetrics, func(err error) {
			log.Printf("control panel heartbeat failed: %v", err)
		})
	} else if requireControlPanelRuntimeConfig() {
		log.Fatal("CONTROL_PANEL_URL and CONTROL_PANEL_TOKEN are required in this environment")
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           httpapi.NewServerWithManagersAndRuntimeConfig("encoder_recorder", processManager, nil, httpapi.TokenVerifierFromEnv(), controlClient.ResolveRuntimeSecret, runtimeConfigProvider),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("autostream-encoder-recorder listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case err := <-errCh:
		if err != nil {
			log.Fatal(err)
		}
	case <-ctx.Done():
		log.Printf("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown failed: %v", err)
			if closeErr := server.Close(); closeErr != nil {
				log.Printf("http close failed: %v", closeErr)
			}
		}
		if errs := processManager.StopAllAndDrain(shutdownCtx); len(errs) > 0 {
			for _, err := range errs {
				log.Printf("stream process shutdown failed: %v", err)
			}
		}
	}
}

func logRuntimeConfig(ctx context.Context, client control.Client) (control.RuntimeConfig, bool) {
	cfg, err := client.RuntimeConfig(ctx)
	if err != nil {
		log.Printf("control panel runtime config fetch failed: %v", err)
		return control.RuntimeConfig{}, false
	}
	profileCount := 0
	for _, profiles := range cfg.Profiles {
		profileCount += len(profiles)
	}
	log.Printf("loaded control panel runtime config for %s: assignments=%d profiles=%d youtube_configs=%d", cfg.Service.ServiceID, len(cfg.Assignments), profileCount, len(cfg.StreamYouTubeConfigs))
	return cfg, true
}

func encoderProfileFromRuntimeConfig(cfg control.RuntimeConfig) (ffmpeg.EncoderProfile, bool) {
	if profile, ok := firstRuntimeProfileForService(cfg.Profiles["encoder"], cfg.Service.ServiceID); ok {
		return encoderProfileFromConfig(profile.Config), true
	}
	return ffmpeg.EncoderProfile{}, false
}

func firstRuntimeProfileForService(profiles []control.RuntimeProfile, serviceID string) (control.RuntimeProfile, bool) {
	for _, profile := range profiles {
		if profileBelongsToService(profile, serviceID) {
			return profile, true
		}
	}
	return control.RuntimeProfile{}, false
}

func profileBelongsToService(profile control.RuntimeProfile, serviceID string) bool {
	rawServiceID, ok := profile.Config["service_id"]
	if !ok {
		return true
	}
	profileServiceID, ok := rawServiceID.(string)
	if !ok {
		return false
	}
	profileServiceID = strings.TrimSpace(profileServiceID)
	return profileServiceID == "" || profileServiceID == strings.TrimSpace(serviceID)
}

func requireControlPanelRuntimeConfig() bool {
	if envBool("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", false) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("AUTOSTREAM_ENV")), "production")
}

func envBool(key string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if raw == "" {
		return fallback
	}
	return raw == "1" || raw == "true" || raw == "yes" || raw == "on"
}

func encoderProfileFromConfig(config map[string]any) ffmpeg.EncoderProfile {
	profile := ffmpeg.DefaultProfile()
	if width := intConfig(config, "width"); width > 0 {
		profile.Width = width
	}
	if height := intConfig(config, "height"); height > 0 {
		profile.Height = height
	}
	if fps := intConfig(config, "fps"); fps > 0 {
		profile.FPS = fps
	}
	if kbps := intConfig(config, "video_bitrate_kbps"); kbps > 0 {
		profile.VideoBitrate = fmt.Sprintf("%dk", kbps)
	}
	if kbps := intConfig(config, "audio_bitrate_kbps"); kbps > 0 {
		profile.AudioBitrate = fmt.Sprintf("%dk", kbps)
	}
	if sampleRate := intConfig(config, "audio_sample_rate_hz"); sampleRate > 0 {
		profile.SampleRate = sampleRate
	}
	if keyframe := intConfig(config, "keyframe_interval_sec"); keyframe > 0 {
		profile.KeyframeSec = keyframe
	}
	return profile
}

func intConfig(config map[string]any, key string) int {
	switch value := config[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}
