package nixnet

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type debFixtureEntry struct {
	Name     string
	Data     []byte
	Linkname string
	Mode     int64
	Type     byte
}

func makeTarFixture(t *testing.T, entries []debFixtureEntry, compressed string) []byte {
	t.Helper()
	var tarBuffer bytes.Buffer
	tw := tar.NewWriter(&tarBuffer)
	for _, entry := range entries {
		typeFlag := entry.Type
		if typeFlag == 0 {
			typeFlag = tar.TypeReg
		}
		mode := entry.Mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{Name: entry.Name, Mode: mode, Size: int64(len(entry.Data)), Typeflag: typeFlag, Linkname: entry.Linkname, Format: tar.FormatPAX}
		if typeFlag == tar.TypeSymlink || typeFlag == tar.TypeDir {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if typeFlag == tar.TypeReg || typeFlag == tar.TypeRegA {
			if _, err := tw.Write(entry.Data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	plain := tarBuffer.Bytes()
	if compressed == "" {
		return append([]byte(nil), plain...)
	}
	if compressed == "gz" {
		var out bytes.Buffer
		gz := gzip.NewWriter(&out)
		if _, err := gz.Write(plain); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	if compressed == "xz" {
		tool, err := exec.LookPath("xz")
		if err != nil {
			t.Skip("xz is unavailable; xz fixture skipped")
		}
		cmd := exec.Command(tool, "-c")
		cmd.Stdin = bytes.NewReader(plain)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("xz fixture compression: %v", err)
		}
		return out
	}
	t.Fatalf("unsupported test compression %q", compressed)
	return nil
}

func makeDebFixture(t *testing.T, control string, data []debFixtureEntry, compression string) []byte {
	t.Helper()
	controlArchive := makeTarFixture(t, []debFixtureEntry{{Name: "./control", Data: []byte(control), Mode: 0o644}}, compression)
	dataArchive := makeTarFixture(t, data, compression)
	controlSuffix := ".tar"
	dataSuffix := ".tar"
	if compression == "gz" {
		controlSuffix += ".gz"
		dataSuffix += ".gz"
	} else if compression == "xz" {
		controlSuffix += ".xz"
		dataSuffix += ".xz"
	}
	members := []struct {
		name string
		data []byte
	}{
		{name: "debian-binary", data: []byte("2.0\n")},
		{name: "control" + controlSuffix, data: controlArchive},
		{name: "data" + dataSuffix, data: dataArchive},
	}
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	for _, member := range members {
		name := member.name + "/"
		if len(name) > 16 {
			t.Fatalf("test ar member name too long: %s", name)
		}
		header := fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10s%s", name, "0", "0", "0", "100644", fmt.Sprint(len(member.data)), "`\n")
		if len(header) != 60 {
			t.Fatalf("invalid ar fixture header length %d", len(header))
		}
		out.WriteString(header)
		out.Write(member.data)
		if len(member.data)%2 != 0 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func TestDebPackageConversionWithoutDpkgDeb(t *testing.T) {
	control := "Package: demo\nVersion: 2:1.0~rc1-2\nArchitecture: all\nDepends: bash (>= 5.2), coreutils | busybox, libc (<< 2.0) [!arm]\nDescription: test package\n"
	deb := makeDebFixture(t, control, []debFixtureEntry{
		{Name: "./data/data/com.termux/files/usr/bin", Type: tar.TypeDir, Mode: 0o755},
		{Name: "./data/data/com.termux/files/usr/bin/demo", Data: []byte("#!/data/data/com.termux/files/usr/bin/bash\necho demo\n"), Mode: 0o755},
		{Name: "./data/data/com.termux/files/usr/bin/demo-link", Linkname: "demo", Type: tar.TypeSymlink, Mode: 0o777},
		{Name: "./data/data/com.termux/files/usr/share/demo/data.txt", Data: []byte("payload")},
	}, "")
	t.Setenv("PATH", t.TempDir())
	archive, err := debToZip(deb)
	if err != nil {
		t.Fatalf("native .deb parser failed without dpkg-deb: %v", err)
	}
	manifest, files, err := inspectPackageArchive(archive)
	if err != nil {
		t.Fatalf("converted archive failed validation: %v", err)
	}
	if manifest.Name != "demo" || manifest.Version != "2:1.0~rc1-2" || manifest.Architecture != "all" {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	if got, want := strings.Join(manifest.Dependencies, ";"), "bash>=5.2;coreutils | busybox;libc<2.0"; got != want {
		t.Fatalf("dependency metadata = %q, want %q", got, want)
	}
	if len(manifest.Entrypoints) != 2 {
		t.Fatalf("expected two binary entrypoints, got %#v", manifest.Entrypoints)
	}
	if !containsString(files, "payload/usr/share/demo/data.txt") {
		t.Fatalf("Termux prefix share path was not normalized: %#v", files)
	}
	zipReader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	foundSymlink := false
	for _, file := range zipReader.File {
		if file.Name == "payload/bin/demo-link" {
			foundSymlink = file.Mode()&os.ModeSymlink != 0
		}
	}
	if !foundSymlink {
		t.Fatal("relative Debian symlink was not preserved in the staging archive")
	}
	destination := filepath.Join(t.TempDir(), "store")
	if _, err := extractPackageArchive(archive, destination, manifest); err != nil {
		t.Fatalf("package extraction failed: %v", err)
	}
	linkTarget, err := os.Readlink(filepath.Join(destination, "payload", "bin", "demo-link"))
	if err != nil || linkTarget != "demo" {
		t.Fatalf("symlink extraction failed: target=%q err=%v", linkTarget, err)
	}
}

func TestDebPackageConversionGzipAndXz(t *testing.T) {
	control := "Package: sample\nVersion: 1.0-1\nArchitecture: all\nDescription: test\n"
	for _, compression := range []string{"gz", "xz"} {
		t.Run(compression, func(t *testing.T) {
			deb := makeDebFixture(t, control, []debFixtureEntry{{Name: "./usr/bin/sample", Data: []byte("#!/bin/sh\nexit 0\n"), Mode: 0o755}}, compression)
			if _, err := debToZip(deb); err != nil {
				t.Fatalf("failed to parse %s-compressed Debian archives: %v", compression, err)
			}
		})
	}
}

func TestDebParserRejectsMalformedArchivesAndTraversal(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("bad"), []byte("!<arch>\nshort")} {
		if _, err := debToZip(raw); err == nil {
			t.Errorf("malformed ar archive accepted: %q", raw)
		}
	}
	control := "Package: bad\nVersion: 1.0-1\nArchitecture: all\nDescription: test\n"
	deb := makeDebFixture(t, control, []debFixtureEntry{{Name: "./../../outside", Data: []byte("escape")}}, "")
	if _, err := debToZip(deb); err == nil {
		t.Fatal("path traversal in data.tar was accepted")
	}
}

func TestDebParserRejectsDuplicateControlFields(t *testing.T) {
	control := "Package: safe\nPackage: shadow\nVersion: 1.0-1\nArchitecture: all\nDescription: test\n"
	deb := makeDebFixture(t, control, []debFixtureEntry{{Name: "./usr/bin/demo", Data: []byte("x"), Mode: 0o755}}, "")
	if _, err := debToZip(deb); err == nil {
		t.Fatal("duplicate Debian control fields were accepted")
	}
}

func TestDebParserMapsAbsoluteTermuxSymlinksToRelativeStorePaths(t *testing.T) {
	control := "Package: links\nVersion: 1.0-1\nArchitecture: all\nDescription: test\n"
	deb := makeDebFixture(t, control, []debFixtureEntry{
		{Name: "./data/data/com.termux/files/usr/bin/tool", Data: []byte("x"), Mode: 0o755},
		{Name: "./data/data/com.termux/files/usr/bin/tool-alias", Linkname: "/data/data/com.termux/files/usr/bin/tool", Type: tar.TypeSymlink},
	}, "")
	archive, err := debToZip(deb)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := inspectPackageArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entrypoints) != 2 {
		t.Fatalf("expected normalized symlink entrypoint, got %#v", manifest.Entrypoints)
	}
}

func TestDebArRejectsInvalidMemberBounds(t *testing.T) {
	var raw bytes.Buffer
	raw.WriteString("!<arch>\n")
	header := fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10s%s", "debian-binary/", "0", "0", "0", "100644", "999", "`\n")
	raw.WriteString(header)
	raw.WriteString("2.0")
	if _, err := readDebAr(raw.Bytes()); err == nil {
		t.Fatal("ar member larger than archive was accepted")
	}
}

func TestDebCompressionReaderLimit(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1024)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(data)
	_ = gz.Close()
	if _, err := decompressDebTar(buf.Bytes(), "control.tar.gz", 100); err == nil {
		t.Fatal("decompressed archive over configured limit was accepted")
	}
	if xz, err := exec.LookPath("xz"); err == nil {
		cmd := exec.Command(xz, "-c")
		cmd.Stdin = bytes.NewReader(data)
		compressed, compressErr := cmd.Output()
		if compressErr != nil {
			t.Fatal(compressErr)
		}
		if _, decompressErr := runXZDecompress(compressed, 100); decompressErr == nil {
			t.Fatal("XZ decompression exceeded its output limit without an error")
		}
	}
}

func TestDebianArchitectureRestrictionsAreApplied(t *testing.T) {
	cases := []struct {
		name     string
		arch     string
		os       string
		want     bool
		wantText string
	}{
		{name: "positive exact match", arch: "aarch64", os: "android", want: true, wantText: "bash [aarch64]"},
		{name: "positive mismatch", arch: "aarch64", os: "android", want: false, wantText: "bash [amd64]"},
		{name: "negative mismatch", arch: "aarch64", os: "android", want: false, wantText: "bash [!aarch64]"},
		{name: "negative match", arch: "aarch64", os: "android", want: true, wantText: "bash [!arm]"},
		{name: "any wildcard", arch: "aarch64", os: "android", want: true, wantText: "bash [any]"},
		{name: "linux wildcard", arch: "amd64", os: "linux", want: true, wantText: "bash [linux-any]"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, applies, err := filterDebianArchitectureRestriction(test.wantText, test.arch, test.os)
			if err != nil {
				t.Fatal(err)
			}
			if applies != test.want {
				t.Fatalf("applies=%v, want %v", applies, test.want)
			}
		})
	}
	if _, _, err := filterDebianArchitectureRestriction("bash [aarch64", "aarch64", "android"); err == nil {
		t.Fatal("unterminated architecture restriction accepted")
	}
	if _, err := parseDebDependencies("bash <stage1>"); err == nil {
		t.Fatal("unsupported build-profile restriction was silently ignored")
	}
}

func TestDebianRelativeLibrarySymlinkRemainsInsidePayload(t *testing.T) {
	control := "Package: libdemo\nVersion: 1.0-1\nArchitecture: all\nDescription: link test\n"
	deb := makeDebFixture(t, control, []debFixtureEntry{
		{Name: "./usr/bin/demo", Data: []byte("#!/bin/sh\nexit 0\n"), Mode: 0o755},
		{Name: "./usr/lib/libdemo.so.1", Data: []byte("library")},
		{Name: "./usr/lib/libdemo.so", Linkname: "libdemo.so.1", Type: tar.TypeSymlink, Mode: 0o777},
		{Name: "./usr/bin/demo-lib", Linkname: "../lib/libdemo.so", Type: tar.TypeSymlink, Mode: 0o777},
	}, "")
	archive, err := debToZip(deb)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := inspectPackageArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "store")
	if _, err := extractPackageArchive(archive, destination, manifest); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(destination, "payload", "bin", "demo-lib")
	linkTarget, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), linkTarget))
	want := filepath.Join(destination, "payload", "usr", "lib", "libdemo.so")
	if resolved != want {
		t.Fatalf("relative library symlink resolves to %q, want %q", resolved, want)
	}
}

func TestDebParserPreservesEmptyDirectories(t *testing.T) {
	control := "Package: dirs\nVersion: 1.0-1\nArchitecture: all\nDescription: directory fixture\n"
	deb := makeDebFixture(t, control, []debFixtureEntry{
		{Name: "./usr/bin/dirs", Data: []byte("#!/bin/sh\nexit 0\n"), Mode: 0o755},
		{Name: "./usr/share/dirs/empty", Type: tar.TypeDir, Mode: 0o750},
	}, "")
	archive, err := debToZip(deb)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range reader.File {
		if file.Name == "payload/usr/share/dirs/empty/" && file.FileInfo().IsDir() {
			found = true
		}
	}
	if !found {
		t.Fatal("empty directory was dropped from the converted package archive")
	}
	manifest, _, err := inspectPackageArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "store")
	if _, err := extractPackageArchive(archive, destination, manifest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "payload", "usr", "share", "dirs", "empty"))
	if err != nil || !info.IsDir() {
		t.Fatalf("empty directory was not restored into the store: info=%v err=%v", info, err)
	}
}
