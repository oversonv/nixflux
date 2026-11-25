package nixflux

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type PackageManifest struct {
	Name         string
	Version      string
	Platform     string
	Architecture string
	Dependencies []string
	Entrypoints  []string
}

type PackageArtifact struct {
	Manifest      PackageManifest
	Raw           []byte
	Hash          string
	ManifestHash  string
	ChecksumsHash string
	Files         []string
	Modes         map[string]uint32
	Source        string
}

func parsePackageTOML(raw []byte) (PackageManifest, error) {
	m := PackageManifest{Platform: "all", Architecture: "all", Dependencies: []string{}, Entrypoints: []string{}}
	lines := strings.Split(string(raw), "\n")
	section := ""
	sectionSeen := false
	fieldsSeen := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(stripTOMLComment(lines[i]))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			newSection := strings.TrimSpace(line[1 : len(line)-1])
			if newSection != "package" || sectionSeen {
				return m, fmt.Errorf("package.toml supports exactly one [package] section")
			}
			section, sectionSeen = newSection, true
			continue
		}
		if section != "package" {
			return m, fmt.Errorf("package.toml fields must be inside [package]")
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return m, fmt.Errorf("invalid package.toml assignment")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if fieldsSeen[key] {
			return m, fmt.Errorf("duplicate package.toml field %q", key)
		}
		fieldsSeen[key] = true
		for tomlValueNeedsMore(value) {
			i++
			if i >= len(lines) {
				return m, fmt.Errorf("unterminated value for package.toml field %q", key)
			}
			next := strings.TrimSpace(stripTOMLComment(lines[i]))
			if next == "" {
				continue
			}
			value += "\n" + next
		}
		switch key {
		case "name", "version", "platform", "architecture":
			v, err := parseTOMLString(value)
			if err != nil {
				return m, err
			}
			switch key {
			case "name":
				m.Name = v
			case "version":
				m.Version = v
			case "platform":
				m.Platform = v
			case "architecture":
				m.Architecture = v
			}
		case "dependencies", "entrypoints":
			items, err := splitTOMLArray(value)
			if err != nil {
				return m, err
			}
			values := []string{}
			for _, item := range items {
				v, e := parseTOMLString(item)
				if e != nil {
					return m, e
				}
				values = append(values, v)
			}
			if key == "dependencies" {
				m.Dependencies = values
			} else {
				m.Entrypoints = values
			}
		default:
			return m, fmt.Errorf("unknown package field %q", key)
		}
	}
	if !sectionSeen || !fieldsSeen["name"] || !fieldsSeen["version"] {
		return m, fmt.Errorf("package.toml requires [package], name and version")
	}
	if !isValidPackageName(m.Name) || !isValidVersion(m.Version) {
		return m, fmt.Errorf("package.toml requires a valid package name and Debian or PEP 440 version")
	}
	if m.Platform != "all" && m.Platform != runtimePlatform() {
		return m, fmt.Errorf("package platform %q does not match %q", m.Platform, runtimePlatform())
	}
	if m.Architecture != "all" && m.Architecture != runtimeArchitecture() {
		return m, fmt.Errorf("package architecture %q does not match %q", m.Architecture, runtimeArchitecture())
	}
	for _, dep := range m.Dependencies {
		if _, err := parseDependencyGroup(dep); err != nil {
			return m, fmt.Errorf("invalid dependency %q: %w", dep, err)
		}
	}
	for _, entry := range m.Entrypoints {
		if !safeArchivePath(entry) || !strings.HasPrefix(filepath.ToSlash(entry), "payload/bin/") {
			return m, fmt.Errorf("invalid entrypoint %q", entry)
		}
	}
	return m, nil
}

func runtimePlatform() string     { return runtimeGOOS }
func runtimeArchitecture() string { return runtimeGOARCH }

