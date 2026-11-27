package nixnet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPEP440Ordering(t *testing.T) {
	pairs := [][2]string{
		{"1.9.0", "1.10.0"},
		{"2.0.0.dev1", "2.0.0a1"},
		{"2.0.0a1", "2.0.0b1"},
		{"2.0.0b1", "2.0.0rc1"},
		{"2.0.0rc1", "2.0.0"},
		{"2.0.0", "2.0.1"},
		{"2.0", "1!1.0"},
	}
	for _, pair := range pairs {
		cmp, err := comparePEP440(pair[0], pair[1])
		if err != nil || cmp >= 0 {
			t.Fatalf("expected %s < %s, cmp=%d err=%v", pair[0], pair[1], cmp, err)
		}
	}
	if cmp, _ := comparePEP440("1.0", "1.0.0"); cmp != 0 {
		t.Fatalf("release padding should compare equal: %d", cmp)
	}
	for _, invalid := range []string{"", "latest", "1..0", "1.0+K"} {
		if isValidVersion(invalid) {
			t.Errorf("expected invalid PEP 440 version %q", invalid)
		}
	}
}

func TestPackageSpecValidation(t *testing.T) {
	for _, name := range []string{"demo", "Demo", "my_package", "my.package", "a1"} {
		if !isValidPackageName(name) {
			t.Errorf("expected valid package name %q", name)
		}
	}
	for _, name := range []string{"", "-demo", "demo-", "demo/name", "démо"} {
		if isValidPackageName(name) {
			t.Errorf("expected invalid package name %q", name)
		}
	}
	if _, _, _, err := parseRepoPackageSpec("main:demo"); err == nil {
		t.Fatal("repository install must require an explicit version")
	}
	repo, name, version, err := parseRepoPackageSpec("main:Demo_Pkg==1.2.3")
	if err != nil || repo != "main" || name != "Demo_Pkg" || version != "1.2.3" {
		t.Fatalf("unexpected parsed spec: %q %q %q err=%v", repo, name, version, err)
	}
}

