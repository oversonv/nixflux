package nixflux

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func signAPTTestRelease(t *testing.T, release []byte) []byte {
	t.Helper()
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("gpg is unavailable; signed APT fixture tests skipped")
	}
	gpgv, err := exec.LookPath("gpgv")
	if err != nil {
		t.Skip("gpgv is unavailable; signed APT fixture tests skipped")
	}
	_ = gpgv
	home := t.TempDir()
	generate := exec.Command(gpg, "--batch", "--homedir", home, "--passphrase", "", "--quick-generate-key", "NixFlux APT Test <nixflux-test@example.invalid>", "ed25519", "sign", "0")
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate ephemeral APT signing key: %v: %s", err, output)
	}
	publicKey, err := exec.Command(gpg, "--batch", "--homedir", home, "--export").Output()
	if err != nil {
		t.Fatalf("export ephemeral APT test key: %v", err)
	}
	keyring := filepath.Join(home, "test-keyring.gpg")
	if err := os.WriteFile(keyring, publicKey, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NIXFLUX_KEYRING", keyring)
	releasePath := filepath.Join(home, "Release")
	signaturePath := filepath.Join(home, "Release.gpg")
	if err := os.WriteFile(releasePath, release, 0o600); err != nil {
		t.Fatal(err)
	}
	sign := exec.Command(gpg, "--batch", "--yes", "--homedir", home, "--detach-sign", "--output", signaturePath, releasePath)
	if output, err := sign.CombinedOutput(); err != nil {
		t.Fatalf("sign APT Release fixture: %v: %s", err, output)
	}
	signature, err := os.ReadFile(signaturePath)
	if err != nil {
		t.Fatal(err)
	}
	return signature
}

func testAPTRepository(t *testing.T, architecture string, packages []string) *httptest.Server {
	t.Helper()
	packagesRaw := []byte(strings.Join(packages, "\n\n") + "\n")
	compressed := gzipBytes(t, packagesRaw)
	sum := sha256.Sum256(compressed)
	release := fmt.Sprintf("Origin: Termux\nLabel: Termux\nSuite: stable\nCodename: stable\nArchitectures: aarch64 arm i686 x86_64 amd64 arm64 i386\nComponents: main\nSHA256:\n %s %d main/binary-%s/Packages.gz\n", hex.EncodeToString(sum[:]), len(compressed), architecture)
	signature := signAPTTestRelease(t, []byte(release))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apt/termux-main/dists/stable/Release":
			_, _ = w.Write([]byte(release))
		case "/apt/termux-main/dists/stable/Release.gpg":
			_, _ = w.Write(signature)
		case "/apt/termux-main/dists/stable/main/binary-" + architecture + "/Packages.gz":
			_, _ = w.Write(compressed)
		default:
			http.NotFound(w, r)
		}
	}))
	return server
}

func TestParseAPTReleaseAndPackages(t *testing.T) {
	index := "Package: demo\nVersion: 1:2.0~rc1-3\nArchitecture: aarch64\nFilename: pool/main/d/demo/demo_2.0~rc1-3_aarch64.deb\nSize: 12345\nSHA256: " + strings.Repeat("a", 64) + "\nDepends: bash (>= 5.2), coreutils | busybox\nDescription: demo utility\n long description\n\nPackage: data-only\nVersion: 1.0-1\nArchitecture: all\nFilename: pool/main/d/data-only_1.0-1_all.deb\nSize: 2048\nSHA256: " + strings.Repeat("b", 64) + "\n"
	entries, err := parseAPT_Packages([]byte(index))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected two Packages entries, got %d", len(entries))
	}
	if entries[0].Package != "demo" || entries[0].Version != "1:2.0~rc1-3" || entries[0].Architecture != "aarch64" || entries[0].Size != 12345 {
		t.Fatalf("unexpected APT package entry: %#v", entries[0])
	}
	if got, want := entries[0].Dependencies, "bash (>= 5.2), coreutils | busybox"; got != want {
		t.Fatalf("Depends metadata = %q, want %q", got, want)
	}
	releaseRaw := fmt.Sprintf("Suite: stable\nArchitectures: aarch64 arm\nComponents: main\nSHA256:\n %s 999 main/binary-aarch64/Packages.gz\n", strings.Repeat("c", 64))
	release, err := parseAPTRelease(releaseRaw)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(release.Architectures, "aarch64") || !containsString(release.Components, "main") {
		t.Fatalf("Release metadata fields were not parsed: %#v", release)
	}
}

