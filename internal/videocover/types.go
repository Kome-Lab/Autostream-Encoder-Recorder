// Package videocover owns the Encoder-side Video Cover contract and its
// fail-closed state. The public values intentionally contain safe identities
// and integrity metadata only; transport URLs, storage paths, tokens, and
// image bytes never appear in these types.
package videocover

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const Capability = "live_video_cover_v1"

type Readiness string

const (
	ReadinessReady    Readiness = "ready"
	ReadinessNotReady Readiness = "not_ready"
	ReadinessUnknown  Readiness = "unknown"
)

type ErrorCode string

const (
	ErrorMediaAssetUnauthorized       ErrorCode = "media_asset_unauthorized"
	ErrorMediaAssetNotFound           ErrorCode = "media_asset_not_found"
	ErrorMediaAssetHashMismatch       ErrorCode = "media_asset_hash_mismatch"
	ErrorMediaAssetDimensionMismatch  ErrorCode = "media_asset_dimension_mismatch"
	ErrorMediaAssetTimeout            ErrorCode = "media_asset_timeout"
	ErrorMediaAssetFormatUnsupported  ErrorCode = "media_asset_format_unsupported"
	ErrorMediaAssetTooLarge           ErrorCode = "media_asset_too_large"
	ErrorMediaAssetDecodeFailed       ErrorCode = "media_asset_decode_failed"
	ErrorMediaAssetAspectRatioInvalid ErrorCode = "media_asset_aspect_ratio_invalid"
	ErrorMediaAssetVariantProcessing  ErrorCode = "media_asset_variant_processing"
	ErrorMediaAssetVariantFailed      ErrorCode = "media_asset_variant_failed"
	ErrorStaleJobGeneration           ErrorCode = "stale_job_generation"
	ErrorStaleCoverGeneration         ErrorCode = "stale_cover_generation"
	ErrorStaleCoverRevision           ErrorCode = "stale_cover_revision"
	ErrorIdempotencyConflict          ErrorCode = "idempotency_conflict"
	ErrorCoverApplyAmbiguous          ErrorCode = "cover_apply_ambiguous"
	ErrorCoverGraphUnavailable        ErrorCode = "cover_graph_unavailable"
	ErrorRevisionPayloadConflict      ErrorCode = "revision_payload_conflict"
	ErrorCapabilityRequired           ErrorCode = "capability_required"
	ErrorInvalidRequest               ErrorCode = "invalid_video_cover_request"
)

type SafeError struct {
	Code      ErrorCode `json:"code"`
	RequestID string    `json:"request_id,omitempty"`
}

func (safeError *SafeError) UnmarshalJSON(body []byte) error {
	type wire SafeError
	var value wire
	if _, err := decodeStrictObject(body, &value, []string{"code"}); err != nil {
		return err
	}
	*safeError = SafeError(value)
	return nil
}

type codedError struct{ code ErrorCode }

func (e codedError) Error() string { return string(e.code) }

func NewError(code ErrorCode) error { return codedError{code: code} }

func ErrorCodeOf(err error) ErrorCode {
	var coded codedError
	if errors.As(err, &coded) {
		return coded.code
	}
	return ""
}

type MediaAssetDescriptor struct {
	AssetID             string     `json:"asset_id"`
	VariantID           string     `json:"variant_id"`
	Usage               string     `json:"usage"`
	MediaType           string     `json:"media_type"`
	Width               int        `json:"width"`
	Height              int        `json:"height"`
	ByteSize            int64      `json:"byte_size"`
	PixelCount          int64      `json:"pixel_count"`
	Animated            bool       `json:"animated"`
	AspectRatioErrorPPM *int       `json:"aspect_ratio_error_ppm,omitempty"`
	Opaque              *bool      `json:"opaque,omitempty"`
	SHA256              string     `json:"sha256"`
	Revision            uint64     `json:"revision"`
	Readiness           Readiness  `json:"readiness"`
	Error               *SafeError `json:"error,omitempty"`
}

