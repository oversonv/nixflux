package nixflux

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTransactionJournalRejectsSymlinkedStageMoveParents(t *testing.T) {
	paths := pathsForHomeForPlatform(t.TempDir(), "linux", "")
	stageRoot := filepath.Join(paths.Staging, "txn-test")
	outside := filepath.Join(t.TempDir(), "outside")
	for _, dir := range []string{paths.Store, paths.Staging, stageRoot, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(stageRoot, "new")); err != nil {
		t.Fatal(err)
	}
	journal := transactionJournal{
		State:     "prepared",
		StageRoot: stageRoot,
		Moves: []transactionMove{{
			Kind: "install", Source: filepath.Join(stageRoot, "new", "payload"),
			Destination: filepath.Join(paths.Store, "pkg"),
		}},
	}
	if err := validateTransactionJournal(paths, journal); err == nil {
		t.Fatal("transaction accepted a move source that escapes staging through a symlink")
	}
}

func TestTransactionJournalRejectsSymlinkedProfileLauncherParents(t *testing.T) {
	paths := pathsForHomeForPlatform(t.TempDir(), "linux", "")
	profileDir := filepath.Join(paths.Profiles, "default")
	outside := filepath.Join(t.TempDir(), "outside")
	for _, dir := range []string{paths.Profiles, profileDir, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(profileDir, "launchers")); err != nil {
		t.Fatal(err)
	}
	launcher, err := profileLauncherPath(paths, "default.toml", "123abc", "demo")
	if err != nil {
		t.Fatal(err)
	}
	change := transactionFileChange{
		Path: launcher, Mode: 0o755,
		Content: []byte("#!/bin/sh\nexec '/nix/store/demo' \"$@\"\n"),
	}
	if err := validateTransactionFileChange(paths, change); err == nil {
		t.Fatal("transaction accepted a launcher path that escapes profiles through a symlink")
	}
}

func TestApplyTransactionRejectsCommandLinkChangedAfterPlanning(t *testing.T) {
	paths, journal, _, newDB, _, _, _, _, _, _, _, _, oldLink, _ := newTransactionCrashFixture(t)
	oldDatabase, err := os.ReadFile(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	changedTarget := filepath.Join(paths.Store, "unexpected", "demo")
	if err := atomicSymlink(changedTarget, filepath.Join(paths.Bin, "demo")); err != nil {
		t.Fatal(err)
	}
	if err := applyTransaction(paths, journal, newDB); err == nil {
		t.Fatal("transaction overwrote a command link changed after planning")
	}
	got, err := os.Readlink(filepath.Join(paths.Bin, "demo"))
	if err != nil || got != changedTarget {
		t.Fatalf("concurrent command link change was overwritten: target=%q err=%v", got, err)
	}
	if _, err := os.Stat(paths.Transaction); !os.IsNotExist(err) {
		t.Fatalf("transaction journal was created despite failed precondition: %v", err)
	}
	if got, err := os.ReadFile(paths.Database); err != nil || string(got) != string(oldDatabase) {
		t.Fatalf("database changed after rejected transaction: %q err=%v", got, err)
	}
	if got, err := os.Readlink(filepath.Join(paths.Bin, "legacy-demo")); err != nil || got != oldLink {
		t.Fatalf("unrelated command link changed: target=%q err=%v", got, err)
	}
}

func TestTransactionJournalRejectsDuplicateLinkNames(t *testing.T) {
	paths := pathsForHomeForPlatform(t.TempDir(), "linux", "")
	if err := os.MkdirAll(paths.Staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Store, 0o755); err != nil {
		t.Fatal(err)
	}
	stageRoot, err := os.MkdirTemp(paths.Staging, "txn-duplicate-link-")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(paths.Store, "pkg", "payload", "bin", "demo")
	journal := transactionJournal{
		State: "prepared", StageRoot: stageRoot,
		Links: []transactionLink{
			{Name: "demo", OldTarget: target, NewTarget: target},
			{Name: "demo", OldTarget: target, NewTarget: target},
		},
	}
	if err := validateTransactionJournal(paths, journal); err == nil {
		t.Fatal("transaction journal accepted duplicate command link mutations")
	}
}

func TestEnsureProfileLaunchersRejectsSymlinkedParent(t *testing.T) {
	paths := pathsForHomeForPlatform(t.TempDir(), "linux", "")
	profileDir := filepath.Join(paths.Profiles, "default")
	outside := t.TempDir()
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(profileDir, "launchers")); err != nil {
		t.Fatal(err)
	}
	pkg := InstalledPackage{
		Name: "demo", Version: "1.0.0", Dir: "hash-demo-1.0.0",
		Entrypoints: []string{"payload/bin/demo"},
	}
	db := emptyDatabase()
	db.Artifacts[pkg.Dir] = pkg
	db.ProfileClosures["default.toml"] = map[string]string{"demo": pkg.Dir}
	settings := Settings{ProfileName: "default.toml", GenerationID: "123abc", Enabled: []string{"demo==1.0.0"}}
	if err := ensureProfileLaunchers(paths, db, settings); err == nil {
		t.Fatal("launcher repair followed a profile symlink into a directory outside profiles")
	}
	if _, err := os.Lstat(filepath.Join(outside, "123abc", "demo")); !os.IsNotExist(err) {
		t.Fatalf("launcher was written outside the profile tree: %v", err)
	}
}