func TestSettingsTOMLRoundTripAndManifestDeduplication(t *testing.T) {
	input := `channels = [
  { name = "main", url = "https://example.org/repo" },
]
enabled = [
  "demo==1.10.0",
  "Demo==1.10.0",
  "tool==2.0.0",
]
`
	settings, err := parseSettingsTOML(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := normalizeSettings(&settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Channels) != 1 || settings.Channels[0].Name != "main" {
		t.Fatalf("unexpected channels: %#v", settings.Channels)
	}
	if len(settings.Enabled) != 2 || settings.Enabled[0] != "demo==1.10.0" || settings.Enabled[1] != "tool==2.0.0" {
		t.Fatalf("unexpected enabled list: %#v", settings.Enabled)
	}
	encoded := encodeSettingsTOML(settings)
	again, err := parseSettingsTOML(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Channels) != 1 || len(again.Enabled) != 2 {
		t.Fatalf("TOML round trip failed: %#v", again)
	}
}

func TestSettingsPreferNewerConflictingEnabledVersion(t *testing.T) {
	settings := Settings{
		Channels: []Channel{{Name: "main", URL: "https://example.org/repo"}},
		Enabled:  []string{"neofetch==1.0.0", "Neofetch==2.0.0"},
	}
	if err := normalizeSettings(&settings); err != nil {
		t.Fatalf("normalize conflicting versions: %v", err)
	}
	if len(settings.Enabled) != 1 || settings.Enabled[0] != "neofetch==2.0.0" {
		t.Fatalf("newer enabled version was not selected: %#v", settings.Enabled)
	}
}

func TestProfileLoadWarnsForConflictingAndDuplicateEnabledVersions(t *testing.T) {
	paths := pathsForHome(t.TempDir())
	for _, profile := range []string{"conflict", "duplicate"} {
		if err := installProfile(paths, profile); err != nil {
			t.Fatalf("install profile %s: %v", profile, err)
		}
	}

	conflictPath, err := profilePath(paths, "conflict.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflictPath, []byte(encodeProfileTOML([]string{"python==3.14", "python==3.15"})), 0o644); err != nil {
		t.Fatal(err)
	}
	duplicatePath, err := profilePath(paths, "duplicate.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(duplicatePath, []byte(encodeProfileTOML([]string{"python==3.14", "python==3.14"})), 0o644); err != nil {
		t.Fatal(err)
	}

	captureLoad := func(filename string) (Settings, string, error) {
		t.Helper()
		stderrPath := filepath.Join(t.TempDir(), "stderr.log")
		stderrFile, createErr := os.Create(stderrPath)
		if createErr != nil {
			t.Fatal(createErr)
		}
		previousStderr := os.Stderr
		os.Stderr = stderrFile
		settings, loadErr := loadProfileSettings(paths, filename)
		os.Stderr = previousStderr
		if closeErr := stderrFile.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		warning, readErr := os.ReadFile(stderrPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return settings, string(warning), loadErr
	}

	conflict, conflictWarning, err := captureLoad("conflict.toml")
	if err != nil {
		t.Fatalf("load conflicting profile: %v", err)
	}
	if len(conflict.Enabled) != 1 || conflict.Enabled[0] != "python==3.15" {
		t.Fatalf("profile did not keep the newer version: %#v", conflict.Enabled)
	}
	if !strings.Contains(conflictWarning, "warning: profile conflict.toml contains conflicting versions for python") || !strings.Contains(conflictWarning, "newer version python==3.15 takes priority") {
		t.Fatalf("missing conflict priority warning: %q", conflictWarning)
	}

	duplicate, duplicateWarning, err := captureLoad("duplicate.toml")
	if err != nil {
		t.Fatalf("load duplicate profile: %v", err)
	}
	if len(duplicate.Enabled) != 1 || duplicate.Enabled[0] != "python==3.14" {
		t.Fatalf("duplicate enabled version was not deduplicated: %#v", duplicate.Enabled)
	}
	if !strings.Contains(duplicateWarning, "warning: profile duplicate.toml repeats python==3.14; duplicate entry will be ignored") {
		t.Fatalf("missing duplicate warning: %q", duplicateWarning)
	}
}

func TestRuntimePathsStayUnderHomeNix(t *testing.T) {
	home := filepath.Join(t.TempDir(), "user-home")
	paths := pathsForHome(home)
	for _, path := range []string{paths.Store, paths.Settings, paths.Database, paths.Transaction} {
		rel, err := filepath.Rel(filepath.Join(home, "nix"), path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			t.Fatalf("runtime path escaped ~/nix: %s", path)
		}
	}
	if strings.Contains(paths.Store, "nixnet-go") {
		t.Fatalf("store must not be inside source project: %s", paths.Store)
	}
}

func TestLegacyChannelsMigrationBootstrapsProfile(t *testing.T) {
	paths := pathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Set, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"channels":[{"name":"main","url":"https://repo.example"}]}`)
	if err := os.WriteFile(paths.LegacyChannels, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	settings, err := loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Channels) != 1 || settings.Channels[0].Name != "main" {
		t.Fatalf("legacy channel was not migrated: %#v", settings)
	}
	if _, err := os.Stat(paths.Settings); !os.IsNotExist(err) {
		t.Fatalf("legacy settings file should not be recreated: %v", err)
	}
	if _, exists, err := readChannels(paths.Channels); err != nil || !exists {
		t.Fatalf("channel data was not retained: exists=%t err=%v", exists, err)
	}
	if _, err := readProfileLock(paths); err != nil {
		t.Fatalf("default profile was not bootstrapped: %v", err)
	}
}

func TestAtomicWriteFileReplacesContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.toml")
	if err := atomicWriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "second" {
		t.Fatalf("atomic replacement failed: %q, %v", data, err)
	}
}

func TestDependencyAlternativeFailureDoesNotLeakPartialPlan(t *testing.T) {
	artifacts := map[string][]byte{
		"app":     makeTestPackage(t, "app", "1.0.0", []string{"badalt | goodalt"}, []byte("#!/bin/sh\necho app\n")),
		"badalt":  makeTestPackage(t, "badalt", "1.0.0", []string{"missingdep"}, []byte("#!/bin/sh\necho bad\n")),
		"goodalt": makeTestPackage(t, "goodalt", "1.0.0", nil, []byte("#!/bin/sh\necho good\n")),
	}
	server := newTestRepository(t, artifacts)
	defer server.Close()
	paths := pathsForHome(t.TempDir())
	settings := Settings{Channels: []Channel{{Name: "main", URL: server.URL}}, Enabled: []string{}}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:app==1.0.0"); err != nil {
		t.Fatalf("dependency alternative install failed: %v", err)
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := db.Packages["goodalt"]; !exists {
		t.Fatal("second dependency alternative was not installed")
	}
	if _, exists := db.Packages["badalt"]; exists {
		t.Fatal("failed first dependency alternative leaked into committed install plan")
	}
}

func TestInstallFreezeSetUpdateAndRemove(t *testing.T) {
	artifacts := map[string][]byte{
		"1.0.0":       makeTestPackage(t, "demo", "1.0.0", nil, []byte("#!/bin/sh\necho v1\n")),
		"2.0.0":       makeTestPackage(t, "demo", "2.0.0", nil, []byte("#!/bin/sh\necho v2\n")),
		"1.0.0-child": makeTestPackage(t, "child", "1.0.0", []string{"demo>=1.0.0"}, []byte("#!/bin/sh\necho child\n")),
	}
	server := newTestRepository(t, artifacts)
	defer server.Close()
	paths := pathsForHome(t.TempDir())
	settings := Settings{Channels: []Channel{{Name: "main", URL: server.URL}}, Enabled: []string{}}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:demo==2.0.0"); err != nil {
		t.Fatal(err)
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	installed := db.Packages["demo"]
	if installed.Version != "2.0.0" || installed.Source != "main" || len(installed.Files) < 3 {
		t.Fatalf("unexpected installed record: %#v", installed)
	}
	if err := verifyStoredPackage(paths, installed); err != nil {
		t.Fatalf("installed package failed integrity verification: %v", err)
	}
	extraFile := filepath.Join(paths.Store, installed.Dir, "payload", "share", "untracked")
	if err := os.MkdirAll(filepath.Dir(extraFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extraFile, []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyStoredPackage(paths, installed); err == nil {
		t.Fatal("untracked files in the store must fail integrity verification")
	}
	if err := os.Remove(extraFile); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:demo==1.0.0"); err == nil {
		t.Fatal("env install must not perform a downgrade")
	}
	linkPath := filepath.Join(paths.Bin, "demo")
	linkTarget, err := os.Readlink(linkPath)
	if err != nil || linkTarget != filepath.Join(paths.Store, installed.Dir, "payload", "bin", "demo") {
		t.Fatalf("binary link was not created correctly: %q %v", linkTarget, err)
	}
	linkInfo, err := os.Stat(linkPath)
	if err != nil || !linkInfo.Mode().IsRegular() || linkInfo.Mode().Perm()&0o111 == 0 {
		t.Fatalf("binary link does not resolve to an executable file: %#v %v", linkInfo, err)
	}
	if err := setFreeze(paths, "demo==2.0.0", true); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:demo==1.0.0"); err == nil {
		t.Fatal("frozen package version change must be rejected")
	}
	entrypoint := filepath.Join(paths.Store, installed.Dir, "payload", "bin", "demo")
	if err := os.WriteFile(entrypoint, []byte("corrupt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:demo==2.0.0"); err != nil {
		t.Fatalf("same-version repair of a frozen package failed: %v", err)
	}
	db, err = loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if !db.Packages["demo"].Frozen {
		t.Fatal("repairing the same version must preserve freeze state")
	}
	if err := setFreeze(paths, "demo==2.0.0", false); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:child==1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := envRemove(paths, "demo==2.0.0"); err == nil {
		t.Fatal("dependency package should not be removable")
	}
	settings.Enabled = []string{"demo==1.0.0"}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if err := setFreeze(paths, "demo==2.0.0", true); err != nil {
		t.Fatal(err)
	}
	if err := setUpdate(paths); err == nil {
		t.Fatal("set update must respect freeze")
	}
	if err := setFreeze(paths, "demo==2.0.0", false); err != nil {
		t.Fatal(err)
	}
	if err := setUpdate(paths); err != nil {
		t.Fatal(err)
	}
	db, err = loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.Packages["demo"].Version; got != "1.0.0" {
		t.Fatalf("set update did not perform manifest downgrade: got %s", got)
	}
	if err := envRemove(paths, "demo==1.0.0"); err == nil {
		t.Fatal("dependency package should still be protected after downgrade")
	}
}

func TestEmbeddedMetadataVersionOutput(t *testing.T) {
	metadata, err := loadMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Name != "nixnet" || metadata.Version != "0.2.0-dev" || metadata.Data != "2026-11-27" {
		t.Fatalf("unexpected embedded metadata: %#v", metadata)
	}
	var output bytes.Buffer
	if err := renderVersion(&output); err != nil {
		t.Fatal(err)
	}
	want := "name: nixnet\nversion: 0.2.0-dev\nstatus: development\ndata: 2026-11-27\n"
	if output.String() != want {
		t.Fatalf("unexpected version output: %s", output.String())
	}
}

func makeTestPackage(t *testing.T, name, version string, dependencies []string, payload []byte) []byte {
	t.Helper()
	depends := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		alternatives, err := parseDependencyGroup(dependency)
		if err != nil {
			t.Fatal(err)
		}
		parts := make([]string, 0, len(alternatives))
		for _, alternative := range alternatives {
			item := alternative[0]
			constraint := alternative[1]
			if constraint != "" {
				op := ""
				for _, candidate := range []string{">=", "<=", "==", "!=", ">", "<"} {
					if strings.HasPrefix(constraint, candidate) {
						op = candidate
						break
					}
				}
				if op == "" {
					t.Fatalf("unsupported fixture dependency constraint %q", constraint)
				}
				constraintVersion := strings.TrimPrefix(constraint, op)
				if op == "==" {
					op = "="
				}
				item += " (" + op + " " + constraintVersion + ")"
			}
			parts = append(parts, item)
		}
		depends = append(depends, strings.Join(parts, " | "))
	}
	control := "Package: " + name + "\nVersion: " + version + "\nArchitecture: all\nDescription: NixNet integration fixture\n"
	if len(depends) > 0 {
		control += "Depends: " + strings.Join(depends, ", ") + "\n"
	}
	return makeDebFixture(t, control, []debFixtureEntry{{Name: "./usr/bin/" + name, Data: payload, Mode: 0o755}}, "")
}

func newTestRepository(t *testing.T, artifacts map[string][]byte) *httptest.Server {
	return newTestRepositoryWithRequestHook(t, artifacts, nil)
}

func newTestRepositoryWithRequestHook(t *testing.T, artifacts map[string][]byte, requestHook func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
	t.Helper()
	type artifactEntry struct {
		filename string
		content  []byte
	}
	artifactFiles := map[string]artifactEntry{}
	paragraphs := []string{}
	for key, raw := range artifacts {
		manifest, _, err := inspectPackageArchive(raw)
		if err != nil {
			t.Fatalf("test fixture is not a valid .deb: %v", err)
		}
		name := manifest.Name
		version := manifest.Version
		arch := manifest.Architecture
		if arch == "" {
			arch = "all"
		}
		filename := fmt.Sprintf("pool/main/%s/%s/%s_%s_%s.deb", name[:1], name, name, version, arch)
		controlRaw := testDebControl(t, raw)
		fields, err := parseControlFieldsStrict(string(controlRaw))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		paragraph := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nFilename: %s\nSize: %d\nSHA256: %s\n", name, version, arch, filename, len(raw), hex.EncodeToString(sum[:]))
		if fields["pre-depends"] != "" {
			paragraph += "Pre-Depends: " + strings.ReplaceAll(fields["pre-depends"], "\n", "\n ") + "\n"
		}
		if fields["depends"] != "" {
			paragraph += "Depends: " + strings.ReplaceAll(fields["depends"], "\n", "\n ") + "\n"
		}
		paragraphs = append(paragraphs, paragraph)
		artifactFiles[filename] = artifactEntry{filename: filename, content: raw}
		_ = key
	}
	packagesRaw := []byte(strings.Join(paragraphs, "\n"))
	compressed := gzipBytes(t, packagesRaw)
	sum := sha256.Sum256(compressed)
	architecture := runtimeDebianArchitecture()
	release := fmt.Sprintf("Origin: NixNet test repository\nSuite: stable\nCodename: stable\nDate: %s\nArchitectures: %s\nComponents: main\nSHA256:\n %s %d main/binary-%s/Packages.gz\n", time.Now().UTC().Format(time.RFC1123), architecture, hex.EncodeToString(sum[:]), len(compressed), architecture)
	signature := signAPTTestRelease(t, []byte(release))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestHook != nil && requestHook(w, r) {
			return
		}
		switch r.URL.Path {
		case "/dists/stable/Release":
			_, _ = w.Write([]byte(release))
		case "/dists/stable/Release.gpg":
			_, _ = w.Write(signature)
		case "/dists/stable/main/binary-" + architecture + "/Packages.gz":
			_, _ = w.Write(compressed)
		default:
			if artifact, ok := artifactFiles[strings.TrimPrefix(r.URL.Path, "/")]; ok {
				_, _ = w.Write(artifact.content)
				return
			}
			http.NotFound(w, r)
		}
	}))
	return server
}

func testDebControl(t *testing.T, raw []byte) []byte {
	t.Helper()
	members, err := readDebAr(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if strings.HasPrefix(member.name, "control.tar") {
			decoded, err := decompressDebTar(member.data, member.name, maxDebControlSize)
			if err != nil {
				t.Fatal(err)
			}
			control, err := readDebControlTar(decoded)
			if err != nil {
				t.Fatal(err)
			}
			return control
		}
	}
	t.Fatal("fixture .deb has no control archive")
	return nil
}

func TestCompatibleReleaseSpecifier(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"1.4.0", true},
		{"1.9.9", true},
		{"2.0.0", false},
	}
	for _, tc := range cases {
		got, err := versionSatisfies(tc.version, "~=1.4")
		if err != nil || got != tc.want {
			t.Errorf("versionSatisfies(%q, ~=1.4) = %v, %v; want %v", tc.version, got, err, tc.want)
		}
	}
	got, err := versionSatisfies("1.4.5", "~=1.4.5")
	if err != nil || !got {
		t.Fatalf("compatible release with patch component failed: %v %v", got, err)
	}
	got, err = versionSatisfies("1.5.0", "~=1.4.5")
	if err != nil || got {
		t.Fatalf("compatible release upper bound failed: %v %v", got, err)
	}
}

func TestTransactionRollbackBeforeInstallMovePreservesExistingDirectory(t *testing.T) {
	paths := pathsForHome(t.TempDir())
	stageRoot := filepath.Join(paths.Staging, "txn-test")
	source := filepath.Join(stageRoot, "new", "pkg")
	destination := filepath.Join(paths.Store, "pkg")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "new-file"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "old-file"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal := transactionJournal{
		State:     "prepared",
		StageRoot: stageRoot,
		Moves:     []transactionMove{{Kind: "install", Source: source, Destination: destination}},
	}
	if err := rollbackTransaction(paths, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "old-file")); err != nil {
		t.Fatalf("rollback removed untouched old package: %v", err)
	}
}

func TestLegacyDatabaseMigrationPreservesVersions(t *testing.T) {
	paths := pathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Set, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]map[string]string{
		"old-one": {"name": "demo", "version": "1.0.0", "dir": "abc-demo-1.0.0"},
		"old-two": {"name": "demo", "version": "2.0.0", "dir": "def-demo-2.0.0"},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Database, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(paths.Store, "abc-demo-1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(paths.Store, "def-demo-2.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if db.Packages["demo"].Version != "2.0.0" || len(db.LegacyPackages) != 1 {
		t.Fatalf("migration did not preserve both versions: %#v", db)
	}
	if len(db.LegacyPackages) != 1 || db.LegacyPackages["demo==1.0.0"].Version != "1.0.0" || db.Packages["demo"].Version != "2.0.0" {
		t.Fatalf("migration did not preserve active and legacy versions: active=%#v legacy=%#v", db.Packages["demo"], db.LegacyPackages)
	}
}

func TestPackageManifestRejectsDuplicateFieldsAndUnknownSections(t *testing.T) {
	invalid := []string{
		"[package]\nname = \"demo\"\nname = \"other\"\nversion = \"1.0.0\"\n",
		"[package]\nname = \"demo\"\nversion = \"1.0.0\"\n[other]\nvalue = \"x\"\n",
	}
	for _, raw := range invalid {
		if _, err := parsePackageTOML([]byte(raw)); err == nil {
			t.Errorf("expected invalid package metadata to be rejected: %s", raw)
		}
	}
}

func TestPEP440WildcardSpecifiers(t *testing.T) {
	cases := []struct {
		version string
		spec    string
		want    bool
	}{
		{"1.2.0", "==1.2.*", true},
		{"1.2.9rc1", "==1.2.*", true},
		{"1.3.0", "==1.2.*", false},
		{"1.3.0", "!=1.2.*", true},
		{"1.2.4", "!=1.2.*", false},
	}
	for _, tc := range cases {
		got, err := versionSatisfies(tc.version, tc.spec)
		if err != nil || got != tc.want {
			t.Errorf("versionSatisfies(%q, %q) = %v, %v; want %v", tc.version, tc.spec, got, err, tc.want)
		}
	}
}

func TestUnsafeStoreDirectoryNamesAreRejected(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../outside", "/tmp/store", `..\\outside`, "dir/subdir"} {
		if safeStoreDirectoryName(name) {
			t.Errorf("unsafe store directory name accepted: %q", name)
		}
	}
}

func TestPEP440SpecifierEdgeCases(t *testing.T) {
	cases := []struct {
		version string
		spec    string
		want    bool
	}{
		{"1.0+local1", "==1.0", true},
		{"1.0+local1", "==1.0+local1", true},
		{"1.0+local1", "==1.0+local2", false},
		{"1.7.0.post1", ">1.7", false},
		{"1.7.1", ">1.7", true},
		{"1.0rc1", "<1.0", false},
		{"1.0rc1", "<1.0rc2", true},
		{"1.0RC1", "===1.0rc1", true},
		{"1.4.5+build1", "~=1.4.5", true},
	}
	for _, tc := range cases {
		got, err := versionSatisfies(tc.version, tc.spec)
		if err != nil || got != tc.want {
			t.Errorf("versionSatisfies(%q, %q) = %v, %v; want %v", tc.version, tc.spec, got, err, tc.want)
		}
	}
	for _, invalid := range []string{" 1.0", "1.0 ", "1.0+local.*"} {
		if isValidVersion(invalid) {
			t.Errorf("invalid public PEP 440 version accepted: %q", invalid)
		}
	}
}

func TestRepositorySelectionUsesDebianVersionOrdering(t *testing.T) {
	packages := []string{
		"Package: demo\nVersion: 1.0~rc1-1\nArchitecture: all\nFilename: pool/demo-rc.deb\nSize: 100\nSHA256: " + strings.Repeat("a", 64),
		"Package: demo\nVersion: 1.0-1\nArchitecture: all\nFilename: pool/demo-final.deb\nSize: 100\nSHA256: " + strings.Repeat("b", 64),
	}
	server := testAPTRepository(t, runtimeDebianArchitecture(), packages)
	defer server.Close()
	settings := Settings{Channels: []Channel{{Name: "main", URL: server.URL + "/apt/termux-main", Distribution: "stable", Component: "main", Architecture: runtimeDebianArchitecture()}}}
	if _, err := loadRepositoryIndex(settings.Channels[0]); err != nil {
		t.Fatalf("failed loading test APT repository: %v", err)
	}
	_, _, version, _, err := selectRepositoryVersion(settings, "demo", ">=1.0~rc1-1", "")
	if err != nil || version != "1.0-1" {
		t.Fatalf("Debian version ordering did not choose the final release: version=%q err=%v", version, err)
	}
}

func TestLegacyChannelsMigrationDoesNotOverwriteCorruptFile(t *testing.T) {
	paths := pathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Set, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"channels":[`)
	if err := os.WriteFile(paths.LegacyChannels, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSettings(paths); err == nil {
		t.Fatal("corrupted legacy configuration should be rejected")
	}
	if _, err := os.Stat(paths.Settings); !os.IsNotExist(err) {
		t.Fatal("failed migration must not create an empty settings.toml")
	}
	persisted, err := os.ReadFile(paths.LegacyChannels)
	if err != nil || !bytes.Equal(persisted, legacy) {
		t.Fatal("failed migration changed the legacy file")
	}
}

func TestTransactionJournalRejectsPathsOutsideHomeStore(t *testing.T) {
	paths := pathsForHome(t.TempDir())
	journal := transactionJournal{
		State:     "prepared",
		StageRoot: filepath.Join(paths.Staging, "txn-test"),
		Moves: []transactionMove{{
			Kind: "remove", Source: filepath.Join(paths.Root, "important"), Destination: filepath.Join(paths.Staging, "txn-test", "old", "important"),
		}},
	}
	if err := validateTransactionJournal(paths, journal); err == nil {
		t.Fatal("transaction journal must not move paths outside the store")
	}
}

func TestTransactionRollbackRestoresRemovedPackage(t *testing.T) {
	paths := pathsForHome(t.TempDir())
	stageRoot := filepath.Join(paths.Staging, "txn-removed")
	source := filepath.Join(paths.Store, "hash-demo-1.0.0")
	backup := filepath.Join(stageRoot, "old", "hash-demo-1.0.0")
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "package.toml"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal := transactionJournal{
		State:     "prepared",
		StageRoot: stageRoot,
		Moves:     []transactionMove{{Kind: "remove", Source: source, Destination: backup}},
	}
	if err := rollbackTransaction(paths, journal); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(source, "package.toml")); err != nil || string(content) != "old" {
		t.Fatalf("removed package was not restored: %q %v", content, err)
	}
}

