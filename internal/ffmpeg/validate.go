package ffmpeg

import (
	"context"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/outputrelay"
	"github.com/example/autostream-encoder-recorder/internal/videoingest"
)

var ErrUnsafeOutputTarget = errors.New("unsafe ffmpeg output target")
var ErrUnsafeInputTarget = errors.New("unsafe ffmpeg input target")

type HostResolver func(ctx context.Context, host string) ([]net.IP, error)

type RuntimeInputPolicy struct {
	AllowDirectHLS      bool
	AllowHostnameInputs bool
	RequireAllowedHosts bool
}

func ValidateOutputTarget(rtmpURL, streamKey string) error {
	rtmpURL = strings.TrimSpace(rtmpURL)
	streamKey = strings.TrimSpace(streamKey)
	if rtmpURL == "" || streamKey == "" {
		return ErrUnsafeOutputTarget
	}
	if strings.ContainsAny(rtmpURL, "|[]\r\n") || strings.ContainsAny(streamKey, "|[]\r\n") {
		return ErrUnsafeOutputTarget
	}
	parsed, err := url.Parse(rtmpURL)
	if err != nil {
		return ErrUnsafeOutputTarget
	}
	if parsed.Scheme != "rtmps" {
		return ErrUnsafeOutputTarget
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ErrUnsafeOutputTarget
	}
	if strings.ContainsAny(streamKey, `/\?# `) {
		return ErrUnsafeOutputTarget
	}
	return nil
}

func ValidateRelayOutputTarget(outputTarget string) error {
	if err := outputrelay.ValidateRelayTarget(outputTarget); err != nil {
		return ErrUnsafeOutputTarget
	}
	return nil
}

func ValidateInputTarget(inputURL string) error {
	inputURL = strings.TrimSpace(inputURL)
	if inputURL == "" {
		return ErrUnsafeInputTarget
	}
	if strings.ContainsAny(inputURL, "\r\n") {
		return ErrUnsafeInputTarget
	}
	if strings.HasPrefix(inputURL, "internal_discord_audio:") {
		path := strings.TrimSpace(strings.TrimPrefix(inputURL, "internal_discord_audio:"))
		if path == "" || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#") || strings.Contains(path, "://") {
			return ErrUnsafeInputTarget
		}
		if !strings.HasSuffix(strings.ToLower(path), ".sdp") {
			return ErrUnsafeInputTarget
		}
		return nil
	}
	if _, ok := videoingest.ResolveInputTarget(inputURL); ok {
		return nil
	}
	if strings.HasPrefix(inputURL, "internal_worker_video:") {
		return ErrUnsafeInputTarget
	}
	parsed, err := url.Parse(inputURL)
	if err != nil {
		return ErrUnsafeInputTarget
	}
	if parsed.Scheme == "" {
		return ErrUnsafeInputTarget
	}
	switch parsed.Scheme {
	case "rtsp", "rtmp", "rtmps", "srt", "udp", "rtp":
		if parsed.Host == "" || parsed.Fragment != "" {
			return ErrUnsafeInputTarget
		}
		if unsafeNetworkHost(parsed.Scheme, parsed.Hostname()) {
			return ErrUnsafeInputTarget
		}
		return nil
	case "http", "https":
		if parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return ErrUnsafeInputTarget
		}
		if unsafeNetworkHost(parsed.Scheme, parsed.Hostname()) {
			return ErrUnsafeInputTarget
		}
		path := strings.ToLower(parsed.Path)
		if strings.HasSuffix(path, ".m3u8") && parsed.RawQuery == "" {
			return nil
		}
		query := parsed.Query()
		if len(query) == 1 && strings.EqualFold(query.Get("format"), "m3u8") && hasOnlyQueryKey(query, "format") {
			return nil
		}
		return ErrUnsafeInputTarget
	default:
		return ErrUnsafeInputTarget
	}
}

