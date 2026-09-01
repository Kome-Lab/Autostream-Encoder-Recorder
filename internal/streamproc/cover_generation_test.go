package streamproc

import (
	"os"
	"testing"
)

func TestCoverGenerationRecordRejectsUnknownPersistentFields(t *testing.T) {
	manager := &Manager{ArchiveRoot: t.TempDir()}
	if generation, err := manager.nextCoverGeneration("stream-cover", 7); err != nil || generation != 1 {
		t.Fatalf("initial generation=%d err=%v", generation, err)
	}
	_, _, path, err := manager.coverGenerationPaths("stream-cover")
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(`{"stream_id":"stream-cover","job_generation":7,"generation":1,"storage_path":"C:/secret"}`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := &Manager{ArchiveRoot: manager.ArchiveRoot}
	if _, err := restarted.nextCoverGeneration("stream-cover", 7); err == nil {
		t.Fatal("unknown durable generation fields must fail closed")
	}
}