func TestPackagePlatformAndArchitectureAreChecked(t *testing.T) {
	manifest := fmt.Sprintf("[package]\nname = \"demo\"\nversion = \"1.0.0\"\nplatform = %q\narchitecture = \"all\"\n", "plan9")
	if _, err := parsePackageTOML([]byte(manifest)); err == nil {
		t.Fatal("package for an unsupported platform must be rejected")
	}
	manifest = fmt.Sprintf("[package]\nname = \"demo\"\nversion = \"1.0.0\"\nplatform = %q\narchitecture = %q\n", runtimePlatform(), "unsupported-arch")
	if _, err := parsePackageTOML([]byte(manifest)); err == nil {
		t.Fatal("package for an unsupported architecture must be rejected")
	}
}

func TestLoadSettingsMigratesLegacyPlaceholder(t *testing.T) {
	home := t.TempDir()
	paths := pathsForHome(home)
	if err := os.MkdirAll(paths.Set, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Settings, []byte("enabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	settings, err := loadSettings(paths)
	if err != nil {
		t.Fatalf("legacy placeholder should migrate: %v", err)
	}
	expectedDefaults := defaultSettings()
	for i := range expectedDefaults.Channels {
		expectedDefaults.Channels[i] = defaultAPTChannel(expectedDefaults.Channels[i])
	}
	if len(settings.Enabled) != 0 {
		t.Fatalf("expected no enabled packages after migration, got %#v", settings.Enabled)
	}
	if len(settings.Channels) != len(expectedDefaults.Channels) {
		t.Fatalf("expected %d platform-default channels after migration, got %#v", len(expectedDefaults.Channels), settings.Channels)
	}
	for i := range expectedDefaults.Channels {
		if settings.Channels[i] != expectedDefaults.Channels[i] {
			t.Fatalf("expected platform-default channel %#v, got %#v", expectedDefaults.Channels[i], settings.Channels[i])
		}
	}
	archived, err := os.ReadFile(paths.Settings + ".migrated")
	if err != nil || string(archived) != "enabled = true\n" {
		t.Fatalf("legacy placeholder was not preserved in its migration archive: %q err=%v", archived, err)
	}
	if _, exists, err := readChannels(paths.Channels); err != nil || !exists {
		t.Fatalf("platform default channels were not persisted: exists=%t err=%v", exists, err)
	}
	if _, err := os.Lstat(paths.Settings); !os.IsNotExist(err) {
		t.Fatalf("migrated placeholder remained at the active settings path: %v", err)
	}
}

func TestCLIValidatesInstallSpecBeforeLoadingSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	paths := pathsForHome(home)
	if err := os.MkdirAll(paths.Set, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Settings, []byte("enabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []string{"repo.name", "inavlidrepo_:hh", "inavlidrepo_:hh=5", "inavlidrepo_:hh=5.00"} {
		if code := runCLI([]string{"env", "install", spec}); code != 2 {
			t.Errorf("runCLI(%q) exit code = %d, want 2", spec, code)
		}
	}
}

func TestEnvInstallLatestPersistsResolvedConcreteVersion(t *testing.T) {
	artifact := makeTestPackage(t, "neofetch", "1.2.3", nil, []byte("#!/bin/sh\necho neofetch\n"))
	server := newTestRepository(t, map[string][]byte{"neofetch": artifact})
	defer server.Close()
	paths := pathsForHome(t.TempDir())
	settings := Settings{Channels: []Channel{{Name: "main", URL: server.URL}}, Enabled: []string{}}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:neofetch==latest"); err != nil {
		t.Fatal(err)
	}
	settings, err := loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Enabled) != 1 || settings.Enabled[0] != "neofetch==1.2.3" {
		t.Fatalf("latest was not persisted as a concrete resolved version: %#v", settings.Enabled)
	}
	var repeatedInstall packageInstallPlan
	if err := envInstallWithPlan(paths, "main:neofetch==latest", func(plan packageInstallPlan) {
		repeatedInstall = plan
	}); err != nil {
		t.Fatalf("repeating latest install failed: %v", err)
	}
	if len(repeatedInstall.Changes) != 0 || !repeatedInstall.HadPrevious || repeatedInstall.Root.Name != "neofetch" || repeatedInstall.Root.Dir != repeatedInstall.Previous.Dir || repeatedInstall.ExplicitlyAdded {
		t.Fatalf("repeat install did not produce an already-installed result: %#v", repeatedInstall)
	}
}

