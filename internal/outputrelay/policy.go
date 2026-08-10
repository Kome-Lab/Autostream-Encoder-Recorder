// Package outputrelay defines the non-secret local Relay route selected by
// Encoder/Recorder.  It deliberately has no Control Panel dependency so every
// ingress path can make the same decision before resolving runtime secrets.
package outputrelay

import (
	"errors"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const (
	ModeDirect          = "direct"
	ModeLegacyStreamKey = "legacy_stream_key"
	ModeLiveAPIStatic   = "live_api_static"

	composeOutputRelayEnv  = "AUTOSTREAM_COMPOSE_OUTPUT_RELAY"
	composeOutputRelayHost = "output-relay"
	composeOutputRelayPort = "1935"
)

var (
	ErrInvalidConfiguration              = errors.New("invalid output relay configuration")
	ErrRelayRequired                     = errors.New("output relay URL is required")
	ErrUnsafeRelayTarget                 = errors.New("unsafe output relay target")
	ErrStaticBindingRequired             = errors.New("static output relay binding is required")
	ErrInvalidRelayBindingID             = errors.New("invalid output relay binding id")
	ErrLiveAPIRequiresManagedOutputRelay = errors.New("live_api_requires_managed_output_relay")
	ErrLiveAPIRelayBindingMismatch       = errors.New("live_api_relay_binding_mismatch")
	ErrLiveAPIRelayStaticNotReady        = errors.New("live_api_relay_static_not_ready")
)

var relayBindingIDPattern = regexp.MustCompile(`^relay-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Policy contains only non-secret Relay configuration.  URL is intentionally
// not emitted in a service capability or other public status record.
type Policy struct {
	URL       string
	Mode      string
	BindingID string
	// RequireRelay makes URL-free direct output invalid. It is carried in the
	// shared policy so capability reporting and every ingress path fail closed
	// before Control Panel can prepare an incompatible dynamic broadcast.
	RequireRelay bool

	configuredMode string
}

// New interprets an unset mode as the pre-existing fixed stream-key Relay when
// a Relay URL is present. A URL-free process is direct-output compatible when
// a Relay is not required.
func New(rawURL, rawMode, rawBindingID string) Policy {
	return NewWithRequireRelay(rawURL, rawMode, rawBindingID, false)
}

// NewWithRequireRelay constructs the shared policy with an explicit Relay
// requirement. Keep the requirement separate from the URL so an absent URL
// cannot be advertised as a valid direct-output capability by accident.
func NewWithRequireRelay(rawURL, rawMode, rawBindingID string, requireRelay bool) Policy {
	p := Policy{
		URL:            strings.TrimSpace(rawURL),
		BindingID:      rawBindingID,
		RequireRelay:   requireRelay,
		configuredMode: strings.ToLower(strings.TrimSpace(rawMode)),
	}
	if p.URL == "" {
		p.Mode = ModeDirect
		return p
	}
	if p.configuredMode == "" {
		p.Mode = ModeLegacyStreamKey
		return p
	}
	p.Mode = p.configuredMode
	return p
}

func FromEnv() Policy {
	return NewWithRequireRelay(
		os.Getenv("AUTOSTREAM_OUTPUT_RELAY_URL"),
		os.Getenv("AUTOSTREAM_OUTPUT_RELAY_MODE"),
		os.Getenv("AUTOSTREAM_OUTPUT_RELAY_BINDING_ID"),
		RequireRelayFromEnv(),
	)
}

// RequireRelayFromEnv shares the explicit Relay requirement decision between
// capability reporting, HTTP preflight/dry-run, and stream process startup.
// Direct YouTube Live API output is the normal path; a Relay is opt-in.
func RequireRelayFromEnv() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("AUTOSTREAM_REQUIRE_OUTPUT_RELAY")))
	if raw != "" {
		return raw == "1" || raw == "true" || raw == "yes" || raw == "on"
	}
	return false
}

func (p Policy) ValidateConfiguration() error {
	if p.URL == "" {
		if p.configuredMode != "" && p.configuredMode != ModeDirect {
			return ErrInvalidConfiguration
		}
		if p.RequireRelay {
			return ErrRelayRequired
		}
		return nil
	}

	switch p.Mode {
	case ModeLegacyStreamKey:
	case ModeLiveAPIStatic:
		if p.BindingID == "" {
			return ErrStaticBindingRequired
		}
		if !ValidRelayBindingID(p.BindingID) {
			return ErrInvalidRelayBindingID
		}
	default:
		// A Relay URL must never be silently ignored by selecting direct mode.
		return ErrInvalidConfiguration
	}
	if err := ValidateRelayURLTemplate(p.URL); err != nil {
		return ErrUnsafeRelayTarget
	}
	return nil
}

// CapabilityMode returns a canonical public value only for a valid local
// configuration. Invalid configuration must not masquerade as direct output:
// the Control Panel treats an omitted mode as unknown and fails closed.
func (p Policy) CapabilityMode() (string, bool) {
	if p.ValidateConfiguration() != nil {
		return "", false
	}
	return p.Mode, true
}

func (p Policy) UsesLocalRelay() bool {
	return p.URL != "" && p.ValidateConfiguration() == nil
}

// AuthorizeYouTubeOutput returns true only when FFmpeg must use the configured
// local Relay target.  It performs no network, secret, or input resolution.
func (p Policy) AuthorizeYouTubeOutput(youtubeMode string, ready bool, bindingID string) (bool, error) {
	if err := p.ValidateConfiguration(); err != nil {
		return false, err
	}

	switch p.Mode {
	case ModeDirect:
		return false, nil
	case ModeLegacyStreamKey:
		if strings.ToLower(strings.TrimSpace(youtubeMode)) != "stream_key" {
			return false, ErrLiveAPIRequiresManagedOutputRelay
		}
		return true, nil
	case ModeLiveAPIStatic:
		if strings.ToLower(strings.TrimSpace(youtubeMode)) != "live_api_relay_static" {
			return false, ErrLiveAPIRequiresManagedOutputRelay
		}
		if !ready {
			return false, ErrLiveAPIRelayStaticNotReady
		}
		if !ValidRelayBindingID(bindingID) || p.BindingID != bindingID {
			return false, ErrLiveAPIRelayBindingMismatch
		}
		return true, nil
	default:
		return false, ErrInvalidConfiguration
	}
}

// ValidRelayBindingID is shared-compatible with the Control Panel's static
// Relay identity contract. It prevents a stream key, URL, or generic label
// from being persisted or advertised as a supposedly non-secret binding.
func ValidRelayBindingID(bindingID string) bool {
	return relayBindingIDPattern.MatchString(bindingID)
}

// ValidateRelayURLTemplate verifies the configured Relay URL before Encoder
// advertises it. The placeholder is replaced by a safe non-secret ID so the
// exact same target safety rules apply to capabilities and FFmpeg output.
func ValidateRelayURLTemplate(template string) error {
	target := strings.TrimSpace(template)
	if strings.Contains(target, "{stream_id}") {
		target = strings.ReplaceAll(target, "{stream_id}", "relay-policy-validation")
	} else {
		target = strings.TrimRight(target, "/") + "/relay-policy-validation"
	}
	return ValidateRelayTarget(target)
}

// ValidateRelayTarget admits loopback RTMP/RTMPS targets and exactly the
// Compose-owned output-relay service when its explicit identity flag is set.
// It intentionally cannot authorize arbitrary hostnames.
func ValidateRelayTarget(outputTarget string) error {
	outputTarget = strings.TrimSpace(outputTarget)
	if outputTarget == "" || strings.ContainsAny(outputTarget, "|[]\r\n") {
		return ErrUnsafeRelayTarget
	}
	parsed, err := url.Parse(outputTarget)
	if err != nil {
		return ErrUnsafeRelayTarget
	}
	if parsed.Scheme != "rtmp" && parsed.Scheme != "rtmps" {
		return ErrUnsafeRelayTarget
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ErrUnsafeRelayTarget
	}
	if strings.TrimSpace(parsed.Path) == "" || parsed.Path == "/" {
		return ErrUnsafeRelayTarget
	}
	if isConfiguredComposeOutputRelay(parsed) {
		return nil
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return ErrUnsafeRelayTarget
	}
	return nil
}

func isConfiguredComposeOutputRelay(parsed *url.URL) bool {
	return strings.TrimSpace(os.Getenv(composeOutputRelayEnv)) == "1" &&
		parsed.Scheme == "rtmp" &&
		parsed.Hostname() == composeOutputRelayHost &&
		parsed.Port() == composeOutputRelayPort
}
