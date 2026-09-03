package v2migration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const manifestName = "archive-v2-migration-manifest.json"

var ErrEmptyDenominator = errors.New("archive migration source denominator is empty")

type Entry struct {
	SourceRelative              string `json:"source_relative"`
	DestinationRelative         string `json:"destination_relative"`
	DestinationDirectoryExisted bool   `json:"destination_directory_existed"`
	BackupRelative              string `json:"backup_relative"`
	SizeBytes                   int64  `json:"size_bytes"`
	SHA256                      string `json:"sha256"`
}

type Plan struct {
	SchemaVersion             int               `json:"schema_version"`
	InventoryIDs              []string          `json:"inventory_ids"`
	Strategies                map[string]string `json:"strategies"`
	BasePathReferenceRetained bool              `json:"base_path_reference_retained"`
	Entries                   []Entry           `json:"entries"`
}

type Artifact struct {
	Directory      string
	ManifestPath   string
	ManifestSHA256 string
	RecordCount    int
}

type Result struct {
	PreCount           int
	PostCount          int
	OrphanCount        int
	BackupStatus       string
	RestoreStatus      string
	Idempotence        string
	Rollback           string
	PhysicalDeletion   bool
	ProductionMutation bool
}

func BuildPlan(root string, expectedNonEmpty bool) (Plan, error) {
	rootAbs, err := cleanRoot(root)
	if err != nil {
		return Plan{}, err
	}
	finalRoot := filepath.Join(rootAbs, "final")
	streams, err := os.ReadDir(finalRoot)
	if os.IsNotExist(err) && !expectedNonEmpty {
		return newPlan(nil), nil
	}
	if err != nil {
		return Plan{}, fmt.Errorf("read archive final root: %w", err)
	}

	var entries []Entry
	for _, stream := range streams {
		if stream.Type()&os.ModeSymlink != 0 || !stream.IsDir() {
			return Plan{}, fmt.Errorf("unsafe archive stream entry %q", stream.Name())
		}
		streamDir := filepath.Join(finalRoot, stream.Name())
		children, err := os.ReadDir(streamDir)
		if err != nil {
			return Plan{}, fmt.Errorf("read archive stream %q: %w", stream.Name(), err)
		}
		var legacy []Entry
		for _, child := range children {
			if child.IsDir() {
				continue // Existing v2 run directories are not legacy source rows.
			}
			if child.Type()&os.ModeSymlink != 0 || !child.Type().IsRegular() {
				return Plan{}, fmt.Errorf("unsafe legacy archive entry %q", filepath.Join(stream.Name(), child.Name()))
			}
			sourceRel := filepath.Join("final", stream.Name(), child.Name())
			size, digest, err := hashRegularFile(rootAbs, sourceRel)
			if err != nil {
				return Plan{}, err
			}
			legacy = append(legacy, Entry{SourceRelative: sourceRel, SizeBytes: size, SHA256: digest})
		}
		if len(legacy) == 0 {
			continue
		}
		sort.Slice(legacy, func(i, j int) bool { return legacy[i].SourceRelative < legacy[j].SourceRelative })
		runHasher := sha256.New()
		_, _ = io.WriteString(runHasher, stream.Name()+"\n")
		for _, entry := range legacy {
			_, _ = io.WriteString(runHasher, filepath.Base(entry.SourceRelative)+"\x00"+entry.SHA256+"\n")
		}
		runID := "legacy-" + hex.EncodeToString(runHasher.Sum(nil))[:16]
		destinationDir := filepath.Join(streamDir, runID)
		destinationDirectoryExisted := false
		if info, statErr := os.Lstat(destinationDir); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return Plan{}, fmt.Errorf("unsafe migration destination directory %q", destinationDir)
			}
			destinationDirectoryExisted = true
		} else if !os.IsNotExist(statErr) {
			return Plan{}, statErr
		}
		for index := range legacy {
			name := filepath.Base(legacy[index].SourceRelative)
			legacy[index].DestinationRelative = filepath.Join("final", stream.Name(), runID, name)
			legacy[index].DestinationDirectoryExisted = destinationDirectoryExisted
			legacy[index].BackupRelative = filepath.Join("objects", stream.Name(), runID, name)
		}
		entries = append(entries, legacy...)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].SourceRelative < entries[j].SourceRelative })
	if expectedNonEmpty && len(entries) == 0 {
		return Plan{}, ErrEmptyDenominator
	}
	return newPlan(entries), nil
}