func TestEnvUpdateAliasIsRegistered(t *testing.T) {
	if commandArgumentCounts["env update"] != 1 || commandArgumentCounts["set update"] != 0 {
		t.Fatal("env update must require a channel and selector, with no implicit all-channel update")
	}
	args, profile, handled, err := parseArguments([]string{"env", "update", "termux:all==latest", "--profile", "work"})
	if err != nil || handled || profile != "work" || !sameStrings(args, []string{"env", "update", "termux:all==latest"}) {
		t.Fatalf("profile selector was not parsed independently from the env command: args=%#v profile=%q handled=%v err=%v", args, profile, handled, err)
	}
	aliases := map[string]string{"i": "install", "r": "remove", "u": "update", "l": "list", "f": "freeze", "uf": "unfreeze", "lf": "frozen"}
	for short, full := range aliases {
		if got := canonicalEnvAction(short); got != full {
			t.Errorf("env alias %q resolved to %q, want %q", short, got, full)
		}
	}
}

func TestInstallingNewVersionKeepsOldArtifactAndEnvUpdateAllTargetsEnabledRoot(t *testing.T) {
	artifacts := map[string][]byte{
		"python-old": makeTestPackage(t, "python", "3.14.0", nil, []byte("#!/bin/sh\necho python 3.14\n")),
		"python-new": makeTestPackage(t, "python", "3.15.2", nil, []byte("#!/bin/sh\necho python 3.15\n")),
	}
	server := newTestRepository(t, artifacts)
	defer server.Close()
	paths := pathsForHome(t.TempDir())
	settings := Settings{Channels: []Channel{{Name: "termux", URL: server.URL}}, Enabled: []string{}}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "termux:python==3.14.0"); err != nil {
		t.Fatalf("install old Python: %v", err)
	}
	oldSettings, err := loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	oldGeneration := oldSettings.GenerationID
	if err := envInstall(paths, "termux:python==3.15.2"); err != nil {
		t.Fatalf("install new Python: %v", err)
	}
	current, err := loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Enabled) != 1 || current.Enabled[0] != "python==3.15.2" {
		t.Fatalf("expected only new Python version to be enabled, got %#v", current.Enabled)
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	oldArtifactFound := false
	for _, pkg := range db.Artifacts {
		if pkg.Name == "python" && pkg.Version == "3.14.0" {
			oldArtifactFound = true
		}
	}
	if !oldArtifactFound {
		t.Fatal("installing Python 3.15.2 removed Python 3.14.0 from the store database")
	}
	if _, err := readProfileGeneration(paths, current.ProfileName, oldGeneration); err != nil {
		t.Fatalf("previous profile generation no longer references the old environment: %v", err)
	}
	var update envUpdatePlan
	if err := updateEnvironmentWithPlan(paths, "termux:all==latest", nil, func(plan envUpdatePlan) { update = plan }); err != nil {
		t.Fatalf("update all failed: %v", err)
	}
	if update.TargetCount != 1 || len(update.Changes) != 0 {
		t.Fatalf("update all should inspect only the one enabled Python root, got %#v", update)
	}
}

