package nixflux

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func profileLauncherPath(paths AppPaths, profile, generation, command string) (string, error) {
	if _, err := profileFilename(strings.TrimSuffix(profile, ".toml")); err != nil || !strings.HasSuffix(profile, ".toml") {
		return "", appError("invalid_config", "invalid launcher profile")
	}
	if !generationIDPattern.MatchString(generation) || command == "" || filepath.Base(command) != command || strings.ContainsAny(command, `/\\`) || command == "." || command == ".." {
		return "", appError("invalid_config", "invalid profile launcher path")
	}
	dir := filepath.Join(paths.Profiles, strings.TrimSuffix(profile, ".toml"), "launchers", generation)
	if !pathWithin(paths.Profiles, filepath.Join(dir, command)) {
		return "", appError("invalid_config", "profile launcher is outside profiles")
	}
	return filepath.Join(dir, command), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func packageRuntimeLibraries(paths AppPaths, packages map[string]InstalledPackage) []string {
	set := map[string]bool{}
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(file)))
			base := filepath.Base(clean)
			if !strings.HasPrefix(clean, "payload/") || !(strings.Contains(base, ".so.") || strings.HasSuffix(base, ".so")) {
				continue
			}
			dir := filepath.Join(paths.Store, pkg.Dir, filepath.Dir(filepath.FromSlash(clean)))
			set[dir] = true
		}
	}
	result := make([]string, 0, len(set))
	for dir := range set {
		result = append(result, dir)
	}
	sort.Strings(result)
	return result
}

func launcherContent(executable string, libraryDirs []string) []byte {
	loader := "/bin/sh"
	if runtimeGOOS == "android" {
		loader = "/system/bin/sh"
	}
	content := "#!" + loader + "\n"
	if len(libraryDirs) > 0 {
		content += "NIXFLUX_LIBRARY_PATH=" + shellQuote(strings.Join(libraryDirs, ":")) + "\n"
		content += "if [ -n \"${LD_LIBRARY_PATH:-}\" ]; then\n  LD_LIBRARY_PATH=\"$NIXFLUX_LIBRARY_PATH:$LD_LIBRARY_PATH\"\nelse\n  LD_LIBRARY_PATH=$NIXFLUX_LIBRARY_PATH\nfi\nexport LD_LIBRARY_PATH\n"
	}
	content += "exec " + shellQuote(executable) + " \"$@\"\n"
	return []byte(content)
}

func profileLauncherChanges(paths AppPaths, db Database, settings Settings) ([]transactionFileChange, error) {
	if settings.GenerationID == "" {
		return nil, nil
	}
	packages := packagesForProfile(db, settings.ProfileName)
	libraries := packageRuntimeLibraries(paths, packages)
	changes := []transactionFileChange{}
	for _, spec := range settings.Enabled {
		name, version, err := parsePackageVersion(spec)
		if err != nil {
			return nil, err
		}
		pkg, ok := packages[normalizePackageName(name)]
		if !ok || !versionsEqual(pkg.Version, version) {
			continue
		}
		for _, entry := range pkg.Entrypoints {
			clean := filepath.ToSlash(entry)
			if !strings.HasPrefix(clean, "payload/bin/") {
				return nil, fmt.Errorf("invalid entrypoint in database: %s", entry)
			}
			command := filepath.Base(clean)
			path, err := profileLauncherPath(paths, settings.ProfileName, settings.GenerationID, command)
			if err != nil {
				return nil, err
			}
			if err := validateNoSymlinkParents(paths.Profiles, path); err != nil {
				return nil, wrapAppError("invalid_config", "unsafe profile launcher path", err)
			}
			executable := filepath.Join(paths.Store, pkg.Dir, filepath.FromSlash(clean))
			change, err := captureTransactionFileChange(path, launcherContent(executable, libraries), 0o755, false)
			if err != nil {
				return nil, wrapAppError("io_error", "prepare profile command launcher", err)
			}
			changes = append(changes, change)
		}
	}
	return changes, nil
}

func ensureProfileLaunchers(paths AppPaths, db Database, settings Settings) error {
	changes, err := profileLauncherChanges(paths, db, settings)
	if err != nil {
		return err
	}
	for _, change := range changes {
		if info, statErr := os.Lstat(change.Path); statErr == nil {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				return appError("invalid_config", "profile launcher is not a regular executable file")
			}
			current, readErr := os.ReadFile(change.Path)
			if readErr != nil {
				return readErr
			}
			if string(current) == string(change.Content) {
				continue
			}
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		if err := atomicWriteFile(change.Path, change.Content, 0o755); err != nil {
			return wrapAppError("io_error", "repair profile command launcher", err)
		}
	}
	return nil
}