func TestAPTRepositoryIndexUsesReleaseAndCompressedPackages(t *testing.T) {
	packages := []string{
		"Package: neofetch\nVersion: 7.1.0-1\nArchitecture: all\nFilename: pool/main/n/neofetch/neofetch_7.1.0-1_all.deb\nSize: 500\nSHA256: " + strings.Repeat("a", 64) + "\nDepends: bash",
		"Package: native-tool\nVersion: 2:1.2-4\nArchitecture: aarch64\nFilename: pool/main/n/native-tool/native-tool_1.2-4_aarch64.deb\nSize: 800\nSHA256: " + strings.Repeat("b", 64) + "\n",
	}
	server := testAPTRepository(t, "aarch64", packages)
	defer server.Close()
	index, err := loadRepositoryIndex(Channel{Name: "termux", URL: server.URL + "/apt/termux-main", Distribution: "stable", Component: "main", Architecture: "aarch64"})
	if err != nil {
		t.Fatal(err)
	}
	if !index.Debian || len(index.Packages) != 2 {
		t.Fatalf("unexpected Debian repository index: %#v", index)
	}
	if entry, version, ok := latestRepositoryVersion(index.Packages["native-tool"]); !ok || version != "2:1.2-4" || !entry.Debian {
		t.Fatalf("latest Debian version selection failed: %#v %q %v", entry, version, ok)
	}
	if entry, version, ok := latestRepositoryVersion(index.Packages["neofetch"]); !ok || version != "7.1.0-1" || entry.Architecture != "all" {
		t.Fatalf("Architecture: all package was not indexed: %#v %q %v", entry, version, ok)
	}
}

func TestAPTRepositoryRejectsCorruptedIndexChecksum(t *testing.T) {
	packagesRaw := []byte("Package: demo\nVersion: 1.0-1\nArchitecture: aarch64\nFilename: pool/demo.deb\nSize: 100\nSHA256: " + strings.Repeat("a", 64) + "\n")
	compressed := gzipBytes(t, packagesRaw)
	actual := sha256.Sum256(compressed)
	bad := strings.Repeat("f", 64)
	if hex.EncodeToString(actual[:]) == bad {
		t.Fatal("test digest unexpectedly collides")
	}
	release := fmt.Sprintf("Architectures: aarch64\nComponents: main\nSHA256:\n %s %d main/binary-aarch64/Packages.gz\n", bad, len(compressed))
	signature := signAPTTestRelease(t, []byte(release))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dists/stable/Release" {
			_, _ = w.Write([]byte(release))
			return
		}
		if r.URL.Path == "/dists/stable/Release.gpg" {
			_, _ = w.Write(signature)
			return
		}
		if r.URL.Path == "/dists/stable/main/binary-aarch64/Packages.gz" {
			_, _ = w.Write(compressed)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	_, err := loadAPTRepositoryIndex(Channel{Name: "termux", URL: server.URL, Distribution: "stable", Component: "main", Architecture: "aarch64"})
	if !isApplicationError(err, "checksum_mismatch") {
		t.Fatalf("corrupted APT index was not rejected: %v", err)
	}
}

func TestAPTRepositoryRejectsUnsignedRelease(t *testing.T) {
	packages := []byte("Package: demo\\nVersion: 1.0-1\\nArchitecture: aarch64\\nFilename: pool/demo.deb\\nSize: 1\\nSHA256: " + strings.Repeat("a", 64) + "\\n")
	compressed := gzipBytes(t, packages)
	sum := sha256.Sum256(compressed)
	release := fmt.Sprintf("Architectures: aarch64\\nComponents: main\\nSHA256:\\n %s %d main/binary-aarch64/Packages.gz\\n", hex.EncodeToString(sum[:]), len(compressed))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dists/stable/Release" {
			_, _ = w.Write([]byte(release))
			return
		}
		if r.URL.Path == "/dists/stable/main/binary-aarch64/Packages.gz" {
			_, _ = w.Write(compressed)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	_, err := loadRepositoryIndex(Channel{Name: "unsigned", URL: server.URL, Distribution: "stable", Component: "main", Architecture: "aarch64"})
	if !isApplicationError(err, "repository_signature") {
		t.Fatalf("unsigned APT Release was not rejected: %v", err)
	}
}

