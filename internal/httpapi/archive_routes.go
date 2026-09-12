package httpapi

import (
	"errors"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/autostream-encoder-recorder/internal/archive"
	"github.com/example/autostream-encoder-recorder/internal/ffmpeg"
	"github.com/example/autostream-encoder-recorder/internal/lifecycle"
)

func streamPreview(archiveRoot string, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		layout, err := archive.NewLayout(archiveRoot, r.PathValue("id"))
		if err != nil || !archive.IsPreviewFileName(r.PathValue("name")) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_preview_path"})
			return
		}
		file, info, err := archive.OpenPreviewFile(layout, r.PathValue("name"))
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "preview_not_found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_preview_file"})
			return
		}
		defer file.Close()

		w.Header().Set("Vary", "Authorization")
		if r.PathValue("name") == archive.PreviewPlaylistName {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Content-Type", "video/mp2t")
			w.Header().Set("Cache-Control", "private, max-age=30")
		}
		http.ServeContent(w, r, r.PathValue("name"), info.ModTime(), file)
	}
}

func packageStream(verifier TokenVerifier, resolver RuntimeSecretResolver, runtimeConfig RuntimeConfigProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		var request packageStreamRequest
		if status, err := decodeLimitedStrictJSON(w, r, maxControlBodyBytes, &request); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		job := request.packageJob()
		if err := applyPackageArchiveRuntimeConfig(r.Context(), &job, runtimeConfig); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_config_fetch_failed"})
			return
		}
		if err := resolvePackageArchiveRuntimeSecrets(r.Context(), &job, resolver); err != nil {
			writeRuntimeSecretResolveError(w, err)
			return
		}
		archiveRoot := os.Getenv("AUTOSTREAM_ARCHIVE_DIR")
		if archiveRoot == "" {
			archiveRoot = "/var/lib/autostream/archives"
		}
		ffmpegBin := os.Getenv("FFMPEG_BIN")
		if ffmpegBin == "" {
			ffmpegBin = "ffmpeg"
		}
		var runner ffmpeg.Runner = ffmpeg.CommandRunner{}
		if job.DryRun {
			runner = &ffmpeg.DryRunRunner{}
		}
		uploader := archive.RetryUploader{
			Inner:  uploaderFromEnv(job.DryRun),
			Policy: archive.RetryPolicy{MaxAttempts: envInt("GOOGLE_DRIVE_UPLOAD_RETRY_MAX", 5), BaseDelay: time.Duration(envInt("GOOGLE_DRIVE_UPLOAD_RETRY_BASE_DELAY_SEC", 2)) * time.Second},
		}
		manager := lifecycle.Manager{ArchiveRoot: archiveRoot, FFmpegBin: ffmpegBin, Runner: runner, Uploader: uploader}
		started := time.Now().UTC()
		result, err := manager.Package(r.Context(), job)
		if err != nil {
			reportPackageFailed(r.Context(), job, time.Since(started), err)
			writeJSON(w, http.StatusBadRequest, packageFailureResponse(err, job.DryRun))
			return
		}
		reportPackageCompleted(r.Context(), job, result, time.Since(started))
		reportControlPanelArtifacts(r.Context(), job, result)
		writeJSON(w, http.StatusAccepted, result.Metadata)
	}
}

func downloadArchiveArtifact(archiveRoot string, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		path, info, err := safeArchiveArtifactPath(archiveRoot, r.PathValue("id"), r.PathValue("run_id"), r.PathValue("name"))
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "artifact_not_found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_archive_artifact"})
			return
		}
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "artifact_not_found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "open_archive_artifact_failed"})
			return
		}
		defer file.Close()
		if err := verifyOpenArchiveArtifact(path, file, info); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_archive_artifact"})
			return
		}
		contentType := mime.TypeByExtension(filepath.Ext(r.PathValue("name")))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", `attachment; filename="`+safeContentDispositionName(r.PathValue("name"))+`"`)
		http.ServeContent(w, r, r.PathValue("name"), info.ModTime(), file)
	}
}

func deleteArchiveArtifact(archiveRoot string, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		path, _, err := safeArchiveArtifactPath(archiveRoot, r.PathValue("id"), r.PathValue("run_id"), r.PathValue("name"))
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "artifact_not_found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_archive_artifact"})
			return
		}
		if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "artifact_not_found"})
			return
		} else if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "delete_archive_artifact_failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

func renameArchiveArtifact(archiveRoot string, verifier TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireServiceToken(w, r, verifier) {
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if status, err := decodeLimitedJSON(w, r, maxControlBodyBytes, &body); err != nil {
			writeJSON(w, status, map[string]string{"code": limitedJSONErrorCode(status)})
			return
		}
		source, _, err := safeArchiveArtifactPath(archiveRoot, r.PathValue("id"), r.PathValue("run_id"), r.PathValue("name"))
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "artifact_not_found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_archive_artifact"})
			return
		}
		target, _, err := safeArchiveArtifactPathForName(archiveRoot, r.PathValue("id"), r.PathValue("run_id"), body.Name, false)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_archive_artifact"})
			return
		}
		if _, err := os.Lstat(target); err == nil {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "artifact_exists"})
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "stat_archive_artifact_failed"})
			return
		}
		if err := os.Rename(source, target); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "rename_archive_artifact_failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "renamed", "name": strings.TrimSpace(body.Name)})
	}
}

func safeArchiveArtifactPath(archiveRoot, streamID, archiveRunID, name string) (string, os.FileInfo, error) {
	return safeArchiveArtifactPathForName(archiveRoot, streamID, archiveRunID, name, true)
}

func safeArchiveArtifactPathForName(archiveRoot, streamID, archiveRunID, name string, requireExisting bool) (string, os.FileInfo, error) {
	layout, err := archive.NewRunLayout(archiveRoot, streamID, archiveRunID)
	if err != nil {
		return "", nil, err
	}
	name = strings.TrimSpace(name)
	if !safeArchiveArtifactName(name) {
		return "", nil, errors.New("unsafe archive artifact name")
	}
	if err := archive.EnsureDirNoSymlinks(layout.RootDir, layout.FinalDir()); err != nil {
		return "", nil, err
	}
	path := filepath.Join(layout.FinalDir(), name)
	rootAbs, err := filepath.Abs(layout.RootDir)
	if err != nil {
		return "", nil, err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return "", nil, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", nil, errors.New("archive artifact path escaped root")
	}
	info, err := os.Lstat(pathAbs)
	if err != nil {
		if !requireExisting && errors.Is(err, os.ErrNotExist) {
			return pathAbs, nil, nil
		}
		return "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, errors.New("archive artifact must be a regular file")
	}
	return pathAbs, info, nil
}

func verifyOpenArchiveArtifact(path string, file *os.File, before os.FileInfo) error {
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() {
		return errors.New("archive artifact must be a regular file")
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, after) || !os.SameFile(opened, after) {
		return errors.New("archive artifact changed while opening")
	}
	return nil
}

func safeArchiveArtifactName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 255 || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".mp4" && ext != ".mkv" && ext != ".json" && ext != ".jsonl" && ext != ".vtt" {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func safeContentDispositionName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, `"`, "")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	if name == "" {
		return "archive"
	}
	return name
}
