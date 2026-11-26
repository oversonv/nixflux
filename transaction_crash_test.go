package nixflux

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestTransactionCrashRecoveryAtEveryMutationBoundary(t *testing.T) {
	checkpoints := []string{
		"prepared_journal", "move:0", "move:1", "database", "settings", "profile",
		"current_profile", "file:0", "link:0", "link:1", "committed_journal",
		"stage_cleanup", "transaction_cleanup",
	}
	for _, checkpoint := range checkpoints {
		t.Run(checkpoint, func(t *testing.T) {
			paths, journal, oldDB, newDB, oldSettings, newSettings, oldProfile, newProfile, oldCurrent, newCurrent, oldLock, newLock, oldLink, newLink := newTransactionCrashFixture(t)
			interrupted := fmt.Errorf("simulated process interruption at %s", checkpoint)
			err := applyTransactionObserved(paths, journal, newDB, func(name string) error {
				if name == checkpoint {
					return interrupted
				}
				return nil
			})
			if err != interrupted {
				t.Fatalf("expected injected interruption at %q, got %v", checkpoint, err)
			}

			committed := checkpoint == "committed_journal" || checkpoint == "stage_cleanup" || checkpoint == "transaction_cleanup"
			if err := recoverTransaction(paths); err != nil {
				t.Fatalf("first recovery failed: %v", err)
			}
			if err := recoverTransaction(paths); err != nil {
				t.Fatalf("second recovery failed: %v", err)
			}
			assertCrashState(t, paths, journal, oldDB, newDB, oldSettings, newSettings, oldProfile, newProfile, oldCurrent, newCurrent, oldLock, newLock, oldLink, newLink, committed)
		})
	}
}

func TestRecoveryKeepsJournalWhenRollbackSyncFails(t *testing.T) {
	paths, journal, oldDB, newDB, oldSettings, newSettings, oldProfile, newProfile, oldCurrent, newCurrent, oldLock, newLock, oldLink, newLink := newTransactionCrashFixture(t)
	interrupted := fmt.Errorf("simulated process interruption")
	err := applyTransactionObserved(paths, journal, newDB, func(name string) error {
		if name == "move:0" {
			return interrupted
		}
		return nil
	})
	if err != interrupted {
		t.Fatalf("expected interruption after first move, got %v", err)
	}

	syncFailed := false
	err = recoverTransactionWithSync(paths, func(path string) error {
		if path == paths.Store && !syncFailed {
			syncFailed = true
			return fmt.Errorf("injected directory sync failure")
		}
		return syncDirectory(path)
	})
	if err == nil || !syncFailed {
		t.Fatalf("expected rollback sync failure, got err=%v syncFailed=%t", err, syncFailed)
	}
	assertPathExists(t, paths.Transaction, true)
	if err := recoverTransaction(paths); err != nil {
		t.Fatalf("retrying recovery failed: %v", err)
	}
	assertCrashState(t, paths, journal, oldDB, newDB, oldSettings, newSettings, oldProfile, newProfile, oldCurrent, newCurrent, oldLock, newLock, oldLink, newLink, false)
}

