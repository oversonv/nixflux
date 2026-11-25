package nixflux

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const databaseSchemaVersion = 3

type InstalledPackage struct {
	Name          string            `json:"name"`
	Version       string            `json:"version"`
	Hash          string            `json:"hash"`
	ManifestHash  string            `json:"manifest_hash"`
	ChecksumsHash string            `json:"checksums_hash"`
	Dir           string            `json:"dir"`
	Source        string            `json:"source"`
	Dependencies  []string          `json:"dependencies"`
	Files         []string          `json:"files"`
	Entrypoints   []string          `json:"entrypoints"`
	Modes         map[string]uint32 `json:"modes,omitempty"`
	Frozen        bool              `json:"frozen"`
}

type Database struct {
	SchemaVersion  int                         `json:"schema_version"`
	Packages       map[string]InstalledPackage `json:"packages"`
	LegacyPackages map[string]InstalledPackage `json:"legacy_packages,omitempty"`
}

func emptyDatabase() Database {
	return Database{SchemaVersion: databaseSchemaVersion, Packages: map[string]InstalledPackage{}, LegacyPackages: map[string]InstalledPackage{}}
}

func loadDatabase(path string) (Database, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return emptyDatabase(), nil
	}
	if err != nil {
		return Database{}, appError("io_error", err.Error())
	}
	var db Database
	if err := json.Unmarshal(raw, &db); err == nil && db.Packages != nil {
		if db.SchemaVersion > databaseSchemaVersion {
			return Database{}, appError("invalid_database", fmt.Sprintf("database schema %d is newer than supported schema %d", db.SchemaVersion, databaseSchemaVersion))
		}
		if db.SchemaVersion == 0 {
			db.SchemaVersion = databaseSchemaVersion
		}
		if db.LegacyPackages == nil {
			db.LegacyPackages = map[string]InstalledPackage{}
		}
		storePath := filepath.Join(filepath.Dir(filepath.Dir(path)), "store")
		if err := validateDatabase(db, storePath); err != nil {
			return Database{}, appError("invalid_database", err.Error())
		}
		return db, nil
	}
	var legacy map[string]struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Dir     string `json:"dir"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return Database{}, appError("invalid_json", err.Error())
	}
	db = emptyDatabase()
	legacyRecords := make([]InstalledPackage, 0, len(legacy))
	for _, old := range legacy {
		if old.Name == "" || !isValidVersion(old.Version) || !safeStoreDirectoryName(old.Dir) || !isValidPackageName(old.Name) {
			return Database{}, appError("invalid_database", "legacy package record is incomplete or invalid")
		}
		hash := strings.SplitN(old.Dir, "-", 2)[0]
		legacyRecords = append(legacyRecords, InstalledPackage{Name: old.Name, Version: old.Version, Hash: hash, Dir: old.Dir, Source: "legacy", Dependencies: []string{}, Files: []string{}, Entrypoints: []string{}})
	}
	sort.Slice(legacyRecords, func(i, j int) bool {
		return normalizePackageName(legacyRecords[i].Name)+"=="+legacyRecords[i].Version < normalizePackageName(legacyRecords[j].Name)+"=="+legacyRecords[j].Version
	})
	for _, record := range legacyRecords {
		key := normalizePackageName(record.Name)
		previous, exists := db.Packages[key]
		if !exists {
			db.Packages[key] = record
			continue
		}
		cmp, _ := compareVersions(record.Version, previous.Version)
		if cmp > 0 {
			db.LegacyPackages[key+"=="+previous.Version] = previous
			db.Packages[key] = record
		} else {
			db.LegacyPackages[key+"=="+record.Version] = record
		}
	}
	if err := saveDatabase(path, db); err != nil {
		return Database{}, err
	}
	return db, nil
}

func saveDatabase(path string, db Database) error {
	db.SchemaVersion = databaseSchemaVersion
	if db.Packages == nil {
		db.Packages = map[string]InstalledPackage{}
	}
	if db.LegacyPackages == nil {
		db.LegacyPackages = map[string]InstalledPackage{}
	}
	if err := atomicWriteJSON(path, db); err != nil {
		return appError("io_error", err.Error())
	}
	return nil
}

func databasePackageKey(name string) string { return normalizePackageName(name) }

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func safeStoreDirectoryName(name string) bool {
	return name != "" && name != "." && name != ".." && !filepath.IsAbs(name) && filepath.Base(name) == name && !strings.Contains(name, "\\")
}
func listInstalled(db Database) []string {
	keys := make([]string, 0, len(db.Packages))
	for key := range db.Packages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		p := db.Packages[key]
		suffix := ""
		if p.Frozen {
			suffix = " [frozen]"
		}
		lines = append(lines, p.Name+"=="+p.Version+suffix)
	}
	legacyKeys := make([]string, 0, len(db.LegacyPackages))
	for key := range db.LegacyPackages {
		legacyKeys = append(legacyKeys, key)
	}
	sort.Strings(legacyKeys)
	for _, key := range legacyKeys {
		p := db.LegacyPackages[key]
		lines = append(lines, p.Name+"=="+p.Version+" [legacy]")
	}
	return lines
}
func frozenPackages(db Database) []string {
	keys := make([]string, 0)
	for key, p := range db.Packages {
		if p.Frozen {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		p := db.Packages[key]
		lines = append(lines, p.Name+"=="+p.Version)
	}
	return lines
}

func validateDatabase(db Database, storePath string) error {
	validateRecord := func(key string, p InstalledPackage, legacy bool) error {
		if !isValidPackageName(p.Name) || databasePackageKey(p.Name) == "" || !isValidVersion(p.Version) || !safeStoreDirectoryName(p.Dir) {
			return fmt.Errorf("invalid package database record %q", key)
		}
		if !legacy && databasePackageKey(p.Name) != key {
			return fmt.Errorf("package database key does not match package name: %q", key)
		}
		if legacy && key != databasePackageKey(p.Name)+"=="+p.Version {
			return fmt.Errorf("invalid legacy package database key: %q", key)
		}
		if !legacy && p.Source != "legacy" {
			if !isSHA256(p.Hash) || p.Dir != packageDirectory(p.Hash, p.Name, p.Version) || !isSHA256(p.ManifestHash) || !isSHA256(p.ChecksumsHash) {
				return fmt.Errorf("invalid package build identity in database: %q", key)
			}
		}
		for _, file := range p.Files {
			if !safeArchivePath(file) {
				return fmt.Errorf("invalid package file path in database: %q", file)
			}
		}
		for _, entry := range p.Entrypoints {
			if !safeArchivePath(entry) || !strings.HasPrefix(entry, "payload/bin/") {
				return fmt.Errorf("invalid package entrypoint in database: %q", entry)
			}
		}
		knownFiles := make(map[string]bool, len(p.Files))
		for _, file := range p.Files {
			knownFiles[file] = true
		}
		for file, mode := range p.Modes {
			if !safeArchivePath(file) || !strings.HasPrefix(file, "payload/") || !knownFiles[file] || (mode != uint32(os.ModeSymlink) && mode > 0o777) {
				return fmt.Errorf("invalid package mode record: %q", file)
			}
		}
		for _, dep := range p.Dependencies {
			if _, err := parseDependencyGroup(dep); err != nil {
				return fmt.Errorf("invalid dependency in package database: %w", err)
			}
		}
		if _, err := os.Lstat(filepath.Join(storePath, p.Dir)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	for key, p := range db.Packages {
		if err := validateRecord(key, p, false); err != nil {
			return err
		}
	}
	for key, p := range db.LegacyPackages {
		if err := validateRecord(key, p, true); err != nil {
			return err
		}
	}
	return nil
}

func verifyStoredPackage(paths AppPaths, p InstalledPackage) error {
	root := filepath.Join(paths.Store, p.Dir)
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("package store path is not a real directory")
	}
	metadataRaw, err := os.ReadFile(filepath.Join(root, "package.toml"))
	if err != nil {
		return err
	}
	manifest, err := parsePackageTOML(metadataRaw)
	if err != nil {
		return err
	}
	if normalizePackageName(manifest.Name) != normalizePackageName(p.Name) || !versionsEqual(manifest.Version, p.Version) {
		return fmt.Errorf("stored package identity mismatch")
	}
	if p.Hash != "" && p.Dir != packageDirectory(p.Hash, p.Name, p.Version) {
		return fmt.Errorf("stored package build identity mismatch")
	}
	if !sameStrings(manifest.Dependencies, p.Dependencies) || !sameStrings(manifest.Entrypoints, p.Entrypoints) {
		return fmt.Errorf("stored package metadata does not match database")
	}
	checksums, err := os.ReadFile(filepath.Join(root, "checksums.sha256"))
	if err != nil {
		return err
	}
	if p.ManifestHash != "" {
		sum := sha256.Sum256(metadataRaw)
		if hex.EncodeToString(sum[:]) != p.ManifestHash {
			return fmt.Errorf("stored package manifest checksum mismatch")
		}
	}
	if p.ChecksumsHash != "" {
		sum := sha256.Sum256(checksums)
		if hex.EncodeToString(sum[:]) != p.ChecksumsHash {
			return fmt.Errorf("stored checksum manifest checksum mismatch")
		}
	}
	checked := map[string]bool{}
	for _, line := range strings.Split(string(checksums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return fmt.Errorf("invalid stored checksum record")
		}
		digest := strings.TrimPrefix(parts[0], "*")
		name := strings.TrimPrefix(parts[1], "*")
		if !strings.HasPrefix(name, "payload/") || !safeArchivePath(name) || len(digest) != 64 {
			return fmt.Errorf("invalid stored checksum path or digest")
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return fmt.Errorf("invalid stored checksum digest")
		}
		name = filepath.ToSlash(name)
		if checked[name] {
			return fmt.Errorf("duplicate stored checksum path: %s", name)
		}
		filePath := filepath.Join(root, filepath.FromSlash(name))
		fileInfo, err := os.Lstat(filePath)
		if err != nil {
			return err
		}
		if expectedMode, hasMode := p.Modes[name]; hasMode {
			if expectedMode&uint32(os.ModeSymlink) != 0 {
				if fileInfo.Mode()&os.ModeSymlink == 0 {
					return fmt.Errorf("stored file type does not match package metadata: %s", name)
				}
			} else if !fileInfo.Mode().IsRegular() || uint32(fileInfo.Mode().Perm()) != expectedMode {
				return fmt.Errorf("stored file permissions do not match package metadata: %s", name)
			}
		}
		var content []byte
		if fileInfo.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(filePath)
			if readErr != nil {
				return readErr
			}
			if !safeSymlinkTarget(name, target) {
				return fmt.Errorf("stored payload contains an unsafe symlink: %s", name)
			}
			content = []byte(target)
		} else if fileInfo.Mode().IsRegular() {
			content, err = os.ReadFile(filePath)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("stored payload contains an unsupported file type: %s", name)
		}
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != strings.ToLower(digest) {
			return fmt.Errorf("stored file checksum mismatch: %s", name)
		}
		checked[name] = true
	}
	actual := map[string]bool{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			relativePath, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			if !safeSymlinkTarget(filepath.ToSlash(relativePath), linkTarget) {
				return fmt.Errorf("stored package contains an unsafe symlink: %s", path)
			}
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("stored package contains an unsupported file type: %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		actual[filepath.ToSlash(rel)] = true
		return nil
	}); err != nil {
		return err
	}
	expectedFiles := map[string]bool{}
	for _, name := range p.Files {
		if !safeArchivePath(name) || expectedFiles[name] {
			return fmt.Errorf("invalid or duplicate file record: %s", name)
		}
		expectedFiles[name] = true
		if !actual[name] {
			return fmt.Errorf("stored file is missing: %s", name)
		}
		if strings.HasPrefix(name, "payload/") && !checked[name] {
			return fmt.Errorf("stored file is missing from checksum manifest: %s", name)
		}
	}
	if len(actual) != len(expectedFiles) {
		return fmt.Errorf("stored package file set does not match database")
	}
	for name := range actual {
		if !expectedFiles[name] {
			return fmt.Errorf("unregistered file in package store: %s", name)
		}
	}
	return nil
}