func inspectPackageArchive(raw []byte) (PackageManifest, []string, error) {
	if bytes.HasPrefix(raw, []byte("!<arch>\n")) {
		converted, err := debToZip(raw)
		if err != nil {
			return PackageManifest{}, nil, err
		}
		return inspectPackageArchive(converted)
	}
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return PackageManifest{}, nil, fmt.Errorf("invalid internal package staging archive: %w", err)
	}
	files := map[string]*zip.File{}
	var packageTOML, checksums []byte
	for _, f := range reader.File {
		name := filepath.ToSlash(f.Name)
		checkName := strings.TrimSuffix(name, "/")
		if !safeArchivePath(checkName) {
			return PackageManifest{}, nil, fmt.Errorf("unsafe archive path %q", name)
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if _, exists := files[name]; exists {
			return PackageManifest{}, nil, fmt.Errorf("duplicate archive path %q", name)
		}
		files[name] = f
		if name == "package.toml" {
			packageTOML, err = readZipFile(f)
			if err != nil {
				return PackageManifest{}, nil, err
			}
		}
		if name == "checksums.sha256" {
			checksums, err = readZipFile(f)
			if err != nil {
				return PackageManifest{}, nil, err
			}
		}
		if name != "package.toml" && name != "checksums.sha256" && !strings.HasPrefix(name, "payload/") {
			return PackageManifest{}, nil, fmt.Errorf("unexpected archive member %q", name)
		}
	}
	if packageTOML == nil || checksums == nil {
		return PackageManifest{}, nil, fmt.Errorf("package.toml and checksums.sha256 are required")
	}
	manifest, err := parsePackageTOML(packageTOML)
	if err != nil {
		return manifest, nil, err
	}
	expected := map[string]string{}
	for _, line := range strings.Split(string(checksums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return manifest, nil, fmt.Errorf("invalid checksums.sha256 entry")
		}
		digest := strings.TrimPrefix(parts[0], "*")
		name := strings.TrimPrefix(parts[1], "*")
		if !regexp.MustCompile(`^[a-fA-F0-9]{64}$`).MatchString(digest) || !strings.HasPrefix(name, "payload/") || !safeArchivePath(name) {
			return manifest, nil, fmt.Errorf("invalid checksum record")
		}
		if _, exists := expected[filepath.ToSlash(name)]; exists {
			return manifest, nil, fmt.Errorf("duplicate checksum record for %s", name)
		}
		expected[filepath.ToSlash(name)] = strings.ToLower(digest)
	}
	fileNames := make([]string, 0)
	for name, f := range files {
		if !strings.HasPrefix(name, "payload/") {
			continue
		}
		rel := strings.TrimPrefix(name, "payload/")
		digest, ok := expected[name]
		if !ok {
			return manifest, nil, fmt.Errorf("missing checksum for %s", name)
		}
		content, e := readZipFileLimit(f, 1<<30)
		if e != nil {
			return manifest, nil, e
		}
		if f.Mode()&os.ModeSymlink != 0 && !safeSymlinkTarget(name, string(content)) {
			return manifest, nil, fmt.Errorf("unsafe package symlink %q -> %q", name, string(content))
		}
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != digest {
			return manifest, nil, fmt.Errorf("checksum mismatch for %s", name)
		}
		fileNames = append(fileNames, "payload/"+rel)
	}
	for _, entry := range manifest.Entrypoints {
		if _, ok := files[filepath.ToSlash(entry)]; !ok {
			return manifest, nil, fmt.Errorf("entrypoint %s does not exist", entry)
		}
	}
	for name := range expected {
		full := name
		if !strings.HasPrefix(full, "payload/") {
			full = "payload/" + full
		}
		if _, ok := files[full]; !ok {
			return manifest, nil, fmt.Errorf("checksum references missing file %s", name)
		}
	}
	fileNames = append(fileNames, "package.toml", "checksums.sha256")
	sortStrings(fileNames)
	return manifest, fileNames, nil
}