func newTransactionCrashFixture(t *testing.T) (AppPaths, transactionJournal, Database, Database, []byte, []byte, []byte, []byte, []byte, []byte, []byte, []byte, string, string) {
	t.Helper()
	paths := pathsForHomeForPlatform(t.TempDir(), "linux", "")
	for _, dir := range []string{paths.Set, paths.Store, paths.Profiles, paths.Staging, paths.Bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	profilePathDefault, _ := profilePath(paths, "default.toml")
	profilePathNext, _ := profilePath(paths, "next.toml")
	oldProfile := []byte(encodeProfileTOML(nil))
	newProfile := []byte(encodeProfileTOML([]string{"demo==2.0.0"}))
	if err := os.WriteFile(profilePathDefault, oldProfile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePathNext, []byte(encodeProfileTOML(nil)), 0o644); err != nil {
		t.Fatal(err)
	}
	oldCurrent, newCurrent := []byte("default.toml\n"), []byte("next.toml\n")
	if err := os.WriteFile(paths.CurrentProfile, oldCurrent, 0o600); err != nil {
		t.Fatal(err)
	}
	oldLock, newLock := []byte("default.toml\n"), []byte("next.toml\n")
	if err := os.WriteFile(paths.ProfilesLock, oldLock, 0o600); err != nil {
		t.Fatal(err)
	}
	oldSettings := []byte(encodeSettingsTOML(Settings{Channels: []Channel{{Name: "main", URL: "https://example.org"}}}))
	newSettings := []byte(encodeSettingsTOML(Settings{Channels: []Channel{{Name: "main", URL: "https://example.org"}}, Enabled: []string{"demo==2.0.0"}}))
	if err := os.WriteFile(paths.Settings, oldSettings, 0o644); err != nil {
		t.Fatal(err)
	}

	oldDB, newDB := emptyDatabase(), emptyDatabase()
	newDB.ProfileFrozen["next.toml"] = map[string]string{"demo": "2.0.0"}
	if err := saveDatabase(paths.Database, oldDB); err != nil {
		t.Fatal(err)
	}
	databaseBackup, err := os.ReadFile(paths.Database)
	if err != nil {
		t.Fatal(err)
	}

	stageRoot, err := os.MkdirTemp(paths.Staging, "txn-crash-")
	if err != nil {
		t.Fatal(err)
	}
	oldPackage := filepath.Join(paths.Store, "old-package")
	newPackageSource := filepath.Join(stageRoot, "new", "new-package")
	newPackage := filepath.Join(paths.Store, "new-package")
	oldPackageBackup := filepath.Join(stageRoot, "old", "old-package")
	for _, dir := range []string{oldPackage, filepath.Join(newPackageSource, "payload", "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldPackage, "old-marker"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newPackageSource, "payload", "bin", "demo"), []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldLink, newLink := filepath.Join(oldPackage, "payload", "bin", "demo"), filepath.Join(newPackage, "payload", "bin", "demo")
	if err := atomicSymlink(oldLink, filepath.Join(paths.Bin, "demo")); err != nil {
		t.Fatal(err)
	}
	if err := atomicSymlink(oldLink, filepath.Join(paths.Bin, "legacy-demo")); err != nil {
		t.Fatal(err)
	}
	lockChange, err := captureTransactionFileChange(paths.ProfilesLock, newLock, 0o600, false)
	if err != nil {
		t.Fatal(err)
	}
	journal := transactionJournal{
		State:                 "prepared",
		StageRoot:             stageRoot,
		DatabaseExisted:       true,
		DatabaseBackup:        databaseBackup,
		SettingsChanged:       true,
		SettingsExisted:       true,
		SettingsBackup:        oldSettings,
		SettingsContent:       newSettings,
		ProfileChanged:        true,
		ProfileName:           "default.toml",
		ProfileExisted:        true,
		ProfileBackup:         oldProfile,
		ProfileContent:        newProfile,
		CurrentProfileChanged: true,
		CurrentProfileExisted: true,
		CurrentProfileBackup:  oldCurrent,
		CurrentProfileContent: newCurrent,
		Moves: []transactionMove{
			{Kind: "remove", Source: oldPackage, Destination: oldPackageBackup},
			{Kind: "install", Source: newPackageSource, Destination: newPackage},
		},
		Links: []transactionLink{
			{Name: "demo", OldTarget: oldLink, NewTarget: newLink},
			{Name: "legacy-demo", OldTarget: oldLink},
		},
		Files: []transactionFileChange{lockChange},
	}
	return paths, journal, oldDB, newDB, oldSettings, newSettings, oldProfile, newProfile, oldCurrent, newCurrent, oldLock, newLock, oldLink, newLink
}

func assertCrashState(t *testing.T, paths AppPaths, journal transactionJournal, oldDB, newDB Database, oldSettings, newSettings, oldProfile, newProfile, oldCurrent, newCurrent, oldLock, newLock []byte, oldLink, newLink string, committed bool) {
	t.Helper()
	wantDB, wantSettings, wantProfile, wantCurrent, wantLock, wantLink := oldDB, oldSettings, oldProfile, oldCurrent, oldLock, oldLink
	oldPackageExists, newPackageExists := true, false
	if committed {
		wantDB, wantSettings, wantProfile, wantCurrent, wantLock, wantLink = newDB, newSettings, newProfile, newCurrent, newLock, newLink
		oldPackageExists, newPackageExists = false, true
	}
	wantDBBytes, err := marshalPythonJSON(wantDB)
	if err != nil {
		t.Fatal(err)
	}
	assertFileBytes(t, paths.Database, wantDBBytes)
	assertFileBytes(t, paths.Settings, wantSettings)
	assertFileBytes(t, filepath.Join(paths.Profiles, "default.toml"), wantProfile)
	assertFileBytes(t, paths.CurrentProfile, wantCurrent)
	assertFileBytes(t, paths.ProfilesLock, wantLock)
	gotLink, err := os.Readlink(filepath.Join(paths.Bin, "demo"))
	if err != nil || gotLink != wantLink {
		t.Fatalf("command link = %q, %v; want %q", gotLink, err, wantLink)
	}
	legacyPath := filepath.Join(paths.Bin, "legacy-demo")
	legacyTarget, legacyErr := os.Readlink(legacyPath)
	if committed {
		if !os.IsNotExist(legacyErr) {
			t.Fatalf("legacy command link should be removed after commit: target=%q err=%v", legacyTarget, legacyErr)
		}
	} else if legacyErr != nil || legacyTarget != oldLink {
		t.Fatalf("legacy command link = %q, %v; want %q", legacyTarget, legacyErr, oldLink)
	}
	assertPathExists(t, filepath.Join(paths.Store, "old-package"), oldPackageExists)
	assertPathExists(t, filepath.Join(paths.Store, "new-package"), newPackageExists)
	assertPathExists(t, journal.StageRoot, false)
	assertPathExists(t, paths.Transaction, false)
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) {
		t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
	}
}

func assertPathExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Lstat(path)
	if want && err != nil {
		t.Fatalf("expected path %s to exist: %v", path, err)
	}
	if !want && !os.IsNotExist(err) {
		t.Fatalf("expected path %s to be absent; stat error: %v", path, err)
	}
}
