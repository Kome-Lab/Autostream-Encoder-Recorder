package videocover

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"regexp"
	"strconv"
	"strings"
	"sync"

	_ "golang.org/x/image/webp"
)

func TransparentFrame(width, height int) ([]byte, error) {
	if width < 1 || height < 1 || int64(width)*int64(height) > 40_000_000 {
		return nil, NewError(ErrorMediaAssetDimensionMismatch)
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	// Set one pixel explicitly so the intended fully transparent color model
	// remains clear if the allocation implementation ever changes.
	img.SetNRGBA(0, 0, color.NRGBA{})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, NewError(ErrorMediaAssetDecodeFailed)
	}
	return out.Bytes(), nil
}

const MaxAssetBytes int64 = 20 << 20

var safeAssetID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type AssetRef struct {
	StreamID  string
	AssetID   string
	VariantID string
}

type FetchMetadata struct {
	MediaType string
	ByteSize  int64
	AssetID   string
	VariantID string
	Width     int
	Height    int
	SHA256    string
}

type AssetFetcher interface {
	Fetch(ctx context.Context, ref AssetRef, maxBytes int64) ([]byte, FetchMetadata, error)
}

type cacheEntry struct {
	key  string
	data []byte
	size int64
}

type Loader struct {
	Fetcher    AssetFetcher
	maxEntries int
	maxBytes   int64

	mu      sync.Mutex
	entries map[string]*list.Element
	lru     list.List
	bytes   int64
}

func NewLoader(fetcher AssetFetcher, maxEntries int, maxBytes int64) *Loader {
	if maxEntries < 1 {
		maxEntries = 16
	}
	if maxBytes < 1 {
		maxBytes = 64 << 20
	}
	return &Loader{Fetcher: fetcher, maxEntries: maxEntries, maxBytes: maxBytes, entries: map[string]*list.Element{}}
}

