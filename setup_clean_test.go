package main

import (
	"os"
	"path/filepath"
	"testing"
)

// cleanItemFor returns the planned item whose Path == path, or nil.
func cleanItemFor(plan cleanPlan, path string) *cleanItem {
	abs, _ := filepath.Abs(path)
	for i := range plan.Items {
		if plan.Items[i].Path == abs {
			return &plan.Items[i]
		}
	}
	return nil
}

func TestIsAgentchuteBackupName(t *testing.T) {
	yes := []string{
		"agentchute.pre-hidden-poller-fix-20260618",
		"agentchute.pre-wakefix.bak",
		"agentchute.stale-dev-jun19.bak",
		"agentchute.anything.bak",
	}
	no := []string{
		"agentchute",            // the live binary
		".agentchute.tmp.12345", // install.sh staging file (leading dot)
		"agentchutewrapper",
		"ac",
		"notes.bak",
		"agentchute.json",
	}
	for _, n := range yes {
		if !isAgentchuteBackupName(n) {
			t.Errorf("expected %q to be a backup name", n)
		}
	}
	for _, n := range no {
		if isAgentchuteBackupName(n) {
			t.Errorf("expected %q NOT to be a backup name", n)
		}
	}
}

// computeCleanPlan classifies owned regular-file backups under an allowlisted
// root as remove, and everything else (outside roots, symlink, directory) as
// report — never remove.
func TestComputeCleanPlan_ClassifiesRemoveVsReport(t *testing.T) {
	allowed := t.TempDir() // both an allowlisted root AND a scan dir
	outside := t.TempDir() // a scan dir that is NOT allowlisted

	removable := filepath.Join(allowed, "agentchute.pre-good")
	mustWrite(t, removable, []byte("old binary"))
	bakRemovable := filepath.Join(allowed, "agentchute.old.bak")
	mustWrite(t, bakRemovable, []byte("old binary"))

	nonBackup := filepath.Join(allowed, "keep-me.txt")
	mustWrite(t, nonBackup, []byte("not a backup"))

	dirBackup := filepath.Join(allowed, "agentchute.pre-dir")
	mustMkdir(t, dirBackup)

	linkBackup := filepath.Join(allowed, "agentchute.pre-link")
	if err := os.Symlink(removable, linkBackup); err != nil {
		t.Fatal(err)
	}

	outsideBackup := filepath.Join(outside, "agentchute.pre-elsewhere")
	mustWrite(t, outsideBackup, []byte("old binary"))

	plan := computeCleanPlan(cleanAuditInputs{
		AllowedRoots: []string{allowed},
		ScanDirs:     []string{allowed, outside},
		CurrentUID:   os.Getuid(),
	})

	assertAction := func(path string, want cleanAction) {
		t.Helper()
		it := cleanItemFor(plan, path)
		if it == nil {
			t.Fatalf("expected a planned item for %s", path)
		}
		if it.Action != want {
			t.Fatalf("%s: action = %q, want %q (reason: %s)", filepath.Base(path), it.Action, want, it.Reason)
		}
	}
	assertAction(removable, cleanRemove)
	assertAction(bakRemovable, cleanRemove)
	assertAction(dirBackup, cleanReport)
	assertAction(linkBackup, cleanReport)
	assertAction(outsideBackup, cleanReport)

	// A non-backup-named file is never classified at all.
	if it := cleanItemFor(plan, nonBackup); it != nil {
		t.Fatalf("non-backup file must not be planned: %+v", it)
	}
}

// A backup owned by another user is report-only (we model that by passing a
// CurrentUID that does not match the test files' real owner).
func TestComputeCleanPlan_RefusesForeignOwner(t *testing.T) {
	allowed := t.TempDir()
	f := filepath.Join(allowed, "agentchute.pre-foreign")
	mustWrite(t, f, []byte("old binary"))

	plan := computeCleanPlan(cleanAuditInputs{
		AllowedRoots: []string{allowed},
		ScanDirs:     []string{allowed},
		CurrentUID:   os.Getuid() + 1, // not the file's owner
	})
	it := cleanItemFor(plan, f)
	if it == nil || it.Action != cleanReport {
		t.Fatalf("a foreign-owned backup must be report-only, got %+v", it)
	}
}

