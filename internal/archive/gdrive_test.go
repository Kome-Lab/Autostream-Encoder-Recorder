package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

func TestGoogleDriveConfigValidate(t *testing.T) {
	cfg := GoogleDriveConfig{AuthMode: "oauth2", ClientID: "client-id", ClientSecret: "client-secret", RefreshToken: "refresh-token", FolderID: "folder"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGoogleDriveConfigValidateRejectsServiceAccountJSON(t *testing.T) {
	cfg := GoogleDriveConfig{AuthMode: "service_account", ServiceAccountJSON: `{"type":"service_account","client_email":"svc@example.com","private_key":"-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n"}`, FolderID: "folder"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected service account JSON to be rejected")
	}
}

func TestGoogleDriveConfigRejectsServiceAccountMode(t *testing.T) {
	cfg := GoogleDriveConfig{AuthMode: "service_account", FolderID: "folder"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected service account mode to fail")
	}
}

func TestGoogleDriveConfigAllowsOAuth2(t *testing.T) {
	cfg := GoogleDriveConfig{
		AuthMode:     "oauth2",
		ClientID:     "google-client-id",
		ClientSecret: "google-client-secret",
		RefreshToken: "google-refresh-token",
		FolderID:     "folder",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGoogleDriveConfigRejectsIncompleteOAuth2(t *testing.T) {
	cfg := GoogleDriveConfig{AuthMode: "oauth2", ClientID: "google-client-id", FolderID: "folder"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected incomplete oauth2 config to fail")
	}
}

func TestGoogleDriveConfigFromEnvSharedDrive(t *testing.T) {
	t.Setenv("GDRIVE_BASE_PATH", "AutoStream")
	t.Setenv("GOOGLE_DRIVE_SHARED_DRIVE", "true")
	cfg := GoogleDriveConfigFromEnv()
	if cfg.AuthMode != "" || cfg.ApplicationCredential != "" || cfg.FolderID != "" || !cfg.SharedDrive {
		t.Fatalf("expected shared drive flag to be enabled: %#v", cfg)
	}
}

func TestGoogleDriveConfigStringRedactsSecrets(t *testing.T) {
	cfg := GoogleDriveConfig{
		AuthMode:              "oauth2",
		ApplicationCredential: "/etc/autostream/raw-google-credential.json",
		ServiceAccountJSON:    `{"type":"service_account","private_key":"raw-private-key"}`,
		FolderID:              "raw-shared-drive-folder-id",
		SharedDrive:           true,
		ClientID:              "raw-google-client-id",
		ClientSecret:          "raw-google-client-secret",
		RefreshToken:          "raw-google-refresh-token",
		DryRun:                false,
	}
	rendered := fmt.Sprintf("%v %#v", cfg, cfg)
	for _, leaked := range []string{
		"/etc/autostream/raw-google-credential.json",
		"raw-private-key",
		"raw-shared-drive-folder-id",
		"raw-google-client-id",
		"raw-google-client-secret",
		"raw-google-refresh-token",
	} {
		if strings.Contains(rendered, leaked) {
			t.Fatalf("google drive config string leaked %q in %s", leaked, rendered)
		}
	}
	for _, expected := range []string{
		`"folder_id_configured":true`,
		`"folder_id_fingerprint":"sha256:`,
		`"client_secret_configured":true`,
		`"refresh_token_configured":true`,
		`"shared_drive":true`,
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("google drive config summary missing %q in %s", expected, rendered)
		}
	}
}

func TestGoogleDriveUploaderSkeleton(t *testing.T) {
	uploader := GoogleDriveAPIUploader{Config: GoogleDriveConfig{AuthMode: "service_account", ApplicationCredential: "/etc/autostream/google.json", FolderID: "folder", DryRun: true}}
	result, err := uploader.Upload(context.Background(), "stream", "s1", time.Now(), []File{{LocalPath: "final.mp4", DrivePath: "final.mp4"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.FileIDs["final.mp4"] == "" || result.FolderID == "" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestVerifyOpenArchiveFileRejectsChangedFile(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.mp4")
	second := filepath.Join(root, "second.mp4")
	if err := os.WriteFile(first, []byte("first"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second"), 0o640); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(second)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := verifyOpenArchiveFile(file, expected); err == nil {
		t.Fatal("expected changed archive file to fail verification")
	}
}

func TestEnsureDriveFolderUsesSharedDriveQueryOptions(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			io.WriteString(w, `{"files":[]}`)
		case http.MethodPost:
			io.WriteString(w, `{"id":"created-folder"}`)
		default:
			t.Fatalf("unexpected method: %s", r.Method)
		}
	}))
	defer server.Close()

	svc, err := drive.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	id, err := ensureDriveFolder(context.Background(), svc, "shared-parent", "Archive", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "created-folder" {
		t.Fatalf("unexpected folder id: %s", id)
	}
	if len(seen) != 2 {
		t.Fatalf("expected list and create requests, got %#v", seen)
	}
	for _, request := range seen {
		if !strings.Contains(request, "supportsAllDrives=true") {
			t.Fatalf("shared drive request did not include supportsAllDrives=true: %#v", seen)
		}
	}
	if !strings.Contains(seen[0], "includeItemsFromAllDrives=true") {
		t.Fatalf("shared drive list request did not include includeItemsFromAllDrives=true: %#v", seen)
	}
}

func TestEnsureArchiveFolderUsesSelectedFolderAsRoot(t *testing.T) {
	created := make([]drive.File, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"files":[]}`)
		case http.MethodPost:
			var folder drive.File
			if err := json.NewDecoder(r.Body).Decode(&folder); err != nil {
				t.Errorf("decode folder request: %v", err)
			}
			created = append(created, folder)
			_, _ = fmt.Fprintf(w, `{"id":"folder-%d"}`, len(created))
		default:
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	svc, err := drive.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	uploader := GoogleDriveAPIUploader{Config: GoogleDriveConfig{
		FolderID: "selected-folder",
	}}
	started := time.Date(2026, 8, 12, 22, 9, 10, 0, time.FixedZone("JST", 9*60*60))
	folderID, err := uploader.ensureArchiveFolder(context.Background(), svc, "Dev", "stream-uuid", started)
	if err != nil {
		t.Fatal(err)
	}
	if folderID != "folder-2" || len(created) != 2 {
		t.Fatalf("archive hierarchy created unexpected folders: id=%q folders=%#v", folderID, created)
	}
	if created[0].Name != "Dev" || len(created[0].Parents) != 1 || created[0].Parents[0] != "selected-folder" {
		t.Fatalf("stream folder must be directly below selected folder: %#v", created[0])
	}
	if created[1].Name != "20260812_220910_JST_stream-uuid" || len(created[1].Parents) != 1 || created[1].Parents[0] != "folder-1" {
		t.Fatalf("run folder must be below stream folder: %#v", created[1])
	}
}

func TestUploadFileUpdatesExistingNameInsteadOfCreatingDuplicate(t *testing.T) {
	methods := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"files":[{"id":"existing-file","name":"final.mp4"}]}`)
		case http.MethodPatch:
			_, _ = io.WriteString(w, `{"id":"existing-file"}`)
		default:
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "duplicate create is forbidden", http.StatusConflict)
		}
	}))
	defer server.Close()

	svc, err := drive.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(t.TempDir(), "final.mp4")
	if err := os.WriteFile(filePath, []byte("new-video"), 0o640); err != nil {
		t.Fatal(err)
	}
	id, err := uploadFile(context.Background(), svc, "run-folder", File{LocalPath: filePath, DrivePath: "final.mp4"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if id != "existing-file" {
		t.Fatalf("updated file id = %q, want existing-file", id)
	}
	if len(methods) != 2 || !strings.HasPrefix(methods[0], "GET ") || !strings.HasPrefix(methods[1], "PATCH ") {
		t.Fatalf("upload should list then update without create: %#v", methods)
	}
}

func TestUploadFileUsesSharedDriveUploadOption(t *testing.T) {
	var uploadRequest string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploadRequest = r.Method + " " + r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"uploaded-file"}`)
	}))
	defer server.Close()

	svc, err := drive.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "final.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o640); err != nil {
		t.Fatal(err)
	}
	id, err := uploadFile(context.Background(), svc, "shared-folder", File{LocalPath: path, DrivePath: "final.mp4"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if id != "uploaded-file" {
		t.Fatalf("unexpected file id: %s", id)
	}
	if !strings.Contains(uploadRequest, "supportsAllDrives=true") {
		t.Fatalf("shared drive upload request did not include supportsAllDrives=true: %s", uploadRequest)
	}
}

func TestOAuthDriveUploadRefreshesTokenAndUsesBearer(t *testing.T) {
	tokenRequests := 0
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests++
		if r.Method != http.MethodPost {
			t.Errorf("unexpected token method: %s", r.Method)
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		clientID, clientSecret, basicAuth := r.BasicAuth()
		if r.Form.Get("grant_type") != "refresh_token" ||
			r.Form.Get("refresh_token") != "google-refresh-token" ||
			!basicAuth ||
			clientID != "google-client-id" ||
			clientSecret != "google-client-secret" {
			t.Errorf("unexpected token refresh request: form=%#v basicAuth=%t clientID=%q", r.Form, basicAuth, clientID)
			http.Error(w, "bad token refresh request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"ya29.fake-access-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokenServer.Close()

	var uploadAuthorization string
	driveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploadAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files") {
			io.WriteString(w, `{"files":[]}`)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload/drive/v3/files") {
			io.WriteString(w, `{"id":"uploaded-oauth-file"}`)
			return
		}
		t.Fatalf("unexpected Drive request: %s %s", r.Method, r.URL.String())
	}))
	defer driveServer.Close()

	uploader := GoogleDriveAPIUploader{Config: GoogleDriveConfig{
		AuthMode:     "oauth2",
		ClientID:     "google-client-id",
		ClientSecret: "google-client-secret",
		RefreshToken: "google-refresh-token",
		FolderID:     "folder",
	}}
	svc, err := uploader.driveServiceWithOptions(context.Background(), oauth2Endpoint(tokenServer.URL), option.WithEndpoint(driveServer.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(t.TempDir(), "final.mp4")
	if err := os.WriteFile(filePath, []byte("video"), 0o640); err != nil {
		t.Fatal(err)
	}
	id, err := uploadFile(context.Background(), svc, "folder", File{LocalPath: filePath, DrivePath: "final.mp4"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if id != "uploaded-oauth-file" {
		t.Fatalf("unexpected upload id: %s", id)
	}
	if tokenRequests != 1 {
		t.Fatalf("expected one OAuth token refresh, got %d", tokenRequests)
	}
	if uploadAuthorization != "Bearer ya29.fake-access-token" {
		t.Fatalf("Drive upload did not use refreshed bearer token: %q", uploadAuthorization)
	}
}

func oauth2Endpoint(tokenURL string) oauth2.Endpoint {
	return oauth2.Endpoint{TokenURL: tokenURL}
}
