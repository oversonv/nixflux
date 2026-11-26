package nixflux

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureProfileClosureRejectsGenerationIntentDrift(t *testing.T) {
	paths := pathsForHomeForPlatform(t.TempDir(), "linux", "")
	if err := os.MkdirAll(paths.Store, 0o755); err != nil {
		t.Fatal(err)
	}
	profilePath, err := profilePath(paths, "default.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(profilePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, []byte(encodeProfileTOML([]string{"demo==1.0.0", "tool==1.0.0"})), 0o644); err != nil {
		t.Fatal(err)
	}
	demo := writeIntegrityFixturePackage(t, paths, "demo")
	tool := writeIntegrityFixturePackage(t, paths, "tool")
	settings := Settings{ProfileName: "default.toml", Enabled: []string{"demo==1.0.0", "tool==1.0.0"}}
	db := emptyDatabase()
	db.Artifacts[demo.Dir], db.Artifacts[tool.Dir] = demo, tool
	db.ProfileClosures[settings.ProfileName] = map[string]string{"demo": demo.Dir, "tool": tool.Dir}
	generation, err := newProfileGeneration(settings.ProfileName, settings, db)
	if err != nil {
		t.Fatal(err)
	}
	generationPath, err := generationPath(paths, settings.ProfileName, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	generationData, err := encodeGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(generationPath, generationData, 0o644); err != nil {
		t.Fatal(err)
	}
	settings.GenerationID = generation.ID
	db.ProfileClosures[settings.ProfileName] = map[string]string{"tool": tool.Dir}
	settings.Enabled = []string{"tool==1.0.0"}
	if err := ensureProfileClosure(paths, &db, settings); err == nil {
		t.Fatal("profile spec and closure drifted from the active immutable generation without rejection")
	} else {
		if !strings.Contains(err.Error(), "active generation") {
			t.Fatalf("drift rejected for an unrelated reason: %v", err)
		}
	}
}

func writeIntegrityFixturePackage(t *testing.T, paths AppPaths, name string) InstalledPackage {
	t.Helper()
	version := "1.0.0"
	dir := "fixture-" + name + "-" + version
	root := filepath.Join(paths.Store, dir)
	if err := os.MkdirAll(filepath.Join(root, "payload", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(fmt.Sprintf("[package]\nname = %q\nversion = %q\nplatform = \"all\"\narchitecture = \"all\"\ndependencies = []\nentrypoints = [%q]\n", name, version, "payload/bin/"+name))
	payload := []byte("#!/bin/sh\nexit 0\n")
	digest := sha256.Sum256(payload)
	checksums := []byte(hex.EncodeToString(digest[:]) + "  payload/bin/" + name + "\n")
	for path, data := range map[string][]byte{
		filepath.Join(root, "package.toml"):         manifest,
		filepath.Join(root, "checksums.sha256"):     checksums,
		filepath.Join(root, "payload", "bin", name): payload,
	} {
		mode := os.FileMode(0o644)
		if filepath.Base(path) == name {
			mode = 0o755
		}
		if err := atomicWriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	return InstalledPackage{
		Name: name, Version: version, Dir: dir, Source: "fixture",
		Dependencies: []string{}, Entrypoints: []string{"payload/bin/" + name},
		Files: []string{"package.toml", "checksums.sha256", "payload/bin/" + name},
		Modes: map[string]uint32{"package.toml": 0o644, "checksums.sha256": 0o644, "payload/bin/" + name: 0o755},
	}
}