func TestEnvCommandsUseCurrentProfileAndCanTargetInactiveProfileExplicitly(t *testing.T) {
	artifacts := map[string][]byte{
		"alpha":    makeTestPackage(t, "alpha", "1.0.0", nil, []byte("#!/bin/sh\necho alpha\n")),
		"beta-old": makeTestPackage(t, "beta", "1.0.0", nil, []byte("#!/bin/sh\necho beta 1\n")),
		"beta-new": makeTestPackage(t, "beta", "2.0.0", nil, []byte("#!/bin/sh\necho beta 2\n")),
	}
	server := newTestRepository(t, artifacts)
	defer server.Close()
	paths := pathsForHome(t.TempDir())
	if err := saveSettings(paths.Settings, Settings{Channels: []Channel{{Name: "main", URL: server.URL}}, Enabled: []string{}}); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:beta==1.0.0"); err != nil {
		t.Fatalf("install default profile fixture: %v", err)
	}
	if err := installProfile(paths, "work"); err != nil {
		t.Fatalf("create work profile: %v", err)
	}
	if err := updateProfile(paths, "work", true); err != nil {
		t.Fatalf("load work profile: %v", err)
	}
	if err := envInstall(paths, "main:alpha==1.0.0"); err != nil {
		t.Fatalf("install into current work profile: %v", err)
	}
	current, err := loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	defaultSettings, err := loadDefaultSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if current.ProfileName != "work.toml" || len(current.Enabled) != 1 || current.Enabled[0] != "alpha==1.0.0" {
		t.Fatalf("env install did not target current profile: %#v", current)
	}
	if len(defaultSettings.Enabled) != 1 || defaultSettings.Enabled[0] != "beta==1.0.0" {
		t.Fatalf("install into current profile mutated default profile: %#v", defaultSettings.Enabled)
	}
	if err := updateEnvironmentForProfileWithPlan(paths, "main:all==latest", "default", nil, nil); err != nil {
		t.Fatalf("update inactive default profile explicitly: %v", err)
	}
	current, err = loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	defaultSettings, err = loadDefaultSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if current.ProfileName != "work.toml" || len(current.Enabled) != 1 || current.Enabled[0] != "alpha==1.0.0" {
		t.Fatalf("updating inactive profile switched or changed current profile: %#v", current)
	}
	if len(defaultSettings.Enabled) != 1 || defaultSettings.Enabled[0] != "beta==2.0.0" {
		t.Fatalf("explicit inactive-profile update did not update default root: %#v", defaultSettings.Enabled)
	}
	if err := setFreezeForProfile(paths, "beta==2.0.0", "default", true); err != nil {
		t.Fatalf("freeze package in inactive profile: %v", err)
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if db.ProfileFrozen["default.toml"]["beta"] != "2.0.0" || len(db.ProfileFrozen["work.toml"]) != 0 {
		t.Fatalf("freeze state leaked between profiles: %#v", db.ProfileFrozen)
	}
	if err := setFreezeForProfile(paths, "beta==2.0.0", "default", false); err != nil {
		t.Fatalf("unfreeze package in inactive profile: %v", err)
	}
	if err := envRemoveForProfileWithProgress(paths, "beta==2.0.0", "default", nil, nil); err != nil {
		t.Fatalf("remove package from inactive profile: %v", err)
	}
	defaultSettings, err = loadDefaultSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultSettings.Enabled) != 0 {
		t.Fatalf("remove did not clear target profile root: %#v", defaultSettings.Enabled)
	}
	current, err = loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(paths.Bin, "alpha")); err != nil {
		t.Fatalf("current profile command disappeared after inactive update: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(paths.Bin, "beta")); !os.IsNotExist(err) {
		t.Fatalf("inactive profile command was exposed in current PATH: %v", err)
	}
}