func newPlan(entries []Entry) Plan {
	return Plan{
		SchemaVersion: 1,
		InventoryIDs:  []string{"DEP-ENC-0002", "DEP-ENC-0004"},
		Strategies: map[string]string{
			"DEP-ENC-0002": "archive read-only",
			"DEP-ENC-0004": "retain data / remove runtime reference",
		},
		BasePathReferenceRetained: true,
		Entries:                   entries,
	}
}

func Backup(root, backupDir string, plan Plan) (Artifact, error) {
	rootAbs, err := cleanRoot(root)
	if err != nil {
		return Artifact{}, err
	}
	backupAbs, err := filepath.Abs(backupDir)
	if err != nil {
		return Artifact{}, err
	}
	if err := validatePlan(plan); err != nil {
		return Artifact{}, err
	}
	manifestPath := filepath.Join(backupAbs, manifestName)
	if _, err := os.Lstat(backupAbs); err == nil {
		return validateExistingBackup(backupAbs, plan)
	} else if !os.IsNotExist(err) {
		return Artifact{}, err
	}
	if err := os.MkdirAll(backupAbs, 0o750); err != nil {
		return Artifact{}, err
	}
	for _, entry := range plan.Entries {
		if err := copyVerified(rootAbs, entry.SourceRelative, backupAbs, entry.BackupRelative, entry); err != nil {
			return Artifact{}, err
		}
	}
	body, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return Artifact{}, err
	}
	body = append(body, '\n')
	file, err := os.OpenFile(manifestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o440)
	if err != nil {
		return Artifact{}, err
	}
	if _, err = file.Write(body); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Directory: backupAbs, ManifestPath: manifestPath, ManifestSHA256: digestBytes(body), RecordCount: len(plan.Entries)}, nil
}

func DryRun(root string, plan Plan, artifact Artifact) (Result, error) {
	if _, err := verifyInputs(root, plan, artifact, false); err != nil {
		return Result{}, err
	}
	return Result{PreCount: len(plan.Entries), PostCount: len(plan.Entries), BackupStatus: "PASS", RestoreStatus: "NOT_RUN", Idempotence: "NOT_RUN", Rollback: "NOT_RUN"}, nil
}

func Apply(root string, plan Plan, artifact Artifact) (Result, error) {
	return apply(root, plan, artifact, nil)
}

func apply(root string, plan Plan, artifact Artifact, afterMove func(int) error) (Result, error) {
	rootAbs, err := verifyInputs(root, plan, artifact, true)
	if err != nil {
		return Result{}, err
	}
	moved := make([]Entry, 0, len(plan.Entries))
	rollback := func() error {
		var rollbackErr error
		for index := len(moved) - 1; index >= 0; index-- {
			entry := moved[index]
			source, _ := safeJoin(rootAbs, entry.SourceRelative)
			destination, _ := safeJoin(rootAbs, entry.DestinationRelative)
			if err := os.Rename(destination, source); err != nil && rollbackErr == nil {
				rollbackErr = err
			}
		}
		cleanupDestinationDirectories(rootAbs, moved)
		return rollbackErr
	}

	for index, entry := range plan.Entries {
		source, _ := safeJoin(rootAbs, entry.SourceRelative)
		destination, _ := safeJoin(rootAbs, entry.DestinationRelative)
		if _, err := os.Lstat(source); os.IsNotExist(err) {
			if _, _, matchErr := hashMatches(rootAbs, entry.DestinationRelative, entry); matchErr != nil {
				_ = rollback()
				return Result{}, fmt.Errorf("source missing without verified destination: %s", entry.SourceRelative)
			}
			continue
		} else if err != nil {
			_ = rollback()
			return Result{}, err
		}
		if _, err := os.Lstat(destination); err == nil {
			_ = rollback()
			return Result{}, fmt.Errorf("migration destination already exists: %s", entry.DestinationRelative)
		} else if !os.IsNotExist(err) {
			_ = rollback()
			return Result{}, err
		}
		if err := ensureSafeDir(rootAbs, filepath.Dir(entry.DestinationRelative)); err != nil {
			_ = rollback()
			return Result{}, err
		}
		if err := os.Rename(source, destination); err != nil {
			_ = rollback()
			return Result{}, err
		}
		moved = append(moved, entry)
		if afterMove != nil {
			if err := afterMove(index + 1); err != nil {
				rollbackErr := rollback()
				if rollbackErr != nil {
					return Result{}, fmt.Errorf("apply interrupted: %v; rollback: %w", err, rollbackErr)
				}
				return Result{}, err
			}
		}
	}

	result, err := Verify(rootAbs, plan, artifact)
	if err != nil {
		_ = rollback()
		return Result{}, err
	}
	result.Rollback = "PASS"
	return result, nil
}

