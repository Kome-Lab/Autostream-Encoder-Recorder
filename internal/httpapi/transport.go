package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const (
	maxControlBodyBytes             = 64 * 1024
	maxWorkerEventBodyBytes         = 256 * 1024
	defaultDiscordAudioBodyBytes    = 512 * 1024
	defaultDiscordAudioMaxPackets   = 100
	defaultDiscordAudioMaxOpusBytes = 4096
)

func decodeLimitedJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) (int, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		return limitedJSONStatus(err), err
	}
	if err := decoder.Decode(&struct{}{}); errors.Is(err, io.EOF) {
		return http.StatusOK, nil
	} else {
		return limitedJSONStatus(err), err
	}
}

func decodeLimitedStrictJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) (int, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return limitedJSONStatus(err), err
	}
	if err := decoder.Decode(&struct{}{}); errors.Is(err, io.EOF) {
		return http.StatusOK, nil
	} else {
		if err == nil {
			return http.StatusBadRequest, errors.New("unexpected trailing JSON")
		}
		return limitedJSONStatus(err), err
	}
}

func limitedJSONStatus(err error) int {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func limitedJSONErrorCode(status int) string {
	if status == http.StatusRequestEntityTooLarge {
		return "request_body_too_large"
	}
	return "bad_request"
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func boolMetric(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/updater/version" {
			w.Header().Set("Cache-Control", "no-store")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
