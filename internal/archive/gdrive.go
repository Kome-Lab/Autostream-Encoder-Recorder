package archive

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

type GoogleDriveConfig struct {
	AuthMode              string
	ApplicationCredential string
	ServiceAccountJSON    string
	FolderID              string
	BasePath              string
	SharedDrive           bool
	ClientID              string
	ClientSecret          string
	RefreshToken          string
	DryRun                bool
}

type GoogleDriveAPIUploader struct {
	Config GoogleDriveConfig
}

func (c GoogleDriveConfig) String() string {
	body, err := json.Marshal(c.SafeSummary())
	if err != nil {
		return `{"error":"google_drive_config_summary_unavailable"}`
	}
	return string(body)
}

func (c GoogleDriveConfig) GoString() string {
	return "GoogleDriveConfig" + c.String()
}

func (c GoogleDriveConfig) SafeSummary() map[string]any {
	out := map[string]any{
		"auth_mode":    c.AuthMode,
		"base_path":    c.BasePath,
		"shared_drive": c.SharedDrive,
		"dry_run":      c.DryRun,
	}
	if strings.TrimSpace(c.ApplicationCredential) != "" {
		out["application_credentials_configured"] = true
	}
	if strings.TrimSpace(c.ServiceAccountJSON) != "" {
		out["service_account_json_configured"] = true
	}
	if strings.TrimSpace(c.FolderID) != "" {
		out["folder_id_configured"] = true
		out["folder_id_fingerprint"] = secretFingerprint(c.FolderID)
	}
	if strings.TrimSpace(c.ClientID) != "" {
		out["client_id_configured"] = true
	}
	if strings.TrimSpace(c.ClientSecret) != "" {
		out["client_secret_configured"] = true
	}
	if strings.TrimSpace(c.RefreshToken) != "" {
		out["refresh_token_configured"] = true
	}
	return out
}

func GoogleDriveConfigFromEnv() GoogleDriveConfig {
	return GoogleDriveConfig{
		AuthMode:              os.Getenv("GOOGLE_DRIVE_AUTH_MODE"),
		ApplicationCredential: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		FolderID:              os.Getenv("GOOGLE_DRIVE_FOLDER_ID"),
		BasePath:              envDefault("GDRIVE_BASE_PATH", "AutoStream"),
		SharedDrive:           envBool("GOOGLE_DRIVE_SHARED_DRIVE", false),
	}
}

func (c GoogleDriveConfig) Validate() error {
	switch c.AuthMode {
	case "service_account":
		if c.ApplicationCredential == "" && strings.TrimSpace(c.ServiceAccountJSON) == "" {
			return errors.New("GOOGLE_APPLICATION_CREDENTIALS or service_account_json is required for service_account")
		}
	case "oauth2":
		if c.ClientID == "" || c.ClientSecret == "" || c.RefreshToken == "" {
			return errors.New("client_id, client_secret, and refresh_token are required for oauth2")
		}
	default:
		return errors.New("unsupported GOOGLE_DRIVE_AUTH_MODE")
	}
	if c.FolderID == "" {
		return errors.New("GOOGLE_DRIVE_FOLDER_ID is required")
	}
	if c.BasePath == "" {
		return errors.New("GDRIVE_BASE_PATH is required")
	}
	return nil
}

func (u GoogleDriveAPIUploader) Upload(ctx context.Context, streamName, streamID string, startedAtJST time.Time, files []File) (UploadResult, error) {
	if err := ctx.Err(); err != nil {
		return UploadResult{}, err
	}
	if u.Config.DryRun {
		return googleDriveDryRun(files), nil
	}
	if err := u.Config.Validate(); err != nil {
		return UploadResult{}, err
	}
	svc, err := u.driveService(ctx)
	if err != nil {
		return UploadResult{}, err
	}
	folderID, err := u.ensureArchiveFolder(ctx, svc, streamName, streamID, startedAtJST)
	if err != nil {
		return UploadResult{}, err
	}
	result := UploadResult{DryRun: false, FolderID: folderID, FileIDs: map[string]string{}}
	for _, file := range files {
		fileID, err := uploadFile(ctx, svc, folderID, file, u.Config.SharedDrive)
		if err != nil {
			return UploadResult{}, err
		}
		result.FileIDs[file.DrivePath] = fileID
	}
	return result, nil
}

func (u GoogleDriveAPIUploader) driveService(ctx context.Context) (*drive.Service, error) {
	return u.driveServiceWithOptions(ctx, google.Endpoint)
}