// dry-run apply mutates nothing.
func TestApplyCleanPlan_DryRunMutatesNothing(t *testing.T) {
	allowed := t.TempDir()
	f := filepath.Join(allowed, "agentchute.pre-x")
	mustWrite(t, f, []byte("old binary"))

	plan := computeCleanPlan(cleanAuditInputs{
		AllowedRoots: []string{allowed},
		ScanDirs:     []string{allowed},
		CurrentUID:   os.Getuid(),
	})
	would, err := applyCleanPlan(plan, os.Getuid(), true)
	if err != nil {
		t.Fatalf("dry-run apply: %v", err)
	}
	if len(would) != 1 {
		t.Fatalf("dry-run should report 1 would-remove, got %v", would)
	}
	mustExist(t, f) // nothing deleted in dry-run
}

// apply removes the owned backup and leaves every report-only item untouched.
func TestApplyCleanPlan_RemovesOwnedBackupOnly(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()

	removable := filepath.Join(allowed, "agentchute.pre-remove")
	mustWrite(t, removable, []byte("old binary"))
	link := filepath.Join(allowed, "agentchute.pre-link")
	if err := os.Symlink(removable, link); err != nil {
		t.Fatal(err)
	}
	outsideBackup := filepath.Join(outside, "agentchute.pre-keep")
	mustWrite(t, outsideBackup, []byte("old binary"))

	plan := computeCleanPlan(cleanAuditInputs{
		AllowedRoots: []string{allowed},
		ScanDirs:     []string{allowed, outside},
		CurrentUID:   os.Getuid(),
	})
	removed, err := applyCleanPlan(plan, os.Getuid(), false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("expected exactly 1 removal, got %v", removed)
	}
	mustNotExist(t, removable)
	mustExist(t, link)          // symlink (report-only) preserved
	mustExist(t, outsideBackup) // out-of-root backup (report-only) preserved — other repos/caches are never deleted
}

// orphan processes are report-only; bound pids are not reported.
func TestComputeCleanPlan_OrphanProcessesReportOnly(t *testing.T) {
	plan := computeCleanPlan(cleanAuditInputs{
		PoolProcesses: []cleanProcess{
			{PID: 111, Kind: "runner"}, // unbound -> orphan report
			{PID: 222, Kind: "poller"}, // bound -> not reported
		},
		BoundPIDs: map[int]bool{222: true},
	})
	var orphans int
	for _, it := range plan.Items {
		if it.Class != cleanClassOrphanProcess {
			continue
		}
		orphans++
		if it.Action != cleanReport {
			t.Fatalf("orphan process must be report-only, got %q", it.Action)
		}
		if it.PID != 111 {
			t.Fatalf("only the unbound pid 111 should be an orphan, got pid=%d", it.PID)
		}
	}
	if orphans != 1 {
		t.Fatalf("expected exactly 1 orphan-process item, got %d", orphans)
	}
}

func TestCleanPathShadowItem(t *testing.T) {
	if it := cleanPathShadowItem([]string{"/opt/old/agentchute"}, "/home/me/.local/bin/agentchute"); it == nil {
		t.Fatal("expected a shadow report when PATH resolves a different binary first")
	} else if it.Action != cleanReport || it.Class != cleanClassPathShadow {
		t.Fatalf("shadow item misclassified: %+v", it)
	}
	if it := cleanPathShadowItem([]string{"/home/me/.local/bin/agentchute"}, "/home/me/.local/bin/agentchute"); it != nil {
		t.Fatalf("no shadow when the install resolves first: %+v", it)
	}
	if it := cleanPathShadowItem(nil, "/home/me/.local/bin/agentchute"); it != nil {
		t.Fatalf("no shadow when PATH resolves nothing: %+v", it)
	}
}
