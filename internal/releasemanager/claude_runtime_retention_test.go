package releasemanager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Retention only inspects file identities and receipts. Small executable-shaped
// payloads keep repeated-update tests fast; no native CLI or model is invoked.
func claudeRetentionReceipt(t *testing.T, f *claudeApplyFixture, sequence int, created bool) *claudeRecovery {
	t.Helper()
	oldTarget, err := os.Readlink(f.d.Binary)
	if err != nil {
		t.Fatal(err)
	}
	oldVersion := filepath.Base(oldTarget)
	oldBytes, err := os.ReadFile(filepath.Join(f.d.VersionsDir, oldVersion))
	if err != nil {
		t.Fatal(err)
	}
	version := fmt.Sprintf("2.1.%d", 310+sequence)
	candidate := filepath.Join(f.d.VersionsDir, version)
	if err := AtomicWrite(candidate, []byte("native fixture version "+version), 0700, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(candidate)
	if err != nil {
		t.Fatal(err)
	}
	identity := info.Sys().(*syscall.Stat_t)
	if err := os.MkdirAll(claudeBackupRoot(f.l), 0700); err != nil {
		t.Fatal(err)
	}
	backup, err := os.MkdirTemp(claudeBackupRoot(f.l), "update-")
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(filepath.Join(backup, "claude"), oldBytes, 0700, nil); err != nil {
		t.Fatal(err)
	}
	distribution := *f.d
	distribution.Version, distribution.Resolved, distribution.LinkTarget = oldVersion, oldTarget, oldTarget
	config, err := os.ReadFile(f.l.Config)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := os.ReadFile(f.l.Binary)
	if err != nil {
		t.Fatal(err)
	}
	candidateBytes, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	j := &claudeRecovery{Schema: 1, Phase: "committed", CreatedAt: f.now.Add(time.Duration(sequence) * time.Minute), WorkerVersion: "1.2.3", WorkerSHA256: coreHash(worker), ConfigSHA256: coreHash(config), Distribution: distribution, OldTarget: oldTarget, OldSHA256: coreHash(oldBytes), OldMode: 0700, BackupDir: backup, CandidateVersion: version, CandidateSHA256: coreHash(candidateBytes), CandidateSelected: true, CandidateCreated: created, CandidateDevice: uint64(identity.Dev), CandidateInode: uint64(identity.Ino), Profiles: f.profiles}
	if err := validateClaudeRecovery(f.l, j); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(backup, "receipt.json"), j); err != nil {
		t.Fatal(err)
	}
	if err := atomicCodexSymlink(candidate, f.d.Binary); err != nil {
		t.Fatal(err)
	}
	return j
}

func claudeRetentionCounts(t *testing.T, f *claudeApplyFixture) (copies, receipts, natives int) {
	t.Helper()
	entries, err := os.ReadDir(claudeBackupRoot(f.l))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		path := filepath.Join(claudeBackupRoot(f.l), entry.Name())
		if FileExists(filepath.Join(path, "receipt.json")) {
			receipts++
		}
		if FileExists(filepath.Join(path, "claude")) {
			copies++
		}
	}
	versions, err := os.ReadDir(f.d.VersionsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range versions {
		if entry.Type().IsRegular() {
			natives++
		}
	}
	return copies, receipts, natives
}

func TestClaudeRetentionBoundsCopiesAndManagedNativeVersionsAcrossUpdates(t *testing.T) {
	f := newClaudeApplyFixture(t)
	var first *claudeRecovery
	for sequence := 1; sequence <= 9; sequence++ {
		j := claudeRetentionReceipt(t, f, sequence, true)
		if sequence == 1 {
			first = j
		}
		f.m.pruneClaudeBackups(f.l)
		copies, receipts, natives := claudeRetentionCounts(t, f)
		wantCopies := min(sequence, 3)
		// The original preexisting binary survives outside MACT ownership.
		wantNatives := min(sequence, 4) + 1
		if copies != wantCopies || natives != wantNatives || receipts > 4 {
			t.Fatalf("update %d: copies=%d want=%d receipts=%d natives=%d want=%d", sequence, copies, wantCopies, receipts, natives, wantNatives)
		}
		if sequence == 4 {
			if FileExists(filepath.Join(first.BackupDir, "claude")) || !FileExists(filepath.Join(first.BackupDir, "receipt.json")) || !FileExists(first.candidatePath()) {
				t.Fatal("ownership receipt was lost while its native version remained protected")
			}
		}
		if sequence == 5 && (FileExists(first.BackupDir) || FileExists(first.candidatePath())) {
			t.Fatal("metadata-only ownership receipt did not enable later native cleanup")
		}
	}
	data, err := os.ReadFile(filepath.Join(f.d.VersionsDir, "2.1.310"))
	if err != nil || !bytes.Equal(data, f.oldBytes) || f.selectedVersion() != "2.1.319" {
		t.Fatalf("retention changed the original or current version: %v", err)
	}
	// Repeated ticks are idempotent after the bounded set is reached.
	f.m.pruneClaudeBackups(f.l)
	copies, receipts, natives := claudeRetentionCounts(t, f)
	if copies != 3 || receipts != 4 || natives != 5 {
		t.Fatalf("retention is not stable: copies=%d receipts=%d natives=%d", copies, receipts, natives)
	}
}