func (u GoogleDriveAPIUploader) driveServiceWithOptions(ctx context.Context, oauthEndpoint oauth2.Endpoint, options ...option.ClientOption) (*drive.Service, error) {
	switch u.Config.AuthMode {
	case "service_account":
		options = append(options, option.WithScopes(drive.DriveFileScope))
		if strings.TrimSpace(u.Config.ServiceAccountJSON) != "" {
			options = append(options, option.WithCredentialsJSON([]byte(u.Config.ServiceAccountJSON)))
			return drive.NewService(ctx, options...)
		}
		options = append(options, option.WithCredentialsFile(u.Config.ApplicationCredential))
		return drive.NewService(ctx, options...)
	case "oauth2":
		cfg := oauth2.Config{
			ClientID:     u.Config.ClientID,
			ClientSecret: u.Config.ClientSecret,
			Scopes:       []string{drive.DriveFileScope},
			Endpoint:     oauthEndpoint,
		}
		token := &oauth2.Token{RefreshToken: u.Config.RefreshToken}
		options = append(options,
			option.WithTokenSource(cfg.TokenSource(ctx, token)),
			option.WithScopes(drive.DriveFileScope),
		)
		return drive.NewService(ctx, options...)
	default:
		return nil, errors.New("unsupported GOOGLE_DRIVE_AUTH_MODE")
	}
}

func (u GoogleDriveAPIUploader) ensureArchiveFolder(ctx context.Context, svc *drive.Service, streamName, streamID string, startedAtJST time.Time) (string, error) {
	parentID := u.Config.FolderID
	for _, segment := range splitBasePath(u.Config.BasePath) {
		id, err := ensureDriveFolder(ctx, svc, parentID, segment, u.Config.SharedDrive)
		if err != nil {
			return "", err
		}
		parentID = id
	}
	streamFolder, err := ensureDriveFolder(ctx, svc, parentID, safeDriveName(streamName), u.Config.SharedDrive)
	if err != nil {
		return "", err
	}
	archiveFolderName := startedAtJST.Format("20060102_150405") + "_JST_" + safeDriveName(streamID)
	return ensureDriveFolder(ctx, svc, streamFolder, archiveFolderName, u.Config.SharedDrive)
}

func ensureDriveFolder(ctx context.Context, svc *drive.Service, parentID, name string, sharedDrive bool) (string, error) {
	q := "mimeType = 'application/vnd.google-apps.folder' and trashed = false and name = '" + driveQueryLiteral(name) + "' and '" + driveQueryLiteral(parentID) + "' in parents"
	listCall := svc.Files.List().
		Q(q).
		Fields("files(id,name)").
		PageSize(1).
		SupportsAllDrives(sharedDrive).
		Context(ctx)
	if sharedDrive {
		listCall = listCall.IncludeItemsFromAllDrives(true)
	}
	list, err := listCall.Do()
	if err != nil {
		return "", err
	}
	if len(list.Files) > 0 {
		return list.Files[0].Id, nil
	}
	created, err := svc.Files.Create(&drive.File{
		Name:     name,
		MimeType: "application/vnd.google-apps.folder",
		Parents:  []string{parentID},
	}).SupportsAllDrives(sharedDrive).Fields("id").Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return created.Id, nil
}

func uploadFile(ctx context.Context, svc *drive.Service, folderID string, file File, sharedDrive bool) (string, error) {
	info, err := os.Lstat(file.LocalPath)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("archive upload file must not be a symlink")
	}
	if info.IsDir() {
		return "", errors.New("archive upload file must be a regular file")
	}
	f, err := os.Open(file.LocalPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := verifyOpenArchiveFile(f, info); err != nil {
		return "", err
	}
	name := path.Base(file.DrivePath)
	created, err := svc.Files.Create(&drive.File{Name: name, Parents: []string{folderID}}).
		SupportsAllDrives(sharedDrive).
		Media(f, googleapi.ChunkSize(8*1024*1024)).
		Fields("id").
		Context(ctx).
		Do()
	if err != nil {
		return "", err
	}
	return created.Id, nil
}

func verifyOpenArchiveFile(file *os.File, expected os.FileInfo) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("archive upload file must be a regular file")
	}
	if !os.SameFile(expected, info) {
		return errors.New("archive upload file changed while opening")
	}
	return nil
}

func googleDriveDryRun(files []File) UploadResult {
	result := UploadResult{DryRun: true, FolderID: "gdrive-dry-run-folder", FileIDs: map[string]string{}}
	for _, file := range files {
		result.FileIDs[file.DrivePath] = "gdrive-dry-run-file"
	}
	return result
}

func splitBasePath(value string) []string {
	segments := strings.Split(strings.ReplaceAll(value, "\\", "/"), "/")
	out := make([]string, 0, len(segments))
	for _, segment := range segments {
		if cleaned := safeDriveName(segment); cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return out
}

func safeDriveName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "/", "_")
	value = strings.ReplaceAll(value, "\\", "_")
	if value == "" {
		return "untitled"
	}
	return value
}

func driveQueryLiteral(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `'`, `\'`)
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