func (descriptor *MediaAssetDescriptor) UnmarshalJSON(body []byte) error {
	type wire MediaAssetDescriptor
	var value wire
	fields, err := decodeStrictObject(body, &value, []string{
		"asset_id", "variant_id", "usage", "media_type", "width", "height", "byte_size",
		"pixel_count", "animated", "sha256", "revision", "readiness",
	})
	if err != nil {
		return err
	}
	_, aspectPresent := fields["aspect_ratio_error_ppm"]
	_, opaquePresent := fields["opaque"]
	_, errorPresent := fields["error"]
	if value.Usage == "video_cover" && (!aspectPresent || value.AspectRatioErrorPPM == nil || !opaquePresent || value.Opaque == nil) {
		return errors.New("missing required video cover asset field")
	}
	if value.Readiness == ReadinessReady && errorPresent || value.Readiness != ReadinessReady && (!errorPresent || value.Error == nil) {
		return errors.New("invalid video cover asset error presence")
	}
	*descriptor = MediaAssetDescriptor(value)
	return nil
}

type StartSnapshot struct {
	JobGeneration  uint64                `json:"job_generation"`
	Revision       uint64                `json:"revision"`
	Active         bool                  `json:"active"`
	IdempotencyKey string                `json:"idempotency_key"`
	CoverAsset     *MediaAssetDescriptor `json:"cover_asset,omitempty"`
}

func (snapshot *StartSnapshot) UnmarshalJSON(body []byte) error {
	type wire StartSnapshot
	var value wire
	fields, err := decodeStrictObject(body, &value, []string{"job_generation", "revision", "active", "idempotency_key"})
	if err != nil {
		return err
	}
	_, coverPresent := fields["cover_asset"]
	if value.Active != coverPresent || value.Active && value.CoverAsset == nil {
		return errors.New("invalid video cover start asset presence")
	}
	*snapshot = StartSnapshot(value)
	return nil
}

type ApplyRequest struct {
	StreamID           string                `json:"stream_id"`
	JobGeneration      uint64                `json:"job_generation"`
	ExpectedGeneration uint64                `json:"expected_generation"`
	Revision           uint64                `json:"revision"`
	Active             bool                  `json:"active"`
	IdempotencyKey     string                `json:"idempotency_key"`
	CoverAsset         *MediaAssetDescriptor `json:"cover_asset,omitempty"`
	HideConfirmed      bool                  `json:"hide_confirmed,omitempty"`
}

func (request *ApplyRequest) UnmarshalJSON(body []byte) error {
	type wire ApplyRequest
	var value wire
	fields, err := decodeStrictObject(body, &value, []string{
		"stream_id", "job_generation", "expected_generation", "revision", "active", "idempotency_key",
	})
	if err != nil {
		return err
	}
	_, coverPresent := fields["cover_asset"]
	_, hidePresent := fields["hide_confirmed"]
	if value.Active {
		if !coverPresent || value.CoverAsset == nil || hidePresent {
			return errors.New("invalid active video cover field presence")
		}
	} else if coverPresent || !hidePresent || !value.HideConfirmed {
		return errors.New("invalid inactive video cover field presence")
	}
	*request = ApplyRequest(value)
	return nil
}

func decodeStrictObject(body []byte, dst any, required []string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("invalid UTF-8 in video cover object")
	}
	if err := rejectDuplicateJSONFields(body); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid video cover object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	for _, name := range required {
		raw, exists := fields[name]
		if !exists {
			return nil, errors.New("missing required video cover field")
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, errors.New("null required video cover field")
		}
	}
	return fields, nil
}

func rejectDuplicateJSONFields(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := consumeUniqueJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid video cover object")
	}
	return nil
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid video cover object")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate video cover field")
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("invalid video cover object")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid video cover object")
		}
	default:
		return errors.New("invalid video cover object")
	}
	return nil
}