func TestProfilesUpdateRepairsConflictingCurrentProfileRoots(t *testing.T) {
	artifacts := map[string][]byte{
		"python-310": makeTestPackage(t, "python", "3.10.0", nil, []byte("#!/bin/sh\necho python 3.10\n")),
		"python-312": makeTestPackage(t, "python", "3.12.0", nil, []byte("#!/bin/sh\necho python 3.12\n")),
	}
	server := newTestRepository(t, artifacts)
	defer server.Close()
	paths := pathsForHome(t.TempDir())
	t.Setenv("HOME", filepath.Dir(paths.Root))
	if err := saveSettings(paths.Settings, Settings{Channels: []Channel{{Name: "main", URL: server.URL}}, Enabled: []string{}}); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:python==3.10.0"); err != nil {
		t.Fatalf("install initial Python: %v", err)
	}
	profile, err := loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile.ProfilePath, []byte(encodeProfileTOML([]string{"python==3.10.0", "python==3.12.0"})), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runCLI([]string{"profiles", "update"}); code != 0 {
		t.Fatalf("profiles update failed to apply normalized conflicting roots: exit %d", code)
	}
	updated, err := loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Enabled) != 1 || updated.Enabled[0] != "python==3.12.0" {
		t.Fatalf("newer conflicting root did not take priority: %#v", updated.Enabled)
	}
	profileRaw, err := os.ReadFile(updated.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(profileRaw), "3.10.0") || !strings.Contains(string(profileRaw), "python==3.12.0") {
		t.Fatalf("profile file was not canonicalized to the selected root: %s", profileRaw)
	}
}