func TestAPTRepositoryRejectsModifiedReleaseSignature(t *testing.T) {
	packages := []byte("Package: demo\\nVersion: 1.0-1\\nArchitecture: aarch64\\nFilename: pool/demo.deb\\nSize: 1\\nSHA256: " + strings.Repeat("a", 64) + "\\n")
	compressed := gzipBytes(t, packages)
	sum := sha256.Sum256(compressed)
	release := fmt.Sprintf("Architectures: aarch64\\nComponents: main\\nSHA256:\\n %s %d main/binary-aarch64/Packages.gz\\n", hex.EncodeToString(sum[:]), len(compressed))
	signature := signAPTTestRelease(t, []byte(release))
	modified := []byte(release + "Description: changed after signing\\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/stable/Release":
			_, _ = w.Write(modified)
		case "/dists/stable/Release.gpg":
			_, _ = w.Write(signature)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, err := loadRepositoryIndex(Channel{Name: "modified", URL: server.URL, Distribution: "stable", Component: "main", Architecture: "aarch64"})
	if !isApplicationError(err, "repository_signature") {
		t.Fatalf("modified Release metadata passed signature verification: %v", err)
	}
}

func TestAPTChannelDoesNotFallbackToLegacyIndexJSON(t *testing.T) {
	indexRequested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.json" {
			indexRequested = true
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	_, err := loadRepositoryIndex(Channel{Name: "termux", URL: server.URL + "/apt/termux-main", Distribution: "stable", Component: "main", Architecture: "aarch64"})
	if err == nil {
		t.Fatal("missing APT Release metadata must fail")
	}
	if indexRequested {
		t.Fatal("APT channel incorrectly requested the private index.json format")
	}
}

func TestTermuxChannelDefaultsAndSettingsTOMLRoundTrip(t *testing.T) {
	channel := defaultAPTChannel(Channel{Name: "termux", URL: "https://packages-cf.termux.dev/apt/termux-main"})
	if channel.Distribution != "stable" || channel.Component != "main" || channel.Architecture != runtimeDebianArchitecture() {
		t.Fatalf("wrong Termux main defaults: %#v", channel)
	}
	root := defaultAPTChannel(Channel{Name: "root", URL: "https://packages-cf.termux.dev/apt/termux-root"})
	if root.Distribution != "root" || root.Component != "stable" {
		t.Fatalf("wrong Termux root defaults: %#v", root)
	}
	x11 := defaultAPTChannel(Channel{Name: "x11", URL: "https://packages-cf.termux.dev/apt/termux-x11"})
	if x11.Distribution != "x11" || x11.Component != "main" {
		t.Fatalf("wrong Termux X11 defaults: %#v", x11)
	}
	settings := Settings{Channels: []Channel{channel}, Enabled: []string{}}
	encoded := encodeSettingsTOML(settings)
	parsed, err := parseSettingsTOML(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := normalizeSettings(&parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Channels) != 1 || parsed.Channels[0].Distribution != "stable" || parsed.Channels[0].Component != "main" {
		t.Fatalf("APT channel did not survive TOML round trip: %#v", parsed.Channels)
	}
	empty, err := parseSettingsTOML("channels = []\nenabled = []\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Channels) != 0 {
		t.Fatalf("explicit empty channels array should remain empty: %#v", empty.Channels)
	}
}

func TestSettingsParserAcceptsStandardChannelArrayTables(t *testing.T) {
	input := `[[channels]]
name = "termux"
url = "https://packages-cf.termux.dev/apt/termux-main"
distribution = "stable"
component = "main"

enabled = []
`
	settings, err := parseSettingsTOML(input)
	if err != nil {
		t.Fatalf("standard TOML [[channels]] syntax was rejected: %v", err)
	}
	if err := normalizeSettings(&settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Channels) != 1 || settings.Channels[0].Name != "termux" || settings.Channels[0].Distribution != "stable" || settings.Channels[0].Component != "main" {
		t.Fatalf("unexpected parsed channel: %#v", settings.Channels)
	}
}

func TestSettingsParserRejectsMixedChannelTableSyntax(t *testing.T) {
	input := `channels = []
[[channels]]
name = "termux"
url = "https://packages-cf.termux.dev/apt/termux-main"
`
	if _, err := parseSettingsTOML(input); err == nil {
		t.Fatal("mixed channels array and array-table syntax was accepted")
	}
}

func TestSettingsExampleParses(t *testing.T) {
	raw, err := os.ReadFile("settings.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := parseSettingsTOML(string(raw))
	if err != nil {
		t.Fatalf("settings.example.toml is not accepted by the runtime parser: %v", err)
	}
	if err := normalizeSettings(&settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Channels) != 1 || settings.Channels[0].Name != "termux" {
		t.Fatalf("unexpected example channels: %#v", settings.Channels)
	}
}

func TestSystemPackageStatusSatisfiesDebianDependencies(t *testing.T) {
	prefix := t.TempDir()
	statusPath := filepath.Join(prefix, "var", "lib", "dpkg", "status")
	if err := os.MkdirAll(filepath.Dir(statusPath), 0o755); err != nil {
		t.Fatal(err)
	}
	status := "Package: bash\nStatus: install ok installed\nVersion: 5.2.37-1\nArchitecture: " + runtimeDebianArchitecture() + "\n\nPackage: not-installed\nStatus: deinstall ok config-files\nVersion: 1.0-1\n"
	if err := os.WriteFile(statusPath, []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PREFIX", prefix)
	if version, ok := systemPackageVersion("bash"); !ok || version != "5.2.37-1" {
		t.Fatalf("installed system package was not found: %q %v", version, ok)
	}
	if _, ok := systemPackageVersion("not-installed"); ok {
		t.Fatal("package with config-files status must not count as installed")
	}
	alternatives, err := parseDependencyGroup("bash>=5.0 | busybox")
	if err != nil {
		t.Fatal(err)
	}
	if !dependencyGroupSatisfiedInDatabase(emptyDatabase(), alternatives) {
		t.Fatal("installed system bash should satisfy an alternative dependency group")
	}
}

func TestRepositoryRelativePathRejectsEscapes(t *testing.T) {
	base := "https://packages-cf.termux.dev/apt/termux-main"
	for _, file := range []string{"../evil.deb", "/etc/passwd", "https://evil.example/a.deb", "file:///tmp/a.deb", "pool\\..\\evil.deb"} {
		if _, err := resolveRepositoryFile(base, file); err == nil {
			t.Errorf("unsafe repository path accepted: %q", file)
		}
	}
	got, err := resolveRepositoryFile(base, "pool/main/n/neofetch/neofetch.deb")
	if err != nil || got != base+"/pool/main/n/neofetch/neofetch.deb" {
		t.Fatalf("valid repository path did not resolve: %q %v", got, err)
	}
}

func TestAPTInstallLatestDebEndToEndWithSystemDependency(t *testing.T) {
	architecture := runtimeDebianArchitecture()
	deb := makeDebFixture(t,
		"Package: neofetch\nVersion: 7.1.0-1\nArchitecture: all\nDepends: bash\nDescription: integration test package\n",
		[]debFixtureEntry{{Name: "./data/data/com.termux/files/usr/bin/neofetch", Data: []byte("#!/bin/sh\nprintf 'NixFlux APT integration\\n'\n"), Mode: 0o755}},
		"",
	)
	packageSum := sha256.Sum256(deb)
	filename := "pool/main/n/neofetch/neofetch_7.1.0-1_all.deb"
	packagesRaw := []byte(fmt.Sprintf(
		"Package: neofetch\nVersion: 7.1.0-1\nArchitecture: all\nFilename: %s\nSize: %d\nSHA256: %s\nDepends: bash\nDescription: integration test package\n",
		filename, len(deb), hex.EncodeToString(packageSum[:]),
	))
	compressed := gzipBytes(t, packagesRaw)
	indexSum := sha256.Sum256(compressed)
	release := fmt.Sprintf(
		"Origin: Termux integration fixture\nSuite: stable\nCodename: stable\nArchitectures: aarch64 arm i686 x86_64 amd64 arm64 i386\nComponents: main\nSHA256:\n %s %d main/binary-%s/Packages.gz\n",
		hex.EncodeToString(indexSum[:]), len(compressed), architecture,
	)
	signature := signAPTTestRelease(t, []byte(release))
	basePath := "/apt/termux-main"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case basePath + "/dists/stable/Release":
			_, _ = w.Write([]byte(release))
		case basePath + "/dists/stable/Release.gpg":
			_, _ = w.Write(signature)
		case basePath + "/dists/stable/main/binary-" + architecture + "/Packages.gz":
			_, _ = w.Write(compressed)
		case basePath + "/" + filename:
			_, _ = w.Write(deb)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	prefix := t.TempDir()
	statusPath := filepath.Join(prefix, "var", "lib", "dpkg", "status")
	if err := os.MkdirAll(filepath.Dir(statusPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath, []byte("Package: bash\nStatus: install ok installed\nVersion: 5.2.37-1\nArchitecture: "+architecture+"\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PREFIX", prefix)

	home := t.TempDir()
	t.Setenv("HOME", home)
	paths := pathsForHome(home)
	settings := Settings{Channels: []Channel{{
		Name: "termux", URL: server.URL + basePath,
		Distribution: "stable", Component: "main", Architecture: architecture,
	}}, Enabled: []string{}}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if code := runCLI([]string{"env", "install", "termux:neofetch==latest"}); code != 0 {
		t.Fatalf("CLI env install termux:neofetch==latest exited with code %d", code)
	}

	db, err := loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	installed, ok := db.Packages["neofetch"]
	if !ok || installed.Version != "7.1.0-1" || installed.Source != "termux" {
		t.Fatalf("installed package record is incorrect: %#v", installed)
	}
	if err := verifyStoredPackage(paths, installed); err != nil {
		t.Fatalf("installed DEB failed store verification: %v", err)
	}
	link := filepath.Join(paths.Bin, "neofetch")
	if _, err := os.Stat(link); err != nil {
		t.Fatalf("entrypoint link was not created: %v", err)
	}
	output, err := exec.Command(link).CombinedOutput()
	if err != nil {
		t.Fatalf("installed entrypoint did not execute: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "NixFlux APT integration") {
		t.Fatalf("unexpected entrypoint output: %q", output)
	}
}

func TestCLIProfileRollbackRestoresPreviousPackageWithoutNetwork(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is unavailable; command execution check skipped")
	}
	var offline atomic.Bool
	var offlineRequests atomic.Int64
	makeDemo := func(version, marker string) []byte {
		t.Helper()
		control := fmt.Sprintf("Package: demo\nVersion: %s\nArchitecture: all\nDescription: rollback integration fixture\n", version)
		payload := []byte(fmt.Sprintf("#!%s\nprintf '%s\\n'\n", shell, marker))
		return makeDebFixture(t, control, []debFixtureEntry{{
			Name: "./data/data/com.termux/files/usr/bin/demo",
			Data: payload,
			Mode: 0o755,
		}}, "")
	}
	server := newTestRepositoryWithRequestHook(t, map[string][]byte{
		"demo-1": makeDemo("1.0.0", "demo-v1"),
		"demo-2": makeDemo("2.0.0", "demo-v2"),
	}, func(w http.ResponseWriter, r *http.Request) bool {
		if !offline.Load() {
			return false
		}
		offlineRequests.Add(1)
		http.Error(w, "repository unavailable during rollback", http.StatusServiceUnavailable)
		return true
	})
	defer server.Close()

	home := t.TempDir()
	prefix := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PREFIX", prefix)
	paths := pathsForHome(home)
	settings := Settings{Channels: []Channel{{
		Name: "termux", URL: server.URL, Distribution: "stable", Component: "main", Architecture: runtimeDebianArchitecture(),
	}}, Enabled: []string{}}
	if err := saveSettings(paths.Settings, settings); err != nil {
		t.Fatal(err)
	}
	if code := runCLI([]string{"env", "install", "termux:demo==1.0.0"}); code != 0 {
		t.Fatalf("install version 1.0.0 exited with code %d", code)
	}

	generations, _, err := listProfileGenerations(paths, "default")
	if err != nil {
		t.Fatal(err)
	}
	var previousGeneration string
	for _, generation := range generations {
		if len(generation.Enabled) == 1 && generation.Enabled[0] == "demo==1.0.0" {
			previousGeneration = generation.ID
			break
		}
	}
	if previousGeneration == "" {
		t.Fatalf("generation for demo==1.0.0 not found: %#v", generations)
	}
	if code := runCLI([]string{"env", "install", "termux:demo==2.0.0"}); code != 0 {
		t.Fatalf("install version 2.0.0 exited with code %d", code)
	}
	if code := runCLI([]string{"env", "remove", "demo==2.0.0"}); code != 0 {
		t.Fatalf("remove version 2.0.0 exited with code %d", code)
	}

	// Rollback must use the retained Store artifact and must not contact APT.
	offline.Store(true)
	if code := runCLI([]string{"profiles", "rollback", "default", previousGeneration}); code != 0 {
		t.Fatalf("offline profile rollback exited with code %d", code)
	}
	if count := offlineRequests.Load(); count != 0 {
		t.Fatalf("rollback made %d repository request(s) while offline", count)
	}
	profile, err := loadDefaultSettings(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Enabled) != 1 || profile.Enabled[0] != "demo==1.0.0" {
		t.Fatalf("rollback did not restore version 1 declaration: %#v", profile.Enabled)
	}
	activeGeneration, err := readCurrentGeneration(paths, profile.ProfileName)
	if err != nil || activeGeneration != previousGeneration {
		t.Fatalf("rollback active generation = %q, want %q: %v", activeGeneration, previousGeneration, err)
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	installed, ok := databaseForProfile(db, profile).Packages["demo"]
	if !ok || installed.Version != "1.0.0" {
		t.Fatalf("profile closure did not restore version 1.0.0: %#v", installed)
	}
	if err := verifyStoredPackage(paths, installed); err != nil {
		t.Fatalf("restored package failed Store verification: %v", err)
	}
	link := filepath.Join(paths.Bin, "demo")
	output, err := exec.Command(link).CombinedOutput()
	if err != nil {
		t.Fatalf("restored command did not execute: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "demo-v1") {
		t.Fatalf("rollback executed wrong package payload: %q", output)
	}
}

func TestDebianVersionComparatorMatchesDpkg(t *testing.T) {
	dpkg, err := exec.LookPath("dpkg")
	if err != nil {
		t.Skip("dpkg is unavailable; Debian comparator oracle test skipped")
	}
	pairs := []struct {
		a, b string
	}{
		{"1.0~rc1-1", "1.0-1"},
		{"1:1.0-1", "2.0-1"},
		{"1.0-1", "1.0-2"},
		{"1.0", "1.0-0"},
		{"1.01", "1.1"},
		{"1.0+dfsg1-1", "1.0-1"},
		{"1.0-1+b1", "1.0-1"},
		{"1.0-1~deb12u1", "1.0-1"},
		{"2:0.1-1", "1:99.0-1"},
	}
	for _, pair := range pairs {
		got, compareErr := compareDebianVersions(pair.a, pair.b)
		if compareErr != nil {
			t.Fatalf("compareDebianVersions(%q, %q): %v", pair.a, pair.b, compareErr)
		}
		want := 0
		if err := exec.Command(dpkg, "--compare-versions", pair.a, "lt", pair.b).Run(); err == nil {
			want = -1
		} else if err := exec.Command(dpkg, "--compare-versions", pair.a, "gt", pair.b).Run(); err == nil {
			want = 1
		}
		if got != want {
			t.Errorf("Debian version order differs from dpkg: %q vs %q: got %d, want %d", pair.a, pair.b, got, want)
		}
	}
}

func TestAPTRepositoryRejectsCrossOriginRedirect(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("unexpected"))
	}))
	defer foreign.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/Release", http.StatusFound)
	}))
	defer origin.Close()
	if _, err := fetchBytesLimited(origin.URL, 1024); err == nil || !strings.Contains(err.Error(), "cross-origin") {
		t.Fatalf("cross-origin redirect was not rejected: %v", err)
	}
}

func TestAPTRepositoryIndexSupportsXZPackages(t *testing.T) {
	xz, err := exec.LookPath("xz")
	if err != nil {
		t.Skip("xz is unavailable")
	}
	packagesRaw := []byte("Package: xz-demo\nVersion: 1.0-1\nArchitecture: " + runtimeDebianArchitecture() + "\nFilename: pool/xz-demo.deb\nSize: 1\nSHA256: " + strings.Repeat("a", 64) + "\n")
	compress := exec.Command(xz, "-c")
	compress.Stdin = bytes.NewReader(packagesRaw)
	compressed, err := compress.Output()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(compressed)
	architecture := runtimeDebianArchitecture()
	release := fmt.Sprintf("Architectures: %s\nComponents: main\nSHA256:\n %s %d main/binary-%s/Packages.xz\n", architecture, hex.EncodeToString(sum[:]), len(compressed), architecture)
	signature := signAPTTestRelease(t, []byte(release))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/stable/Release":
			_, _ = w.Write([]byte(release))
		case "/dists/stable/Release.gpg":
			_, _ = w.Write(signature)
		case "/dists/stable/main/binary-" + architecture + "/Packages.xz":
			_, _ = w.Write(compressed)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	index, err := loadRepositoryIndex(Channel{Name: "xz", URL: server.URL, Distribution: "stable", Component: "main", Architecture: architecture})
	if err != nil {
		t.Fatalf("xz APT index failed: %v", err)
	}
	if _, ok := index.Packages["xz-demo"]; !ok {
		t.Fatalf("xz APT index package not found: %#v", index.Packages)
	}
}

func TestSystemPackageStatusRejectsForeignArchitecture(t *testing.T) {
	foreign := "arm64"
	if runtimeDebianArchitecture() == foreign {
		foreign = "amd64"
	}
	prefix := t.TempDir()
	statusPath := filepath.Join(prefix, "var", "lib", "dpkg", "status")
	if err := os.MkdirAll(filepath.Dir(statusPath), 0o755); err != nil {
		t.Fatal(err)
	}
	status := "Package: foreign-tool\nStatus: install ok installed\nVersion: 1.0-1\nArchitecture: " + foreign + "\n"
	if err := os.WriteFile(statusPath, []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PREFIX", prefix)
	if _, ok := systemPackageVersion("foreign-tool"); ok {
		t.Fatalf("foreign-architecture package %q incorrectly satisfied local dependency", foreign)
	}
}

func TestAPTReleaseFreshnessValidation(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	valid := aptRelease{
		Date:       now.Add(-time.Hour),
		ValidUntil: now.Add(time.Hour),
	}
	if err := validateAPTReleaseFreshness(valid, now); err != nil {
		t.Fatalf("valid Release metadata was rejected: %v", err)
	}
	expired := valid
	expired.ValidUntil = now
	if err := validateAPTReleaseFreshness(expired, now); err == nil {
		t.Fatal("expired Release metadata was accepted")
	}
	future := valid
	future.Date = now.Add(11 * time.Minute)
	if err := validateAPTReleaseFreshness(future, now); err == nil {
		t.Fatal("far-future Release Date was accepted")
	}
	withoutValidity := aptRelease{Date: now.Add(-24 * time.Hour)}
	if err := validateAPTReleaseFreshness(withoutValidity, now); err != nil {
		t.Fatalf("Release without optional Valid-Until should remain compatible: %v", err)
	}
}

func TestParseAPTReleaseDateFields(t *testing.T) {
	releaseRaw := fmt.Sprintf("Date: Tue, 25 Nov 2026 11:00:00 UTC\nValid-Until: Tue, 25 Nov 2026 13:00:00 UTC\nArchitectures: aarch64\nComponents: main\nSHA256:\n %s 999 main/binary-aarch64/Packages.gz\n", strings.Repeat("c", 64))
	release, err := parseAPTRelease(releaseRaw)
	if err != nil {
		t.Fatal(err)
	}
	if release.Date.IsZero() || release.ValidUntil.IsZero() {
		t.Fatalf("Release freshness timestamps were not retained: %#v", release)
	}
}