func extractPackageArchive(raw []byte, destination string, manifest PackageManifest) ([]string, error) {
	if bytes.HasPrefix(raw, []byte("!<arch>\n")) {
		converted, err := debToZip(raw)
		if err != nil {
			return nil, err
		}
		raw = converted
	}
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return nil, err
	}
	files := []string{"package.toml", "checksums.sha256"}
	for _, f := range reader.File {
		name := filepath.ToSlash(f.Name)
		if (name == "package.toml" || name == "checksums.sha256") && !f.FileInfo().IsDir() {
			content, e := readZipFile(f)
			if e != nil {
				return nil, e
			}
			if e := atomicWriteFile(filepath.Join(destination, name), content, 0o644); e != nil {
				return nil, e
			}
			continue
		}
		if !strings.HasPrefix(name, "payload/") {
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if f.FileInfo().IsDir() {
			if !pathWithin(destination, target) {
				return nil, fmt.Errorf("archive directory escapes package directory")
			}
			permissions := f.Mode().Perm()
			if permissions == 0 {
				permissions = 0o755
			}
			if err := os.MkdirAll(target, permissions); err != nil {
				return nil, err
			}
			continue
		}
		if !pathWithin(destination, target) {
			return nil, fmt.Errorf("archive path escapes package directory")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		content, err := readZipFileLimit(f, 1<<30)
		if err != nil {
			return nil, err
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			linkTarget := string(content)
			if !safeSymlinkTarget(name, linkTarget) {
				return nil, fmt.Errorf("unsafe package symlink %q -> %q", name, linkTarget)
			}
			if err := os.Symlink(linkTarget, target); err != nil {
				return nil, err
			}
		} else {
			permissions := mode.Perm()
			if permissions == 0 {
				permissions = 0o644
			}
			for _, ep := range manifest.Entrypoints {
				if filepath.ToSlash(ep) == name {
					permissions |= 0o111
				}
			}
			if err := atomicWriteFile(target, content, permissions); err != nil {
				return nil, err
			}
		}
		files = append(files, filepath.ToSlash(name))
	}
	return files, nil
}

func packageFileModes(raw []byte, manifest PackageManifest) (map[string]uint32, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	entrypoints := make(map[string]bool, len(manifest.Entrypoints))
	for _, entry := range manifest.Entrypoints {
		entrypoints[filepath.ToSlash(entry)] = true
	}
	modes := make(map[string]uint32)
	for _, file := range reader.File {
		name := filepath.ToSlash(file.Name)
		if !strings.HasPrefix(name, "payload/") || file.FileInfo().IsDir() {
			continue
		}
		mode := file.Mode()
		if mode&os.ModeSymlink != 0 {
			modes[name] = uint32(os.ModeSymlink)
			continue
		}
		permissions := mode.Perm()
		if permissions == 0 {
			permissions = 0o644
		}
		if entrypoints[name] {
			permissions |= 0o111
		}
		modes[name] = uint32(permissions)
	}
	return modes, nil
}

func readZipFile(f *zip.File) ([]byte, error) {
	return readZipFileLimit(f, 64<<20)
}

func readZipFileLimit(f *zip.File, maxBytes int64) ([]byte, error) {
	r, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	content, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxBytes {
		return nil, fmt.Errorf("package file exceeds size limit")
	}
	return content, nil
}
func safeArchivePath(name string) bool {
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || name == ".." {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	return clean == name && !strings.HasPrefix(clean, "../")
}
func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
func sortStrings(values []string)   { sort.Strings(values) }
func packageHash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func packageDirectory(hash, name, version string) string {
	return hash + "-" + normalizePackageName(name) + "-" + sanitizeVersion(version)
}
func sanitizeVersion(v string) string {
	return strings.ReplaceAll(strings.ReplaceAll(v, "/", "_"), "\\", "_")
}
func parseDependency(spec string) (string, string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "", fmt.Errorf("empty dependency")
	}
	if strings.Contains(spec, "|") {
		return "", "", fmt.Errorf("dependency alternatives must be resolved as a group")
	}
	i := strings.IndexAny(spec, "<>=!~")
	if i < 0 {
		name := strings.TrimSpace(spec)
		if err := validatePackageName(name); err != nil {
			return "", "", err
		}
		return name, "", nil
	}
	name := strings.TrimSpace(spec[:i])
	constraint := strings.TrimSpace(spec[i:])
	if err := validatePackageName(name); err != nil {
		return "", "", err
	}
	if constraint == "" {
		return "", "", fmt.Errorf("missing version constraint")
	}
	if _, err := versionSatisfies("0", constraint); err != nil {
		return "", "", err
	}
	return name, constraint, nil
}

func parseDependencyGroup(spec string) ([][2]string, error) {
	alternatives := strings.Split(spec, "|")
	result := make([][2]string, 0, len(alternatives))
	for _, alternative := range alternatives {
		name, constraint, err := parseDependency(strings.TrimSpace(alternative))
		if err != nil {
			return nil, err
		}
		result = append(result, [2]string{name, constraint})
	}
	return result, nil
}

func safeSymlinkTarget(linkName, target string) bool {
	if target == "" || strings.Contains(target, "\\") || strings.HasPrefix(target, "/") {
		return false
	}
	resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(linkName)), filepath.FromSlash(target))))
	return resolved != "." && resolved != ".." && !strings.HasPrefix(resolved, "../") && safeArchivePath(resolved)
}