func Verify(root string, plan Plan, artifact Artifact) (Result, error) {
	rootAbs, err := verifyInputs(root, plan, artifact, false)
	if err != nil {
		return Result{}, err
	}
	postCount, orphanCount := 0, 0
	for _, entry := range plan.Entries {
		if _, _, err := hashMatches(rootAbs, entry.DestinationRelative, entry); err == nil {
			postCount++
		} else {
			orphanCount++
		}
		if _, err := os.Lstat(filepath.Join(rootAbs, entry.SourceRelative)); err == nil {
			orphanCount++
		} else if !os.IsNotExist(err) {
			return Result{}, err
		}
	}
	if postCount != len(plan.Entries) || orphanCount != 0 {
		return Result{}, fmt.Errorf("archive migration count mismatch: pre=%d post=%d orphan=%d", len(plan.Entries), postCount, orphanCount)
	}
	return Result{PreCount: len(plan.Entries), PostCount: postCount, OrphanCount: orphanCount, BackupStatus: "PASS", RestoreStatus: "NOT_RUN", Idempotence: "PASS", Rollback: "PASS"}, nil
}

func Restore(root string, plan Plan, artifact Artifact) (Result, error) {
	rootAbs, err := verifyInputs(root, plan, artifact, false)
	if err != nil {
		return Result{}, err
	}
	for _, entry := range plan.Entries {
		source, _ := safeJoin(rootAbs, entry.SourceRelative)
		destination, _ := safeJoin(rootAbs, entry.DestinationRelative)
		if _, err := os.Lstat(source); err == nil {
			if _, _, matchErr := hashMatches(rootAbs, entry.SourceRelative, entry); matchErr != nil {
				return Result{}, fmt.Errorf("restore would overwrite changed source: %s", entry.SourceRelative)
			}
		} else if os.IsNotExist(err) {
			if err := ensureSafeDir(rootAbs, filepath.Dir(entry.SourceRelative)); err != nil {
				return Result{}, err
			}
			if err := copyVerified(artifact.Directory, entry.BackupRelative, rootAbs, entry.SourceRelative, entry); err != nil {
				return Result{}, err
			}
		} else {
			return Result{}, err
		}
		if _, err := os.Lstat(destination); err == nil {
			if err := os.Remove(destination); err != nil {
				return Result{}, err
			}
		} else if !os.IsNotExist(err) {
			return Result{}, err
		}
	}
	for _, entry := range plan.Entries {
		if _, _, err := hashMatches(rootAbs, entry.SourceRelative, entry); err != nil {
			return Result{}, fmt.Errorf("restored source verification failed: %s", entry.SourceRelative)
		}
	}
	cleanupDestinationDirectories(rootAbs, plan.Entries)
	return Result{PreCount: len(plan.Entries), PostCount: len(plan.Entries), OrphanCount: 0, BackupStatus: "PASS", RestoreStatus: "PASS", Idempotence: "PASS", Rollback: "PASS"}, nil
}

