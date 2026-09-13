package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncoderRecorderInstallerTransactionsPrivilegedHostSetup(t *testing.T) {
	root := filepath.Join("..", "..")
	installerBytes, err := os.ReadFile(filepath.Join(root, "release", "install-autostream-encoder-recorder"))
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerBytes)

	for _, marker := range []string{
		"rollback_created_autostream_account()",
		"rollback_journaled_directories()",
		"rollback_created_release()",
		"install_journaled_directory()",
		"create_autostream_group_transactionally()",
		"create_autostream_user_transactionally()",
		"prepare_autostream_user_rollback_login()",
		"local_group_database_references_are_safe()",
		"calculate_local_group_database_digest()",
		"local_group_database_digests_match()",
		"restore_renamed_autostream_user()",
		"remove_created_autostream_user_preserving_group()",
		"restore_existing_archive_directory()",
		"snapshot_existing_archive_directory()",
		"register_temporary_path()",
		"create_registered_temporary_path()",
		"INPUT_STAGE is the single temporary-path journal exception",
		"input_stage_is_owned()",
		"INPUT_STAGE_IDENTITY",
		"restore_legacy_backup_state()",
		"created_autostream_user=false",
		"created_autostream_user_group_record",
		`readonly AUTOSTREAM_USER_ROLLBACK_LOGIN="autostream-install-rollback"`,
		"autostream_user_rollback_login_ready",
		"created_autostream_group=false",
		"release_created=false",
		"archive_directory_mutation_started=false",
		"archive_directory_previous_identity",
		"backup_previous_kind",
		"backup_created_identity",
		`created_identity="${journaled_directory_created_identity["${STATE_DIR}"]-}"`,
		`created_identity="${journaled_directory_created_identity["${ARCHIVE_DIR}"]-}"`,
		`managed_cleanup_identity="${temporary_path_identity["${managed_candidate}"]-}"`,
		"cleanup_running=true",
		"signal_transaction_active=false",
		"deferred_termination_status=0",
		"handle_installer_signal()",
		"begin_installer_signal_transaction()",
		"finish_installer_signal_transaction()",
		`usermod --login "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" autostream`,
		`usermod --login autostream "${AUTOSTREAM_USER_ROLLBACK_LOGIN}"`,
		`userdel "${AUTOSTREAM_USER_ROLLBACK_LOGIN}"`,
		"function list_contains(value, count, member_index, names)",
		`FILENAME == "/etc/group" && NF >= 4 && list_contains($4)`,
		`FILENAME == "/etc/gshadow" && NF >= 4 &&`,
		`(list_contains($3) || list_contains($4))`,
		`group_content_digest="$(calculate_local_group_database_digest /etc/group)" || return 1`,
		`gshadow_content_digest="$(calculate_local_group_database_digest /etc/gshadow)" || return 1`,
		`exit "${input_stage_status}"`,
		"could not journal the published managed release identity",
		`readonly SHARED_HOST_SETUP_LOCK="/run/autostream-updater/.autostream-runtime-host-setup.lock"`,
		`exec 8<>"${SHARED_HOST_SETUP_LOCK}"`,
		`-f ${SHARED_HOST_SETUP_LOCK_FD_PATH} &&`,
		`$(stat -Lc '%U:%G:%a' -- "${SHARED_HOST_SETUP_LOCK_FD_PATH}") == "root:root:600"`,
		`flock -n 8`,
		"another AutoStream installer is provisioning shared host state",
		"shared host-setup lock identity changed after acquisition",
		`exec 9<>"${TARGET_LOCK}"`,
		`readonly TARGET_LOCK_FD_PATH="/proc/self/fd/9"`,
		`-f ${TARGET_LOCK_FD_PATH} &&`,
		`$(stat -Lc '%U:%G:%a' -- "${TARGET_LOCK_FD_PATH}") == "root:root:600"`,
		"updater lock descriptor/path identity changed",
		"permanent updater lock",
		"durable recovery backup",
	} {
		if !strings.Contains(installer, marker) {
			t.Fatalf("installer is missing privileged transaction marker %q", marker)
		}
	}
	if strings.Contains(installer, "usermod --gid") ||
		strings.Contains(installer, "usermod --home") {
		t.Fatal("service-account rollback must not mutate the created user's GID or home")
	}
	if strings.Contains(installer, "for (index =") {
		t.Fatal("service-account group scan must not use awk's reserved index builtin as an iterator")
	}
	if strings.Contains(installer, "id -Gn autostream") {
		t.Fatal("service-account rollback must inspect exact local group and gshadow fields instead of relying on resolved group names")
	}
	if count := strings.Count(installer, "local_group_database_references_are_safe || return 1"); count != 2 {
		t.Fatalf("service-account transaction must reject local group references before creation and rollback, got %d checks", count)
	}
	if count := strings.Count(installer, "local_group_database_digests_match \\"); count != 3 {
		t.Fatalf("service-account rollback must verify group database digests after rename, restoration, and deletion, got %d checks", count)
	}
	restoreStart := strings.Index(installer, "restore_renamed_autostream_user() {")
	restoreEndOffset := -1
	if restoreStart >= 0 {
		restoreEndOffset = strings.Index(installer[restoreStart:], "\n}\n\nremove_created_autostream_user_preserving_group()")
	}
	if restoreStart < 0 || restoreEndOffset < 0 {
		t.Fatal("could not isolate the renamed service-account restoration helper")
	}
	restoreBody := installer[restoreStart : restoreStart+restoreEndOffset]
	restoreRenameIndex := strings.Index(restoreBody, `usermod --login autostream "${AUTOSTREAM_USER_ROLLBACK_LOGIN}"`)
	restoreDigestIndex := strings.Index(restoreBody, "local_group_database_digests_match")
	if restoreRenameIndex < 0 || restoreDigestIndex <= restoreRenameIndex {
		t.Fatal("renamed service-account restoration must rename the login back before checking group database digests")
	}
	if strings.Contains(installer, `exec 9>"${TARGET_LOCK}"`) {
		t.Fatal("installer must not truncate the production updater lock")
	}
	if strings.Contains(installer, `stat -Lc '%F:%U:%G:%a'`) {
		t.Fatal("installer must not depend on GNU stat's size-sensitive regular-file description")
	}
	if strings.Contains(installer, `rm -f -- "${SHARED_HOST_SETUP_LOCK}"`) {
		t.Fatal("installer must never unlink the permanent shared host-setup lock")
	}
	if strings.Contains(installer, `rm -f -- "${TARGET_LOCK}"`) {
		t.Fatal("installer must never unlink the permanent production updater lock")
	}
	if count := strings.Count(installer, "trap '' HUP INT TERM"); count != 4 {
		t.Fatalf("installer may ignore termination signals only in immediate-exit and cleanup shielding, got %d sites", count)
	}
	for _, forbidden := range []string{
		`$(create_registered_temporary_path`,
		`$(install_journaled_directory`,
	} {
		if strings.Contains(installer, forbidden) {
			t.Fatalf("installer must update rollback journals in the parent shell, found %q", forbidden)
		}
	}
	sharedLockIndex := strings.Index(installer, "flock -n 8")
	firstJournaledAnchorIndex := strings.Index(installer, "ensure_root_anchor_directory /usr\n")
	if sharedLockIndex < 0 || firstJournaledAnchorIndex <= sharedLockIndex {
		t.Fatal("installer must acquire the shared host-setup lock before journaled host mutations")
	}
	cleanupStart := strings.Index(installer, "cleanup() {")
	if cleanupStart < 0 {
		t.Fatal("installer cleanup function is missing")
	}
	cleanupEnd := strings.Index(installer[cleanupStart:], "\n}\ntrap cleanup EXIT")
	if cleanupEnd < 0 {
		t.Fatal("installer cleanup function boundary is missing")
	}
	cleanup := installer[cleanupStart : cleanupStart+cleanupEnd]
	cleanupMaskIndex := strings.Index(cleanup, "trap '' HUP INT TERM")
	cleanupTrapRemovalIndex := strings.Index(cleanup, "trap - EXIT")
	cleanupRollbackIndex := strings.Index(cleanup, "rollback_activation")
	if cleanupMaskIndex < 0 ||
		cleanupTrapRemovalIndex <= cleanupMaskIndex ||
		cleanupRollbackIndex <= cleanupTrapRemovalIndex {
		t.Fatal("installer cleanup must mask repeated termination signals before removing its EXIT trap and rolling back")
	}
	enclosingIfConditions := func(marker string) []string {
		t.Helper()
		var conditions []string
		pendingCondition := ""
		for _, line := range strings.Split(cleanup, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.Contains(trimmed, marker) {
				return append([]string(nil), conditions...)
			}
			if pendingCondition != "" {
				pendingCondition += " " + trimmed
				if strings.HasSuffix(trimmed, "then") {
					conditions = append(conditions, pendingCondition)
					pendingCondition = ""
				}
				continue
			}
			if strings.HasPrefix(trimmed, "elif ") {
				if len(conditions) == 0 {
					t.Fatalf("installer cleanup has an unmatched elif before %q", marker)
				}
				conditions = conditions[:len(conditions)-1]
				pendingCondition = trimmed
				if strings.HasSuffix(trimmed, "then") {
					conditions = append(conditions, pendingCondition)
					pendingCondition = ""
				}
				continue
			}
			if strings.HasPrefix(trimmed, "if ") {
				pendingCondition = trimmed
				if strings.HasSuffix(trimmed, "then") {
					conditions = append(conditions, pendingCondition)
					pendingCondition = ""
				}
				continue
			}
			if trimmed == "fi" {
				if len(conditions) == 0 {
					t.Fatalf("installer cleanup has an unmatched fi before %q", marker)
				}
				conditions = conditions[:len(conditions)-1]
			}
		}
		t.Fatalf("installer cleanup operation is missing %q", marker)
		return nil
	}
	directoryRollbackConditions := strings.Join(enclosingIfConditions("rollback_journaled_directories"), "\n")
	if !strings.Contains(directoryRollbackConditions, "${status} -ne 0") ||
		!strings.Contains(directoryRollbackConditions, "${installation_complete} != true") ||
		strings.Contains(directoryRollbackConditions, "setup_rollback_safe") {
		t.Fatal("installer cleanup must attempt inode-guarded journaled-directory restoration after every failed incomplete install")
	}
	assertSetupRollbackGate := func(operation string) {
		t.Helper()
		conditions := strings.Join(enclosingIfConditions(operation), "\n")
		if !strings.Contains(conditions, "setup_rollback_safe") {
			t.Fatalf("installer cleanup operation %q must retain its shared setup-safety gate", operation)
		}
	}
	assertSetupRollbackGate("rollback_created_release")
	assertSetupRollbackGate("rollback_created_autostream_account")
	for _, transaction := range []struct {
		start string
		end   string
		steps []string
	}{
		{
			start: "create_autostream_group_transactionally() {",
			end:   "\n}\n\ncreate_autostream_user_transactionally()",
			steps: []string{
				"begin_installer_signal_transaction",
				"groupadd --system autostream",
				"created_autostream_group=true",
				`created_autostream_group_record="${created_record}"`,
				"finish_installer_signal_transaction",
			},
		},
		{
			start: "create_autostream_user_transactionally() {",
			end:   "\n}\n\nsnapshot_existing_state_directory()",
			steps: []string{
				"begin_installer_signal_transaction",
				"useradd --system",
				"created_autostream_user=true",
				`created_autostream_user_record="${created_record}"`,
				"finish_installer_signal_transaction",
			},
		},
		{
			start: "install_journaled_directory() {",
			end:   "\n}\n\nrollback_journaled_directories()",
			steps: []string{
				"begin_installer_signal_transaction",
				`install -d -o "${owner}"`,
				`journaled_directory_created_identity["${path}"]="${created_identity}"`,
				"finish_installer_signal_transaction",
			},
		},
	} {
		start := strings.Index(installer, transaction.start)
		if start < 0 {
			t.Fatalf("installer transaction helper is missing %q", transaction.start)
		}
		endOffset := strings.Index(installer[start:], transaction.end)
		if endOffset < 0 {
			t.Fatalf("installer transaction helper boundary is missing %q", transaction.end)
		}
		body := installer[start : start+endOffset]
		previous := -1
		for _, step := range transaction.steps {
			index := strings.Index(body, step)
			if index <= previous {
				t.Fatalf("installer transaction helper %q has unsafe journal ordering at %q", transaction.start, step)
			}
			previous = index
		}
	}
}
