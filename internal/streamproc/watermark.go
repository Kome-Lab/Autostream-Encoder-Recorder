package streamproc

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxWatermarkImageBytes = 5 << 20

func materializeWatermarkAsset(tmpDir string, config map[string]any) (string, error) {
	if config == nil {
		return "", nil
	}
	if enabled, ok := config["watermark_enabled"].(bool); ok && !enabled {
		return "", nil
	}
	dataURL, _ := config["watermark_image_data_url"].(string)
	dataURL = strings.TrimSpace(dataURL)
	if dataURL == "" {
		return "", nil
	}
	header, encoded, ok := strings.Cut(dataURL, ",")
	if !ok || !strings.HasPrefix(strings.ToLower(header), "data:image/") || !strings.Contains(strings.ToLower(header), ";base64") {
		return "", errors.New("watermark image must be a base64 data URL")
	}
	mediaType := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.SplitN(header, ";", 2)[0], "data:"))), "image/")
	ext := ""
	switch mediaType {
	case "png":
		ext = ".png"
	case "jpeg", "jpg":
		ext = ".jpg"
	case "webp":
		ext = ".webp"
	default:
		return "", fmt.Errorf("unsupported watermark image type %q", mediaType)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", errors.New("watermark image data is not valid base64")
	}
	if len(decoded) == 0 || len(decoded) > maxWatermarkImageBytes {
		return "", fmt.Errorf("watermark image must be between 1 and %d bytes", maxWatermarkImageBytes)
	}
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(tmpDir, ".watermark-*"+ext)
	if err != nil {
		return "", err
	}
	path := filepath.Clean(file.Name())
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(decoded); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	remove = false
	return path, nil
}
