package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/example/autostream-encoder-recorder/internal/archive/v2migration"
)

type archiveV2MigrationOutput struct {
	Operation        string `json:"operation"`
	RecordCount      int    `json:"record_count"`
	ManifestSHA256   string `json:"manifest_sha256"`
	PreCount         int    `json:"pre_count"`
	PostCount        int    `json:"post_count"`
	OrphanCount      int    `json:"orphan_count"`
	BackupStatus     string `json:"backup_status"`
	RestoreStatus    string `json:"restore_status"`
	Idempotence      string `json:"idempotence"`
	Rollback         string `json:"rollback"`
	PhysicalDeletion bool   `json:"physical_deletion"`
}

func runArchiveV2Migration(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("archive-v2-migrate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	operation := flags.String("operation", "", "prepare, apply, verify, or restore")
	archiveRoot := flags.String("archive-root", "", "archive root")
	backupDir := flags.String("backup-dir", "", "immutable backup directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*archiveRoot) == "" || strings.TrimSpace(*backupDir) == "" {
		return errors.New("operation, archive-root, and backup-dir are required")
	}

	var (
		plan     v2migration.Plan
		artifact v2migration.Artifact
		result   v2migration.Result
		err      error
	)
	switch strings.TrimSpace(*operation) {
	case "prepare":
		plan, err = v2migration.BuildPlan(*archiveRoot, true)
		if err == nil {
			artifact, err = v2migration.Backup(*archiveRoot, *backupDir, plan)
		}
		if err == nil {
			result, err = v2migration.DryRun(*archiveRoot, plan, artifact)
		}
	case "apply", "verify", "restore":
		plan, artifact, err = v2migration.LoadBackup(*backupDir)
		if err == nil {
			switch *operation {
			case "apply":
				if _, err = v2migration.DryRun(*archiveRoot, plan, artifact); err == nil {
					result, err = v2migration.Apply(*archiveRoot, plan, artifact)
				}
			case "verify":
				result, err = v2migration.Verify(*archiveRoot, plan, artifact)
			case "restore":
				result, err = v2migration.Restore(*archiveRoot, plan, artifact)
			}
		}
	default:
		return fmt.Errorf("unsupported operation %q", *operation)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(archiveV2MigrationOutput{
		Operation:        *operation,
		RecordCount:      artifact.RecordCount,
		ManifestSHA256:   artifact.ManifestSHA256,
		PreCount:         result.PreCount,
		PostCount:        result.PostCount,
		OrphanCount:      result.OrphanCount,
		BackupStatus:     result.BackupStatus,
		RestoreStatus:    result.RestoreStatus,
		Idempotence:      result.Idempotence,
		Rollback:         result.Rollback,
		PhysicalDeletion: result.PhysicalDeletion,
	})
}