func TestClaudeRetentionPreservesPreexistingOrExternallyChangedNativeFiles(t *testing.T) {
	for _, fault := range []string{"preexisting", "replaced-inode", "changed-hash", "world-writable", "symlink"} {
		t.Run(fault, func(t *testing.T) {
			f := newClaudeApplyFixture(t)
			j := claudeRetentionReceipt(t, f, 1, fault != "preexisting")
			path := j.candidatePath()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "replaced-inode":
				if err := AtomicWrite(path, before, 0700, nil); err != nil {
					t.Fatal(err)
				}
			case "changed-hash":
				before = []byte("externally modified in place")
				if err := os.WriteFile(path, before, 0700); err != nil {
					t.Fatal(err)
				}
			case "world-writable":
				if err := os.Chmod(path, 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				outside := filepath.Join(f.l.Home, "external-native")
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			for sequence := 2; sequence <= 8; sequence++ {
				claudeRetentionReceipt(t, f, sequence, true)
				f.m.pruneClaudeBackups(f.l)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, before) {
				t.Fatalf("retention deleted or changed an external native file: %v", err)
			}
		})
	}
}

func TestClaudeRetentionRechecksNativeIdentityAndSelectionAfterHashing(t *testing.T) {
	for _, race := range []string{"unchanged", "replaced-inode", "changed-in-place", "selected-before-hash", "selected-during-hash"} {
		t.Run(race, func(t *testing.T) {
			f := newClaudeApplyFixture(t)
			first := claudeRetentionReceipt(t, f, 1, true)
			if race != "selected-before-hash" {
				claudeRetentionReceipt(t, f, 2, true)
			}
			externalBytes := []byte("concurrently changed external native executable")
			hashes := 0
			mayDiscard := pruneClaudeNativeVersion(first, func(path string) (string, error) {
				hashes++
				oldHash, err := claudeFileHash(path)
				if err != nil {
					return oldHash, err
				}
				switch race {
				case "replaced-inode":
					if err := AtomicWrite(path, externalBytes, 0700, nil); err != nil {
						t.Fatal(err)
					}
				case "changed-in-place":
					if err := os.WriteFile(path, externalBytes, 0700); err != nil {
						t.Fatal(err)
					}
				case "selected-during-hash":
					if err := atomicCodexSymlink(path, first.Distribution.Binary); err != nil {
						t.Fatal(err)
					}
				}
				return oldHash, nil
			})
			if race == "unchanged" {
				if !mayDiscard || FileExists(first.candidatePath()) || hashes != 1 {
					t.Fatalf("unchanged owned candidate was not pruned: discard=%t hashes=%d", mayDiscard, hashes)
				}
				return
			}
			data, err := os.ReadFile(first.candidatePath())
			if err != nil {
				t.Fatalf("concurrent file or selection change lost its binary: %v", err)
			}
			if race == "replaced-inode" || race == "changed-in-place" {
				if !mayDiscard || !bytes.Equal(data, externalBytes) {
					t.Fatalf("external replacement changed or retained obsolete ownership: discard=%t", mayDiscard)
				}
			} else if mayDiscard {
				t.Fatal("newly selected version lost the ownership receipt needed for later cleanup")
			}
		})
	}
}

func TestClaudeRetentionLeavesUnknownOrUnsafeBackupEntriesUntouched(t *testing.T) {
	for _, fault := range []string{"extra-file", "directory-symlink", "receipt-symlink", "backup-symlink", "public-directory", "public-receipt"} {
		t.Run(fault, func(t *testing.T) {
			f := newClaudeApplyFixture(t)
			j := claudeRetentionReceipt(t, f, 1, true)
			copyPath := filepath.Join(j.BackupDir, "claude")
			receiptPath := filepath.Join(j.BackupDir, "receipt.json")
			copyBefore, err := os.ReadFile(copyPath)
			if err != nil {
				t.Fatal(err)
			}
			receiptBefore, err := os.ReadFile(receiptPath)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "extra-file":
				if err := os.WriteFile(filepath.Join(j.BackupDir, "user-notes"), []byte("preserve me"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory-symlink":
				moved := filepath.Join(f.l.Home, "external-backup")
				if err := os.Rename(j.BackupDir, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, j.BackupDir); err != nil {
					t.Fatal(err)
				}
			case "receipt-symlink", "backup-symlink":
				path := receiptPath
				if fault == "backup-symlink" {
					path = copyPath
				}
				moved := filepath.Join(f.l.Home, "external-backup-file")
				if err := os.Rename(path, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, path); err != nil {
					t.Fatal(err)
				}
			case "public-directory":
				if err := os.Chmod(j.BackupDir, 0755); err != nil {
					t.Fatal(err)
				}
			case "public-receipt":
				if err := os.Chmod(receiptPath, 0644); err != nil {
					t.Fatal(err)
				}
			}
			for sequence := 2; sequence <= 8; sequence++ {
				claudeRetentionReceipt(t, f, sequence, true)
				f.m.pruneClaudeBackups(f.l)
			}
			copyAfter, copyErr := os.ReadFile(copyPath)
			receiptAfter, receiptErr := os.ReadFile(receiptPath)
			if copyErr != nil || receiptErr != nil || !bytes.Equal(copyBefore, copyAfter) || !bytes.Equal(receiptBefore, receiptAfter) {
				t.Fatalf("retention modified unrecognized backup entry: copy=%v receipt=%v", copyErr, receiptErr)
			}
			var unchanged claudeRecovery
			if err := json.Unmarshal(receiptAfter, &unchanged); err != nil || unchanged.CandidateVersion != j.CandidateVersion {
				t.Fatalf("unrecognized receipt was rewritten: %v", err)
			}
		})
	}
}