func (l *Loader) Load(ctx context.Context, streamID string, descriptor MediaAssetDescriptor, outputWidth, outputHeight int) ([]byte, error) {
	if err := validateDescriptor(descriptor, outputWidth, outputHeight); err != nil {
		return nil, err
	}
	if l == nil || l.Fetcher == nil {
		return nil, NewError(ErrorCapabilityRequired)
	}
	key := strings.Join([]string{streamID, descriptor.AssetID, descriptor.VariantID, descriptor.SHA256, strconv.FormatUint(descriptor.Revision, 10), strconv.Itoa(outputWidth), strconv.Itoa(outputHeight)}, "\x00")
	if data, ok := l.cached(key); ok {
		return data, nil
	}
	body, meta, err := l.Fetcher.Fetch(ctx, AssetRef{StreamID: streamID, AssetID: descriptor.AssetID, VariantID: descriptor.VariantID}, descriptor.ByteSize)
	if err != nil {
		if ErrorCodeOf(err) != "" {
			return nil, err
		}
		return nil, NewError(ErrorMediaAssetTimeout)
	}
	if int64(len(body)) > MaxAssetBytes || meta.ByteSize > MaxAssetBytes {
		return nil, NewError(ErrorMediaAssetTooLarge)
	}
	if int64(len(body)) != descriptor.ByteSize || meta.ByteSize != descriptor.ByteSize {
		return nil, NewError(ErrorMediaAssetHashMismatch)
	}
	if meta.AssetID != descriptor.AssetID || meta.VariantID != descriptor.VariantID {
		return nil, NewError(ErrorMediaAssetVariantFailed)
	}
	if canonicalMediaType(meta.MediaType) != descriptor.MediaType {
		return nil, NewError(ErrorMediaAssetFormatUnsupported)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if digest != descriptor.SHA256 || strings.ToLower(meta.SHA256) != descriptor.SHA256 {
		return nil, NewError(ErrorMediaAssetHashMismatch)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || mediaTypeForFormat(format) != descriptor.MediaType {
		return nil, NewError(ErrorMediaAssetDecodeFailed)
	}
	if config.Width != descriptor.Width || config.Height != descriptor.Height || meta.Width != descriptor.Width || meta.Height != descriptor.Height {
		return nil, NewError(ErrorMediaAssetDimensionMismatch)
	}
	if descriptor.MediaType == "image/png" && pngHasChunk(body, "acTL") ||
		descriptor.MediaType == "image/webp" && (webPHasChunk(body, "ANIM") || webPHasChunk(body, "ANMF")) {
		return nil, NewError(ErrorMediaAssetFormatUnsupported)
	}
	decoded, format, err := image.Decode(bytes.NewReader(body))
	if err != nil || mediaTypeForFormat(format) != descriptor.MediaType {
		return nil, NewError(ErrorMediaAssetDecodeFailed)
	}
	if !isOpaque(decoded) {
		return nil, NewError(ErrorMediaAssetVariantFailed)
	}
	var normalized bytes.Buffer
	if err := png.Encode(&normalized, decoded); err != nil {
		return nil, NewError(ErrorMediaAssetDecodeFailed)
	}
	data := normalized.Bytes()
	l.store(key, data)
	return append([]byte(nil), data...), nil
}

// ValidateDescriptor validates the complete Control Panel descriptor shape
// independently from fetch/cache availability. Callers use this before fence
// evaluation so a malformed typed request cannot turn into a capability or
// network failure merely because it bypassed JSON Schema validation.
func ValidateDescriptor(d MediaAssetDescriptor) error {
	if err := descriptorShapeError(d); err != nil {
		return NewError(ErrorInvalidRequest)
	}
	return nil
}

func pngHasChunk(body []byte, wanted string) bool {
	if len(wanted) != 4 || len(body) < 8 || !bytes.Equal(body[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return false
	}
	for offset := 8; offset+12 <= len(body); {
		length := uint64(binary.BigEndian.Uint32(body[offset : offset+4]))
		end := uint64(offset) + 12 + length
		if end > uint64(len(body)) {
			return false
		}
		if string(body[offset+4:offset+8]) == wanted {
			return true
		}
		offset = int(end)
	}
	return false
}

func webPHasChunk(body []byte, wanted string) bool {
	if len(wanted) != 4 || len(body) < 12 || string(body[:4]) != "RIFF" || string(body[8:12]) != "WEBP" {
		return false
	}
	for offset := 12; offset+8 <= len(body); {
		length := uint64(binary.LittleEndian.Uint32(body[offset+4 : offset+8]))
		padded := length + length%2
		end := uint64(offset) + 8 + padded
		if end > uint64(len(body)) {
			return false
		}
		if string(body[offset:offset+4]) == wanted {
			return true
		}
		offset = int(end)
	}
	return false
}

func validateDescriptor(d MediaAssetDescriptor, outputWidth, outputHeight int) error {
	if err := descriptorShapeError(d); err != nil {
		return err
	}
	if outputWidth > 0 && outputHeight > 0 {
		denominator := int64(outputWidth) * int64(d.Height)
		delta := int64(d.Width)*int64(outputHeight) - denominator
		if delta < 0 {
			delta = -delta
		}
		if denominator <= 0 || delta*1_000_000/denominator > 1000 {
			return NewError(ErrorMediaAssetAspectRatioInvalid)
		}
	}
	return nil
}

func descriptorShapeError(d MediaAssetDescriptor) error {
	if !safeAssetID.MatchString(d.AssetID) || !safeAssetID.MatchString(d.VariantID) || d.Usage != "video_cover" || d.Readiness != ReadinessReady || d.Error != nil || d.Animated || d.Revision < 1 {
		return NewError(ErrorInvalidRequest)
	}
	if d.MediaType != "image/png" && d.MediaType != "image/jpeg" && d.MediaType != "image/webp" {
		return NewError(ErrorMediaAssetFormatUnsupported)
	}
	if d.Width < 1 || d.Width > 8192 || d.Height < 1 || d.Height > 8192 || d.ByteSize < 1 || d.ByteSize > MaxAssetBytes || d.PixelCount != int64(d.Width)*int64(d.Height) || d.PixelCount > 40_000_000 {
		return NewError(ErrorMediaAssetDimensionMismatch)
	}
	if d.AspectRatioErrorPPM == nil || *d.AspectRatioErrorPPM < 0 || *d.AspectRatioErrorPPM > 1000 || d.Opaque == nil || !*d.Opaque {
		return NewError(ErrorInvalidRequest)
	}
	if len(d.SHA256) != 64 {
		return NewError(ErrorMediaAssetHashMismatch)
	}
	if _, err := hex.DecodeString(d.SHA256); err != nil || strings.ToLower(d.SHA256) != d.SHA256 {
		return NewError(ErrorMediaAssetHashMismatch)
	}
	return nil
}

func mediaTypeForFormat(format string) string {
	switch strings.ToLower(format) {
	case "png":
		return "image/png"
	case "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return ""
	}
}

func canonicalMediaType(value string) string {
	if head, _, ok := strings.Cut(strings.TrimSpace(value), ";"); ok {
		value = head
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func isOpaque(img image.Image) bool {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := img.At(x, y).RGBA()
			if alpha != 0xffff {
				return false
			}
		}
	}
	return true
}

func (l *Loader) cached(key string) ([]byte, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	element, ok := l.entries[key]
	if !ok {
		return nil, false
	}
	l.lru.MoveToFront(element)
	entry := element.Value.(cacheEntry)
	return append([]byte(nil), entry.data...), true
}

func (l *Loader) store(key string, data []byte) {
	if int64(len(data)) > l.maxBytes {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.entries[key]; ok {
		l.lru.MoveToFront(existing)
		return
	}
	entry := cacheEntry{key: key, data: append([]byte(nil), data...), size: int64(len(data))}
	element := l.lru.PushFront(entry)
	l.entries[key] = element
	l.bytes += entry.size
	for len(l.entries) > l.maxEntries || l.bytes > l.maxBytes {
		oldest := l.lru.Back()
		if oldest == nil {
			break
		}
		old := oldest.Value.(cacheEntry)
		delete(l.entries, old.key)
		l.lru.Remove(oldest)
		l.bytes -= old.size
	}
}