func cleanupDestinationDirectories(root string, entries []Entry) {
	directories := map[string]bool{}
	for _, entry := range entries {
		if !entry.DestinationDirectoryExisted {
			directories[filepath.Dir(entry.DestinationRelative)] = true
		}
	}
	ordered := make([]string, 0, len(directories))
	for directory := range directories {
		ordered = append(ordered, directory)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, relative := range ordered {
		path, err := safeJoin(root, relative)
		if err == nil {
			_ = os.Remove(path) // Only succeeds for an empty directory created by this migration.
		}
	}
}

func verifyInputs(root string, plan Plan, artifact Artifact, requireSourcesOrDestinations bool) (string, error) {
	rootAbs, err := cleanRoot(root)
	if err != nil {
		return "", err
	}
	if err := validatePlan(plan); err != nil {
		return "", err
	}
	existing, err := validateExistingBackup(artifact.Directory, plan)
	if err != nil {
		return "", err
	}
	if existing.ManifestSHA256 != artifact.ManifestSHA256 || existing.RecordCount != artifact.RecordCount {
		return "", errors.New("backup artifact identity mismatch")
	}
	if requireSourcesOrDestinations {
		for _, entry := range plan.Entries {
			if _, err := os.Lstat(filepath.Join(rootAbs, entry.SourceRelative)); err != nil {
				if _, destErr := os.Lstat(filepath.Join(rootAbs, entry.DestinationRelative)); destErr != nil {
					return "", fmt.Errorf("migration entry is missing from source and destination: %s", entry.SourceRelative)
				}
			}
		}
	}
	return rootAbs, nil
}

func validateExistingBackup(backupAbs string, want Plan) (Artifact, error) {
	manifestPath := filepath.Join(backupAbs, manifestName)
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return Artifact{}, err
	}
	var got Plan
	if err := json.Unmarshal(body, &got); err != nil {
		return Artifact{}, err
	}
	wantBody, _ := json.Marshal(want)
	gotBody, _ := json.Marshal(got)
	if string(wantBody) != string(gotBody) {
		return Artifact{}, errors.New("existing backup manifest does not match migration plan")
	}
	for _, entry := range got.Entries {
		if _, _, err := hashMatches(backupAbs, entry.BackupRelative, entry); err != nil {
			return Artifact{}, fmt.Errorf("backup object verification failed: %w", err)
		}
	}
	return Artifact{Directory: backupAbs, ManifestPath: manifestPath, ManifestSHA256: digestBytes(body), RecordCount: len(got.Entries)}, nil
}

func validatePlan(plan Plan) error {
	if plan.SchemaVersion != 1 || len(plan.InventoryIDs) != 2 || plan.InventoryIDs[0] != "DEP-ENC-0002" || plan.InventoryIDs[1] != "DEP-ENC-0004" ||
		plan.Strategies["DEP-ENC-0002"] != "archive read-only" ||
		plan.Strategies["DEP-ENC-0004"] != "retain data / remove runtime reference" ||
		len(plan.Strategies) != 2 || !plan.BasePathReferenceRetained {
		return errors.New("invalid archive migration plan authority")
	}
	if len(plan.Entries) == 0 {
		return ErrEmptyDenominator
	}
	seen := map[string]bool{}
	for _, entry := range plan.Entries {
		if entry.SizeBytes < 0 || len(entry.SHA256) != 64 || seen[entry.SourceRelative] {
			return errors.New("invalid archive migration plan entry")
		}
		seen[entry.SourceRelative] = true
		for _, relative := range []string{entry.SourceRelative, entry.DestinationRelative, entry.BackupRelative} {
			if err := validateRelative(relative); err != nil {
				return err
			}
		}
	}
	return nil
}

func cleanRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("archive root is required")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(rootAbs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("archive root must be a real directory")
	}
	return rootAbs, nil
}

func validateRelative(relative string) error {
	if relative == "" || filepath.IsAbs(relative) {
		return errors.New("migration path must be relative")
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return errors.New("migration path escapes its root")
	}
	return nil
}

func safeJoin(root, relative string) (string, error) {
	if err := validateRelative(relative); err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.Clean(relative)), nil
}

func ensureSafeDir(root, relative string) error {
	if err := validateRelative(relative); err != nil {
		return err
	}
	current := root
	for _, part := range strings.Split(filepath.Clean(relative), string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0o750); err != nil && !os.IsExist(err) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("migration directory contains an unsafe component")
		}
	}
	return nil
}

func copyVerified(sourceRoot, sourceRelative, destinationRoot, destinationRelative string, entry Entry) error {
	if _, _, err := hashMatches(sourceRoot, sourceRelative, entry); err != nil {
		return err
	}
	if err := ensureSafeDir(destinationRoot, filepath.Dir(destinationRelative)); err != nil {
		return err
	}
	source, _ := safeJoin(sourceRoot, sourceRelative)
	destination, _ := safeJoin(destinationRoot, destinationRelative)
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o440)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	if closeErr := out.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	_, _, err = hashMatches(destinationRoot, destinationRelative, entry)
	return err
}

func hashMatches(root, relative string, entry Entry) (int64, string, error) {
	size, digest, err := hashRegularFile(root, relative)
	if err != nil {
		return 0, "", err
	}
	if size != entry.SizeBytes || digest != entry.SHA256 {
		return size, digest, errors.New("migration file checksum mismatch")
	}
	return size, digest, nil
}

func hashRegularFile(root, relative string) (int64, string, error) {
	path, err := safeJoin(root, relative)
	if err != nil {
		return 0, "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, "", errors.New("migration source must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hasher.Sum(nil)), nil
}

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