func TestEnvInstallRemoveAndReinstallUpdatesEnabledAndCommandLinks(t *testing.T) {
	artifact := makeTestPackage(t, "neofetch", "1.0.0", nil, []byte("#!/bin/sh\nprintf 'neofetch fixture\\n'\n"))
	server := newTestRepository(t, map[string][]byte{"neofetch": artifact})
	defer server.Close()

	paths := pathsForHome(t.TempDir())
	settings := Settings{Channels: []Channel{{Name: "main", URL: server.URL}}, Enabled: []string{}}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}

	install := func() {
		t.Helper()
		if err := envInstall(paths, "main:neofetch==1.0.0"); err != nil {
			t.Fatalf("install failed: %v", err)
		}
	}
	assertEnabled := func(want bool) {
		t.Helper()
		got, err := loadSettings(paths)
		if err != nil {
			t.Fatal(err)
		}
		if want && (len(got.Enabled) != 1 || got.Enabled[0] != "neofetch==1.0.0") {
			t.Fatalf("package was not recorded in enabled: %#v", got.Enabled)
		}
		if !want && len(got.Enabled) != 0 {
			t.Fatalf("package remains in enabled after removal: %#v", got.Enabled)
		}
	}
	assertLink := func(want bool) {
		t.Helper()
		path := filepath.Join(paths.Bin, "neofetch")
		_, err := os.Lstat(path)
		if want && err != nil {
			t.Fatalf("enabled package command link is missing: %v", err)
		}
		if !want && !os.IsNotExist(err) {
			t.Fatalf("disabled package command link still exists: %v", err)
		}
	}

	install()
	assertEnabled(true)
	assertLink(true)
	db, err := loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(paths.Store, db.Packages["neofetch"].Dir, "payload", "bin", "neofetch")
	if err := os.Chmod(entrypoint, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyStoredPackage(paths, db.Packages["neofetch"]); err == nil {
		t.Fatal("store verification accepted changed executable permissions")
	}
	if err := reconcileCommandLinks(paths, db, settings); err != nil {
		t.Fatalf("reconcile with invalid entrypoint failed: %v", err)
	}
	assertLink(false)
	if err := setUpdate(paths); err != nil {
		t.Fatalf("set update did not repair invalid executable permissions: %v", err)
	}
	assertLink(true)
	db, err = loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	entrypoint = filepath.Join(paths.Store, db.Packages["neofetch"].Dir, "payload", "bin", "neofetch")
	if err := verifyStoredPackage(paths, db.Packages["neofetch"]); err != nil {
		t.Fatalf("set update did not restore valid package permissions: %v", err)
	}

	settings, err = loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	settings.Enabled = []string{"neofetch==2.0.0"}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if err := envRemove(paths, "neofetch==1.0.0"); err != nil {
		t.Fatalf("remove failed: %v", err)
	}
	assertEnabled(false)
	assertLink(false)

	// Reinstalling the exact same version must restore the target state and
	// must not depend on stale database records or stale command symlinks.
	install()
	assertEnabled(true)
	assertLink(true)

	settings, err = loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	settings.Enabled = []string{}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	db, err = loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileCommandLinks(paths, db, settings); err != nil {
		t.Fatalf("disabled command reconciliation failed: %v", err)
	}
	assertLink(false)
	t.Setenv("PATH", paths.Bin)
	if _, err := exec.LookPath("neofetch"); err == nil {
		t.Fatal("a package absent from enabled must not be discoverable through PATH")
	}
}