type DesiredState struct {
	Active    bool   `json:"active"`
	Revision  uint64 `json:"revision"`
	Source    string `json:"source"`
	VariantID string `json:"variant_id,omitempty"`
}

type AppliedState struct {
	State     string `json:"state"`
	Active    *bool  `json:"active,omitempty"`
	Revision  uint64 `json:"revision,omitempty"`
	VariantID string `json:"variant_id,omitempty"`
}

type LayerState struct {
	Enabled   bool   `json:"enabled"`
	Revision  uint64 `json:"revision"`
	VariantID string `json:"variant_id,omitempty"`
}

type VisualAudioContinuity struct {
	ProcessRestart           int `json:"process_restart"`
	AudioEncoderRestart      int `json:"audio_encoder_restart"`
	AudioMuxRestart          int `json:"audio_mux_restart"`
	GraphRebuild             int `json:"graph_rebuild"`
	Reconnect                int `json:"reconnect"`
	SequenceLoss             int `json:"sequence_loss"`
	TimestampDiscontinuity   int `json:"timestamp_discontinuity"`
	IntentionalMuteInsertion int `json:"intentional_mute_insertion"`
}

type PipelineInvariant struct {
	Layers                    []string              `json:"layers"`
	WatermarkTopmost          bool                  `json:"watermark_topmost"`
	CoverWatermarkIndependent bool                  `json:"cover_watermark_independent"`
	OutputParity              []string              `json:"output_parity"`
	AudioContinuity           VisualAudioContinuity `json:"audio_continuity"`
}

func FixedPipelineInvariant() PipelineInvariant {
	return PipelineInvariant{
		Layers:           []string{"base_or_worker_scene", "video_cover", "watermark", "video_encode", "tee_live_archive_preview"},
		WatermarkTopmost: true, CoverWatermarkIndependent: true,
		OutputParity: []string{"live", "archive", "preview"},
	}
}

type AppliedWitness struct {
	GraphApplied bool              `json:"graph_applied"`
	Generation   uint64            `json:"generation"`
	Revision     uint64            `json:"revision"`
	Active       bool              `json:"active"`
	Cover        LayerState        `json:"cover"`
	Watermark    LayerState        `json:"watermark"`
	Pipeline     PipelineInvariant `json:"pipeline"`
}

type RuntimeState struct {
	StreamID          string                `json:"stream_id"`
	JobGeneration     uint64                `json:"job_generation"`
	Generation        uint64                `json:"generation"`
	Capability        string                `json:"capability"`
	Readiness         Readiness             `json:"readiness"`
	Desired           DesiredState          `json:"desired"`
	Applied           AppliedState          `json:"applied"`
	Cover             LayerState            `json:"cover"`
	CoverAsset        *MediaAssetDescriptor `json:"cover_asset,omitempty"`
	Watermark         LayerState            `json:"watermark"`
	Pipeline          PipelineInvariant     `json:"pipeline"`
	AppliedWitness    *AppliedWitness       `json:"applied_witness,omitempty"`
	NoAutomaticResend bool                  `json:"no_automatic_resend"`
	LastGoodApplied   *AppliedState         `json:"last_good_applied,omitempty"`
	Error             *SafeError            `json:"error,omitempty"`
}

type ApplyOutcome string

const (
	OutcomeApplied   ApplyOutcome = "applied"
	OutcomeRejected  ApplyOutcome = "rejected"
	OutcomeAmbiguous ApplyOutcome = "ambiguous"
)

type ApplyResponse struct {
	StreamID          string       `json:"stream_id"`
	JobGeneration     uint64       `json:"job_generation"`
	RequestedRevision uint64       `json:"requested_revision"`
	ActualGeneration  uint64       `json:"actual_generation"`
	Accepted          bool         `json:"accepted"`
	Rejected          bool         `json:"rejected"`
	Applied           bool         `json:"applied"`
	Outcome           ApplyOutcome `json:"outcome"`
	Actual            RuntimeState `json:"actual"`
	Error             *SafeError   `json:"error,omitempty"`
}
