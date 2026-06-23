package archive

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDryRunUploader(t *testing.T) {
	result, err := DryRunUploader{}.Upload(context.Background(), "stream", "s1", time.Now(), []File{{LocalPath: "final.mp4", DrivePath: "final.mp4"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || result.FileIDs["final.mp4"] == "" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestUploadResultMarshalJSONRedactsDriveIDs(t *testing.T) {
	result := UploadResult{
		DryRun:   false,
		FolderID: "shared-drive-folder-secret-id",
		FileIDs: map[string]string{
			"final.mp4":     "drive-file-secret-id-1",
			"metadata.json": "drive-file-secret-id-2",
		},
		Attempts: 2,
	}

	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(body)
	for _, leaked := range []string{
		"shared-drive-folder-secret-id",
		"drive-file-secret-id-1",
		"drive-file-secret-id-2",
		`"folder_id"`,
		`"file_ids"`,
	} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("upload result leaked raw Drive ID marker %q in %s", leaked, raw)
		}
	}
	for _, want := range []string{
		`"folder_id_configured":true`,
		`"folder_id_fingerprint":"sha256:`,
		`"file_count":2`,
		`"file_fingerprints"`,
		`"attempts":2`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("upload result JSON missing public marker %q in %s", want, raw)
		}
	}
}
