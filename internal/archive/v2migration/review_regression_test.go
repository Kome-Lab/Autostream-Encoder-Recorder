package v2migration

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// These cases exercise the review's data-loss and false-positive paths through
// the public migration API. The snapshot is taken before the rejected operation.
func TestReviewRestorePreservesConflicts(t *testing.T) {
	for _, scenario := range []string{"existing destination", "identical existing destination", "changed destination", "later destination conflict", "later source conflict"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			writeLegacy(t, root, testStreamA, "a.mp4", "first")
			writeLegacy(t, root, testStreamA, "z.mp4", "last")
			if scenario == "existing destination" || scenario == "identical existing destination" {
				body := "unowned"
				if scenario == "identical existing destination" {
					body = "first"
				}
				writeReviewFile(t, root, filepath.Join("final", testStreamA, "legacy-11111111111141118111111111111111", "a.mp4"), body)
			}
			plan, artifact := reviewBackup(t, root)
			if scenario == "existing destination" || scenario == "identical existing destination" {
				if _, err := Apply(root, plan, artifact); err == nil {
					t.Fatal("Apply accepted collision")
				}
			} else {
				if _, err := Apply(root, plan, artifact); err != nil {
					t.Fatal(err)
				}
				index := 0
				if scenario == "later destination conflict" || scenario == "later source conflict" {
					index = 1
				}
				relative := plan.Entries[index].DestinationRelative
				if scenario == "later source conflict" {
					relative = plan.Entries[index].SourceRelative
				}
				writeReviewFile(t, root, relative, "changed after apply")
			}
			before := reviewTree(t, root)
			if _, err := Restore(root, plan, artifact); err == nil {
				t.Error("Restore accepted conflict")
			}
			if after := reviewTree(t, root); !reflect.DeepEqual(before, after) {
				t.Error("Restore changed archive before rejecting conflict")
			}
		})
	}
}

func TestReviewDryRunChecksLiveStateWithoutMutation(t *testing.T) {
	for _, scenario := range []string{"missing both", "source drift", "collision", "source directory", "destination directory"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			writeLegacy(t, root, testStreamA, "a.mp4", "first")
			plan, artifact := reviewBackup(t, root)
			entry := plan.Entries[0]
			switch scenario {
			case "missing both", "source directory":
				if err := os.Remove(filepath.Join(root, entry.SourceRelative)); err != nil {
					t.Fatal(err)
				}
				if scenario == "source directory" {
					if err := os.Mkdir(filepath.Join(root, entry.SourceRelative), 0o750); err != nil {
						t.Fatal(err)
					}
				}
			case "source drift":
				writeReviewFile(t, root, entry.SourceRelative, "drift")
			case "collision":
				writeReviewFile(t, root, entry.DestinationRelative, "first")
			case "destination directory":
				if err := os.MkdirAll(filepath.Join(root, entry.DestinationRelative), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			before := reviewTree(t, root)
			if _, err := DryRun(root, plan, artifact); err == nil {
				t.Error("DryRun accepted invalid live state")
			}
			if after := reviewTree(t, root); !reflect.DeepEqual(before, after) {
				t.Error("DryRun mutated archive")
			}
		})
	}
}

func TestReviewApplyAndRestoreReplay(t *testing.T) {
	root := t.TempDir()
	writeLegacy(t, root, testStreamA, "a.mp4", "first")
	// A pre-existing directory is not proof that its new file is unowned.
	if err := os.MkdirAll(filepath.Join(root, "final", testStreamA, "legacy-11111111111141118111111111111111"), 0o750); err != nil {
		t.Fatal(err)
	}
	before := reviewTree(t, root)
	plan, artifact := reviewBackup(t, root)
	for i := 0; i < 2; i++ {
		if _, err := DryRun(root, plan, artifact); err != nil {
			t.Fatal(err)
		}
		if _, err := Apply(root, plan, artifact); err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(root, plan, artifact); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := Restore(root, plan, artifact); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(before, reviewTree(t, root)) {
		t.Fatal("round trip changed original archive")
	}
}

func TestReviewPreexistingFileNeverBecomesOwnedBySourceRemoval(t *testing.T) {
	root := t.TempDir()
	writeLegacy(t, root, testStreamA, "a.mp4", "same bytes")
	destination := filepath.Join("final", testStreamA, "legacy-11111111111141118111111111111111", "a.mp4")
	writeReviewFile(t, root, destination, "same bytes")
	plan, artifact := reviewBackup(t, root)
	if err := os.Remove(filepath.Join(root, plan.Entries[0].SourceRelative)); err != nil {
		t.Fatal(err)
	}
	before := reviewTree(t, root)
	if _, err := DryRun(root, plan, artifact); err == nil {
		t.Fatal("DryRun claimed a pre-existing file")
	}
	if _, err := Restore(root, plan, artifact); err == nil {
		t.Fatal("Restore claimed a pre-existing file")
	}
	if !reflect.DeepEqual(before, reviewTree(t, root)) {
		t.Fatal("pre-existing destination was changed")
	}
}

func reviewBackup(t *testing.T, root string) (Plan, Artifact) {
	t.Helper()
	plan, err := BuildPlan(root, true)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := Backup(root, filepath.Join(t.TempDir(), "backup"), plan)
	if err != nil {
		t.Fatal(err)
	}
	return plan, artifact
}

func writeReviewFile(t *testing.T, root, relative, body string) {
	t.Helper()
	file := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

func reviewTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			result[relative] = "directory"
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[relative] = "file:" + string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