func TestDependencyPackagesDoNotPublishCommandsUnlessEnabled(t *testing.T) {
	artifacts := map[string][]byte{
		"child": makeTestPackage(t, "child", "1.0.0", nil, []byte("#!/bin/sh\necho child\n")),
		"app":   makeTestPackage(t, "app", "1.0.0", []string{"child"}, []byte("#!/bin/sh\necho app\n")),
	}
	server := newTestRepository(t, artifacts)
	defer server.Close()
	paths := pathsForHome(t.TempDir())
	settings := Settings{Channels: []Channel{{Name: "main", URL: server.URL}}, Enabled: []string{}}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:app==1.0.0"); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Enabled) != 1 || loaded.Enabled[0] != "app==1.0.0" {
		t.Fatalf("only the explicitly installed root package should be enabled: %#v", loaded.Enabled)
	}
	for _, name := range []string{"app", "child"} {
		_, statErr := os.Lstat(filepath.Join(paths.Bin, name))
		if name == "app" && statErr != nil {
			t.Fatalf("root package command was not published: %v", statErr)
		}
		if name == "child" && !os.IsNotExist(statErr) {
			t.Fatalf("dependency command must remain disabled: %v", statErr)
		}
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Packages["child"]; !ok {
		t.Fatal("dependency package should still be installed in the store")
	}
}

func TestSetUpdateRepairsMissingTransitiveDependencyForInstalledRoot(t *testing.T) {
	artifacts := map[string][]byte{
		"leaf":  makeTestPackage(t, "leaf", "1.0.0", nil, []byte("#!/bin/sh\necho leaf\n")),
		"child": makeTestPackage(t, "child", "1.0.0", []string{"leaf"}, []byte("#!/bin/sh\necho child\n")),
		"app":   makeTestPackage(t, "app", "1.0.0", []string{"child"}, []byte("#!/bin/sh\necho app\n")),
	}
	server := newTestRepository(t, artifacts)
	defer server.Close()
	paths := pathsForHome(t.TempDir())
	settings := Settings{Channels: []Channel{{Name: "main", URL: server.URL}}, Enabled: []string{}}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "main:app==1.0.0"); err != nil {
		t.Fatal(err)
	}
	settings, err := loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	leaf := db.Packages["leaf"]
	delete(db.Packages, "leaf")
	if err := saveDatabase(paths.Database, db); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(paths.Store, leaf.Dir)); err != nil {
		t.Fatal(err)
	}
	if err := setUpdate(paths); err != nil {
		t.Fatalf("set update failed to repair missing transitive dependency: %v", err)
	}
	db, err = loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Packages["leaf"]; !ok {
		t.Fatal("set update did not reinstall the missing transitive dependency")
	}
	if err := verifyStoredPackage(paths, db.Packages["leaf"]); err != nil {
		t.Fatalf("reinstalled transitive dependency failed store verification: %v", err)
	}
	if len(settings.Enabled) != 1 || settings.Enabled[0] != "app==1.0.0" {
		t.Fatalf("sync unexpectedly modified enabled targets: %#v", settings.Enabled)
	}
}

func TestTransactionRollbackRestoresSettings(t *testing.T) {
	paths := pathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Set, 0o755); err != nil {
		t.Fatal(err)
	}
	oldSettings := Settings{Channels: []Channel{}, Enabled: []string{"old==1.0.0"}}
	newSettings := Settings{Channels: []Channel{}, Enabled: []string{"new==2.0.0"}}
	if err := saveSettings(paths.Settings, oldSettings); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(paths.Settings)
	if err != nil {
		t.Fatal(err)
	}
	stageRoot, err := os.MkdirTemp(paths.Staging, "rollback-test-")
	if err != nil {
		t.Fatal(err)
	}
	journal := transactionJournal{
		State:           "prepared",
		StageRoot:       stageRoot,
		SettingsChanged: true,
		SettingsExisted: true,
		SettingsBackup:  backup,
		SettingsContent: []byte(encodeSettingsTOML(newSettings)),
	}
	if err := atomicWriteJSON(paths.Transaction, journal); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(paths.Settings, journal.SettingsContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := recoverTransaction(paths); err != nil {
		t.Fatalf("transaction recovery failed: %v", err)
	}
	actual, err := loadSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual.Enabled) != 1 || actual.Enabled[0] != "old==1.0.0" {
		t.Fatalf("rollback did not restore settings.toml: %#v", actual.Enabled)
	}
}

func TestSetUpdateRepairsFromPreviouslySelectedRepository(t *testing.T) {
	firstArtifact := makeTestPackage(t, "tool", "1.0.0", nil, []byte("#!/bin/sh\necho first\n"))
	secondArtifact := makeTestPackage(t, "tool", "1.0.0", nil, []byte("#!/bin/sh\necho second\n"))
	first := newTestRepository(t, map[string][]byte{"tool": firstArtifact})
	defer first.Close()
	second := newTestRepository(t, map[string][]byte{"tool": secondArtifact})
	defer second.Close()
	paths := pathsForHome(t.TempDir())
	settings := Settings{
		Channels: []Channel{{Name: "first", URL: first.URL}, {Name: "second", URL: second.URL}},
		Enabled:  []string{},
	}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if err := envInstall(paths, "second:tool==1.0.0"); err != nil {
		t.Fatal(err)
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	installed := db.Packages["tool"]
	if installed.Source != "second" {
		t.Fatalf("explicit repository source was not recorded: %q", installed.Source)
	}
	entrypoint := filepath.Join(paths.Store, installed.Dir, "payload", "bin", "tool")
	if err := os.WriteFile(entrypoint, []byte("corrupt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := setUpdate(paths); err != nil {
		t.Fatal(err)
	}
	db, err = loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	updated := db.Packages["tool"]
	content, err := os.ReadFile(filepath.Join(paths.Store, updated.Dir, "payload", "bin", "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "echo second") {
		t.Fatalf("repair did not prefer the package's original source repository: %q", content)
	}
}
