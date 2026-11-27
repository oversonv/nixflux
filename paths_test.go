package nixnet

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAndroidCommandsUseTermuxPath(t *testing.T) {
	home := t.TempDir()
	prefix := filepath.Join(t.TempDir(), "termux-prefix")
	paths := pathsForHomeForPlatform(home, "android", prefix)
	if paths.Bin != filepath.Join(prefix, "bin") {
		t.Fatalf("Android command links must be placed in Termux PATH: got %q", paths.Bin)
	}
	if paths.LegacyBin != filepath.Join(home, "nix", "bin") {
		t.Fatalf("legacy command directory changed unexpectedly: %q", paths.LegacyBin)
	}
}

func TestAndroidPathRequiresAbsoluteTermuxPrefix(t *testing.T) {
	for _, prefix := range []string{"", "relative/prefix"} {
		paths := pathsForHomeForPlatform(t.TempDir(), "android", prefix)
		if paths.Bin != "" {
			t.Fatalf("Android must not silently fall back to a non-Termux bin directory when PREFIX=%q: got %q", prefix, paths.Bin)
		}
	}
}

func TestLegacyNixBinLinksMigrateIntoTermuxPathAndRunByName(t *testing.T) {
	home := t.TempDir()
	prefix := filepath.Join(t.TempDir(), "termux-prefix")
	paths := pathsForHomeForPlatform(home, "android", prefix)
	storeDir := "test-neofetch-1.0-1"
	entrypoint := filepath.Join(paths.Store, storeDir, "payload", "bin", "neofetch")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("#!/bin/sh\necho 'NixNet command path works'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.LegacyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(entrypoint, filepath.Join(paths.LegacyBin, "neofetch")); err != nil {
		t.Fatal(err)
	}
	db := emptyDatabase()
	db.Packages["neofetch"] = InstalledPackage{
		Name: "neofetch", Version: "1.0-1", Dir: storeDir,
		Entrypoints: []string{"payload/bin/neofetch"},
	}
	db.Artifacts[storeDir] = db.Packages["neofetch"]
	db.ProfileClosures["default.toml"] = map[string]string{"neofetch": storeDir}
	settings := Settings{Channels: []Channel{}, Enabled: []string{"neofetch==1.0-1"}, ProfileName: "default.toml"}
	if err := migrateLegacyCommandLinks(paths, db, settings); err != nil {
		t.Fatalf("legacy command link migration failed: %v", err)
	}
	newLink := filepath.Join(paths.Bin, "neofetch")
	target, err := os.Readlink(newLink)
	if err != nil || target != entrypoint {
		t.Fatalf("Termux command link is incorrect: target=%q err=%v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(paths.LegacyBin, "neofetch")); !os.IsNotExist(err) {
		t.Fatalf("legacy command link was not removed: %v", err)
	}
	t.Setenv("PATH", paths.Bin)
	resolved, err := exec.LookPath("neofetch")
	if err != nil {
		t.Fatalf("command is not discoverable through PATH: %v", err)
	}
	output, err := exec.Command(resolved).CombinedOutput()
	if err != nil {
		t.Fatalf("command found in PATH did not execute: %v: %s", err, output)
	}
	if string(output) != "NixNet command path works\n" {
		t.Fatalf("unexpected command output: %q", output)
	}
}

func TestLegacyLinkMigrationDoesNotOverwriteExistingTermuxCommand(t *testing.T) {
	home := t.TempDir()
	prefix := filepath.Join(t.TempDir(), "termux-prefix")
	paths := pathsForHomeForPlatform(home, "android", prefix)
	storeDir := "test-neofetch-1.0-1"
	entrypoint := filepath.Join(paths.Store, storeDir, "payload", "bin", "neofetch")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.LegacyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(entrypoint, filepath.Join(paths.LegacyBin, "neofetch")); err != nil {
		t.Fatal(err)
	}
	conflict := filepath.Join(paths.Bin, "neofetch")
	if err := os.WriteFile(conflict, []byte("existing Termux command"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := emptyDatabase()
	db.Packages["neofetch"] = InstalledPackage{Name: "neofetch", Version: "1.0-1", Dir: storeDir, Entrypoints: []string{"payload/bin/neofetch"}}
	db.Artifacts[storeDir] = db.Packages["neofetch"]
	db.ProfileClosures["default.toml"] = map[string]string{"neofetch": storeDir}
	settings := Settings{Channels: []Channel{}, Enabled: []string{"neofetch==1.0-1"}, ProfileName: "default.toml"}
	if err := migrateLegacyCommandLinks(paths, db, settings); err == nil {
		t.Fatal("migration must reject an existing non-symlink command instead of overwriting it")
	}
	content, err := os.ReadFile(conflict)
	if err != nil || string(content) != "existing Termux command" {
		t.Fatalf("existing Termux command was modified: %q err=%v", content, err)
	}
}

func TestStartupRemovesStaleNixNetBinLinkForDisabledPackage(t *testing.T) {
	home := t.TempDir()
	prefix := filepath.Join(t.TempDir(), "termux-prefix")
	paths := pathsForHomeForPlatform(home, "android", prefix)
	storeDir := "test-neofetch-1.0-1"
	entrypoint := filepath.Join(paths.Store, storeDir, "payload", "bin", "neofetch")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("#!/bin/sh\necho 'stale NixNet command'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Bin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(paths.Bin, "neofetch")
	if err := os.Symlink(entrypoint, link); err != nil {
		t.Fatal(err)
	}

	db := emptyDatabase()
	db.Packages["neofetch"] = InstalledPackage{
		Name: "neofetch", Version: "1.0-1", Dir: storeDir,
		Entrypoints: []string{"payload/bin/neofetch"},
	}
	settings := Settings{Channels: []Channel{}, Enabled: []string{}}
	if err := migrateLegacyCommandLinks(paths, db, settings); err != nil {
		t.Fatalf("startup reconciliation failed: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("disabled NixNet-owned command link remains in Termux bin: %v", err)
	}

	t.Setenv("PATH", paths.Bin)
	if _, err := exec.LookPath("neofetch"); err == nil {
		t.Fatal("disabled NixNet command is still discoverable through PATH")
	}
}

func TestStartupPreservesNativeTermuxBinaryWhenNixNetPackageIsDisabled(t *testing.T) {
	home := t.TempDir()
	prefix := filepath.Join(t.TempDir(), "termux-prefix")
	paths := pathsForHomeForPlatform(home, "android", prefix)
	storeDir := "test-neofetch-1.0-1"
	entrypoint := filepath.Join(paths.Store, storeDir, "payload", "bin", "neofetch")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("#!/bin/sh\necho 'NixNet copy'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Bin, 0o755); err != nil {
		t.Fatal(err)
	}
	nativeCommand := filepath.Join(paths.Bin, "neofetch")
	if err := os.WriteFile(nativeCommand, []byte("#!/bin/sh\necho 'Termux package'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	db := emptyDatabase()
	db.Packages["neofetch"] = InstalledPackage{
		Name: "neofetch", Version: "1.0-1", Dir: storeDir,
		Entrypoints: []string{"payload/bin/neofetch"},
	}
	settings := Settings{Channels: []Channel{}, Enabled: []string{}}
	if err := migrateLegacyCommandLinks(paths, db, settings); err != nil {
		t.Fatalf("startup reconciliation failed: %v", err)
	}

	content, err := os.ReadFile(nativeCommand)
	if err != nil || !strings.Contains(string(content), "Termux package") {
		t.Fatalf("native Termux command was modified: content=%q err=%v", content, err)
	}
	t.Setenv("PATH", paths.Bin)
	resolved, err := exec.LookPath("neofetch")
	if err != nil {
		t.Fatalf("native Termux command disappeared from PATH: %v", err)
	}
	output, err := exec.Command(resolved).CombinedOutput()
	if err != nil || string(output) != "Termux package\n" {
		t.Fatalf("native Termux command did not remain executable: output=%q err=%v", output, err)
	}
}

func TestStartupRemovesDisabledLegacyNixBinLink(t *testing.T) {
	home := t.TempDir()
	prefix := filepath.Join(t.TempDir(), "termux-prefix")
	paths := pathsForHomeForPlatform(home, "android", prefix)
	storeDir := "test-neofetch-1.0-1"
	entrypoint := filepath.Join(paths.Store, storeDir, "payload", "bin", "neofetch")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.LegacyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyLink := filepath.Join(paths.LegacyBin, "neofetch")
	if err := os.Symlink(entrypoint, legacyLink); err != nil {
		t.Fatal(err)
	}

	db := emptyDatabase()
	db.Packages["neofetch"] = InstalledPackage{
		Name: "neofetch", Version: "1.0-1", Dir: storeDir,
		Entrypoints: []string{"payload/bin/neofetch"},
	}
	settings := Settings{Channels: []Channel{}, Enabled: []string{}}
	if err := migrateLegacyCommandLinks(paths, db, settings); err != nil {
		t.Fatalf("legacy startup cleanup failed: %v", err)
	}
	if _, err := os.Lstat(legacyLink); !os.IsNotExist(err) {
		t.Fatalf("disabled legacy command link remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(paths.Bin, "neofetch")); !os.IsNotExist(err) {
		t.Fatalf("disabled legacy command was migrated into Termux bin: %v", err)
	}
}

func TestAndroidEnvInstallPublishesCommandIntoPrefixBin(t *testing.T) {
	artifact := makeTestPackage(t, "nixnet-path-test", "1.0.0", nil, []byte("#!/bin/sh\necho 'Termux PATH works'\n"))
	server := newTestRepository(t, map[string][]byte{"nixnet-path-test": artifact})
	defer server.Close()

	home := t.TempDir()
	prefix := filepath.Join(t.TempDir(), "termux-prefix")
	paths := pathsForHomeForPlatform(home, "android", prefix)
	settings := Settings{Channels: []Channel{{Name: "main", URL: server.URL}}, Enabled: []string{}}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:nixnet-path-test==1.0.0"); err != nil {
		t.Fatalf("Android-path install failed: %v", err)
	}

	link := filepath.Join(paths.Bin, "nixnet-path-test")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("enabled command was not published in PREFIX/bin: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(paths.LegacyBin, "nixnet-path-test")); !os.IsNotExist(err) {
		t.Fatalf("command was unexpectedly published in legacy ~/nix/bin: %v", err)
	}
	loaded, err := loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Enabled) != 1 || loaded.Enabled[0] != "nixnet-path-test==1.0.0" {
		t.Fatalf("installed target was not persisted in enabled: %#v", loaded.Enabled)
	}

	t.Setenv("PATH", paths.Bin)
	resolved, err := exec.LookPath("nixnet-path-test")
	if err != nil {
		t.Fatalf("enabled command is not discoverable through PATH: %v", err)
	}
	output, err := exec.Command(resolved).CombinedOutput()
	if err != nil || string(output) != "Termux PATH works\n" {
		t.Fatalf("published command did not execute: output=%q err=%v", output, err)
	}
}

func TestLegacyMigrationReplacesStaleNixNetLinkInTermuxBin(t *testing.T) {
	home := t.TempDir()
	prefix := filepath.Join(t.TempDir(), "termux-prefix")
	paths := pathsForHomeForPlatform(home, "android", prefix)
	oldEntrypoint := filepath.Join(paths.Store, "old-neofetch", "payload", "bin", "neofetch")
	newEntrypoint := filepath.Join(paths.Store, "new-neofetch", "payload", "bin", "neofetch")
	for _, entrypoint := range []string{oldEntrypoint, newEntrypoint} {
		if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(entrypoint, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(paths.LegacyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldEntrypoint, filepath.Join(paths.LegacyBin, "neofetch")); err != nil {
		t.Fatal(err)
	}
	termuxLink := filepath.Join(paths.Bin, "neofetch")
	if err := os.Symlink(oldEntrypoint, termuxLink); err != nil {
		t.Fatal(err)
	}

	db := emptyDatabase()
	db.Packages["neofetch"] = InstalledPackage{
		Name: "neofetch", Version: "2.0-1", Dir: "new-neofetch",
		Entrypoints: []string{"payload/bin/neofetch"},
	}
	db.Artifacts["new-neofetch"] = db.Packages["neofetch"]
	db.ProfileClosures["default.toml"] = map[string]string{"neofetch": "new-neofetch"}
	settings := Settings{Channels: []Channel{}, Enabled: []string{"neofetch==2.0-1"}, ProfileName: "default.toml"}
	if err := migrateLegacyCommandLinks(paths, db, settings); err != nil {
		t.Fatalf("legacy link migration failed: %v", err)
	}
	target, err := os.Readlink(termuxLink)
	if err != nil || target != newEntrypoint {
		t.Fatalf("stale NixNet link in PREFIX/bin was not refreshed: target=%q err=%v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(paths.LegacyBin, "neofetch")); !os.IsNotExist(err) {
		t.Fatalf("legacy NixNet link was not removed: %v", err)
	}
}