func ValidateInputTargetWithAllowedHosts(inputURL string, allowedHosts []string) error {
	if err := ValidateInputTarget(inputURL); err != nil {
		return err
	}
	allowedHosts = normalizeHostPatterns(allowedHosts)
	if len(allowedHosts) == 0 || strings.HasPrefix(strings.TrimSpace(inputURL), "internal_discord_audio:") || strings.HasPrefix(strings.TrimSpace(inputURL), "internal_worker_video:") {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(inputURL))
	if err != nil {
		return ErrUnsafeInputTarget
	}
	if !hostMatchesAllowedPatterns(parsed.Hostname(), allowedHosts) {
		return ErrUnsafeInputTarget
	}
	return nil
}

func ValidateInputTargetForRuntime(inputURL string, allowedHosts []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return ValidateInputTargetWithResolver(ctx, inputURL, allowedHosts, defaultResolveHost)
}

func ValidateInputTargetWithResolver(ctx context.Context, inputURL string, allowedHosts []string, resolver HostResolver) error {
	return ValidateInputTargetWithRuntimePolicy(ctx, inputURL, allowedHosts, resolver, RuntimeInputPolicy{AllowDirectHLS: true, AllowHostnameInputs: true})
}

func ValidateInputTargetWithRuntimePolicy(ctx context.Context, inputURL string, allowedHosts []string, resolver HostResolver, policy RuntimeInputPolicy) error {
	if err := ValidateInputTargetWithAllowedHosts(inputURL, allowedHosts); err != nil {
		return err
	}
	inputURL = strings.TrimSpace(inputURL)
	if strings.HasPrefix(inputURL, "internal_discord_audio:") || strings.HasPrefix(inputURL, "internal_worker_video:") {
		return nil
	}
	if policy.RequireAllowedHosts && len(normalizeHostPatterns(allowedHosts)) == 0 {
		return ErrUnsafeInputTarget
	}
	parsed, err := url.Parse(inputURL)
	if err != nil {
		return ErrUnsafeInputTarget
	}
	if (parsed.Scheme == "http" || parsed.Scheme == "https") && !policy.AllowDirectHLS {
		return ErrUnsafeInputTarget
	}
	host := normalizeHost(parsed.Hostname())
	if host == "" {
		return ErrUnsafeInputTarget
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	if !policy.AllowHostnameInputs {
		return ErrUnsafeInputTarget
	}
	if resolver == nil {
		resolver = defaultResolveHost
	}
	ips, err := resolver(ctx, host)
	if err != nil || len(ips) == 0 {
		return ErrUnsafeInputTarget
	}
	for _, ip := range ips {
		if unsafeNetworkIP(parsed.Scheme, ip) {
			return ErrUnsafeInputTarget
		}
	}
	return nil
}

func unsafeNetworkHost(scheme, host string) bool {
	host = normalizeHost(host)
	if host == "" {
		return true
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || host == "metadata.google.internal" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return unsafeNetworkIP(scheme, ip)
	}
	return false
}

func unsafeNetworkIP(scheme string, ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	if ip.IsMulticast() {
		return scheme != "udp" && scheme != "rtp"
	}
	return false
}

func defaultResolveHost(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.IP)
	}
	return out, nil
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func normalizeHostPatterns(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = normalizeHost(pattern)
		if pattern != "" {
			out = append(out, pattern)
		}
	}
	return out
}

func hostMatchesAllowedPatterns(host string, patterns []string) bool {
	host = normalizeHost(host)
	if host == "" {
		return false
	}
	for _, pattern := range patterns {
		if pattern == host {
			return true
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
				return true
			}
		}
	}
	return false
}

func hasOnlyQueryKey(query url.Values, key string) bool {
	for candidate := range query {
		if !strings.EqualFold(candidate, key) {
			return false
		}
	}
	return true
}

func ResolveInputTarget(inputURL string) string {
	inputURL = strings.TrimSpace(inputURL)
	if strings.HasPrefix(inputURL, "internal_discord_audio:") {
		return filepath.Clean(strings.TrimPrefix(inputURL, "internal_discord_audio:"))
	}
	if target, ok := videoingest.ResolveInputTarget(inputURL); ok {
		return target
	}
	return inputURL
}
