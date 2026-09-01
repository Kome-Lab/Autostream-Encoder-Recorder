package watermarkfeed

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"

	"github.com/example/autostream-encoder-recorder/internal/imagefeed"

	_ "golang.org/x/image/webp"
	_ "image/jpeg"
)

const (
	maxImageBytes  = 5 << 20
	maxImagePixels = 4096 * 4096
)

// Source preserves the watermark feed API while sharing the generic image
// transport used by independently controlled visual layers.
type Source struct {
	feed *imagefeed.Source
}

func New(config map[string]any) (*Source, error) {
	frame, err := Frame(config)
	if err != nil {
		return nil, err
	}
	feed, err := imagefeed.New("watermark", frame)
	if err != nil {
		return nil, err
	}
	return &Source{feed: feed}, nil
}

func (s *Source) InputURL() string {
	if s == nil || s.feed == nil {
		return ""
	}
	return s.feed.InputURL()
}

func (s *Source) Update(frame []byte) error {
	if s == nil || len(frame) == 0 {
		return errors.New("watermark frame is required")
	}
	if s.feed == nil {
		return errors.New("watermark source is closed")
	}
	return s.feed.Update(frame)
}

// UpdateAndWait replaces the Watermark frame and waits until the exact feed
// version has been written to the connected graph input. This is only the
// transport half of an applied witness; callers must also observe downstream
// output progress before publishing the new Watermark state.
func (s *Source) UpdateAndWait(ctx context.Context, frame []byte) error {
	if s == nil || len(frame) == 0 {
		return errors.New("watermark frame is required")
	}
	if s.feed == nil {
		return errors.New("watermark source is closed")
	}
	return s.feed.UpdateAndWait(ctx, frame)
}

func (s *Source) Close() error {
	if s == nil || s.feed == nil {
		return nil
	}
	return s.feed.Close()
}

// Frame validates and normalizes a Control Panel watermark profile to PNG.
// A disabled or unselected watermark becomes a transparent frame, which keeps
// the FFmpeg graph stable while allowing later live enablement.
func Frame(config map[string]any) ([]byte, error) {
	if config == nil {
		return transparentPNG()
	}
	if enabled, ok := config["watermark_enabled"].(bool); ok && !enabled {
		return transparentPNG()
	}
	dataURL, _ := config["watermark_image_data_url"].(string)
	dataURL = strings.TrimSpace(dataURL)
	if dataURL == "" {
		return transparentPNG()
	}
	header, encoded, ok := strings.Cut(dataURL, ",")
	lowerHeader := strings.ToLower(strings.TrimSpace(header))
	if !ok || !strings.HasPrefix(lowerHeader, "data:image/") || !strings.Contains(lowerHeader, ";base64") {
		return nil, errors.New("watermark image must be a base64 data URL")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, errors.New("watermark image data is not valid base64")
	}
	if len(decoded) == 0 || len(decoded) > maxImageBytes {
		return nil, fmt.Errorf("watermark image must be between 1 and %d bytes", maxImageBytes)
	}
	configOnly, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || configOnly.Width <= 0 || configOnly.Height <= 0 || configOnly.Width > maxImagePixels/configOnly.Height {
		return nil, errors.New("watermark image dimensions are invalid")
	}
	img, _, err := image.Decode(bytes.NewReader(decoded))
	if err != nil {
		return nil, errors.New("watermark image could not be decoded")
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, errors.New("watermark image could not be normalized")
	}
	return out.Bytes(), nil
}

func transparentPNG() ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.SetNRGBA(0, 0, color.NRGBA{})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
