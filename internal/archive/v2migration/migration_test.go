package v2migration

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const (
	testStreamA = "11111111-1111-4111-8111-111111111111"
	testStreamB = "22222222-2222-4222-8222-222222222222"
)

func TestArchiveMigrationBackupDryRunApplyIdempotenceRestore(t *testing.T) {
	root := t.TempDir()
	writeLegacy(t, root, testStreamA, "final.mp4", "video-a")
	writeLegacy(t, root, testStreamA, "metadata.json", "metadata-a")
	writeLegacy(t, root, testStreamB, "final.mp4", "video-b")
	if err := os.MkdirAll(filepath.Join(root, "final", testStreamB, "existing-run"), 0o750); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildPlan(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 3 || !plan.BasePathReferenceRetained {
		t.Fatalf("plan denominator=%d retained_base_path=%v", len(plan.Entries), plan.BasePathReferenceRetained)
	}
	wantRunID := "legacy-11111111111141118111111111111111"
	if got := filepath.Base(filepath.Dir(plan.Entries[0].DestinationRelative)); got != wantRunID {
		t.Fatalf("migration run id = %q, want Control Panel identity %q", got, wantRunID)
	}
	backupDir := filepath.Join(t.TempDir(), "backup")
	artifact, err := Backup(root, backupDir, plan)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.RecordCount != 3 || len(artifact.ManifestSHA256) != 64 {
		t.Fatalf("backup artifact=%#v", artifact)
	}
	if _, err := Backup(root, backupDir, plan); err != nil {
		t.Fatalf("immutable backup validation is not idempotent: %v", err)
	}
	if result, err := DryRun(root, plan, artifact); err != nil || result.PreCount != 3 || result.PostCount != 3 {
		t.Fatalf("dry-run result=%#v err=%v", result, err)
	}
	first, err := Apply(root, plan, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if first.PreCount != 3 || first.PostCount != 3 || first.OrphanCount != 0 || first.BackupStatus != "PASS" || first.Rollback != "PASS" {
		t.Fatalf("apply proof=%#v", first)
	}
	second, err := Apply(root, plan, artifact)
	if err != nil || second.Idempotence != "PASS" {
		t.Fatalf("idempotent rerun=%#v err=%v", second, err)
	}
	restored, err := Restore(root, plan, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if restored.RestoreStatus != "PASS" || restored.PreCount != restored.PostCount || restored.OrphanCount != 0 {
		t.Fatalf("restore proof=%#v", restored)
	}
	for _, entry := range plan.Entries {
		if _, _, err := hashMatches(root, entry.SourceRelative, entry); err != nil {
			t.Fatalf("restored source %q: %v", entry.SourceRelative, err)
		}
		if _, err := os.Lstat(filepath.Join(root, entry.DestinationRelative)); !os.IsNotExist(err) {
			t.Fatalf("migration destination remained after restore: %s", entry.DestinationRelative)
		}
	}
	if _, err := os.Lstat(filepath.Dir(filepath.Join(root, plan.Entries[0].DestinationRelative))); !os.IsNotExist(err) {
		t.Fatal("migration-created run directory remained after restore")
	}
}

func TestArchiveMigrationRejectsUnsafeEmptyMismatchAndOrphanStates(t *testing.T) {
	t.Run("empty expected denominator", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "final"), 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildPlan(root, true); !errors.Is(err, ErrEmptyDenominator) {
			t.Fatalf("error=%v, want ErrEmptyDenominator", err)
		}
	})

	t.Run("symlink source", func(t *testing.T) {
		root := t.TempDir()
		streamDir := filepath.Join(root, "final", testStreamA)
		if err := os.MkdirAll(streamDir, 0o750); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.mp4")
		if err := os.WriteFile(outside, []byte("outside"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(streamDir, "final.mp4")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := BuildPlan(root, true); err == nil {
			t.Fatal("symlink source unexpectedly accepted")
		}
	})

	t.Run("checksum mismatch", func(t *testing.T) {
		root := t.TempDir()
		writeLegacy(t, root, testStreamA, "final.mp4", "before")
		plan, err := BuildPlan(root, true)
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := Backup(root, filepath.Join(t.TempDir(), "backup"), plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, plan.Entries[0].SourceRelative), []byte("after"), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := Apply(root, plan, artifact); err == nil {
			t.Fatal("changed source unexpectedly migrated")
		}
	})

	t.Run("partial failure rolls back", func(t *testing.T) {
		root := t.TempDir()
		writeLegacy(t, root, testStreamA, "a.mp4", "a")
		writeLegacy(t, root, testStreamA, "b.mp4", "b")
		plan, err := BuildPlan(root, true)
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := Backup(root, filepath.Join(t.TempDir(), "backup"), plan)
		if err != nil {
			t.Fatal(err)
		}
		injected := errors.New("injected partial failure")
		if _, err := apply(root, plan, artifact, func(moved int) error {
			if moved == 1 {
				return injected
			}
			return nil
		}); !errors.Is(err, injected) {
			t.Fatalf("apply error=%v, want injected failure", err)
		}
		for _, entry := range plan.Entries {
			if _, _, err := hashMatches(root, entry.SourceRelative, entry); err != nil {
				t.Fatalf("partial rollback source %q: %v", entry.SourceRelative, err)
			}
		}
	})

	t.Run("orphan mismatch rejected", func(t *testing.T) {
		root := t.TempDir()
		writeLegacy(t, root, testStreamA, "final.mp4", "video")
		plan, err := BuildPlan(root, true)
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := Backup(root, filepath.Join(t.TempDir(), "backup"), plan)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Apply(root, plan, artifact); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, plan.Entries[0].SourceRelative), []byte("orphan"), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(root, plan, artifact); err == nil {
			t.Fatal("orphan source unexpectedly passed verification")
		}
	})
}

func TestLegacyArchiveRunIDRejectsNonCanonicalStreamDirectory(t *testing.T) {
	if _, err := legacyArchiveRunID("stream-a"); err == nil {
		t.Fatal("non-UUID legacy archive stream directory unexpectedly accepted")
	}
}

func writeLegacy(t *testing.T, root, streamID, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "final", streamID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}
