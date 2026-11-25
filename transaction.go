package nixflux

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type transactionMove struct {
	Kind        string `json:"kind"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
}
type transactionLink struct {
	Name      string `json:"name"`
	OldTarget string `json:"old_target"`
	NewTarget string `json:"new_target"`
}
type transactionJournal struct {
	State           string            `json:"state"`
	StageRoot       string            `json:"stage_root"`
	DatabaseExisted bool              `json:"database_existed"`
	DatabaseBackup  []byte            `json:"database_backup"`
	SettingsChanged bool              `json:"settings_changed,omitempty"`
	SettingsExisted bool              `json:"settings_existed,omitempty"`
	SettingsBackup  []byte            `json:"settings_backup,omitempty"`
	SettingsContent []byte            `json:"settings_content,omitempty"`
	ProfileChanged  bool              `json:"profile_changed,omitempty"`
	ProfileName     string            `json:"profile_name,omitempty"`
	ProfileExisted  bool              `json:"profile_existed,omitempty"`
	ProfileBackup   []byte            `json:"profile_backup,omitempty"`
	ProfileContent  []byte            `json:"profile_content,omitempty"`
	CurrentProfileChanged  bool       `json:"current_profile_changed,omitempty"`
	CurrentProfileExisted  bool       `json:"current_profile_existed,omitempty"`
	CurrentProfileBackup   []byte     `json:"current_profile_backup,omitempty"`
	CurrentProfileContent  []byte     `json:"current_profile_content,omitempty"`
	Moves           []transactionMove `json:"moves"`
	Links           []transactionLink `json:"links"`
}

func recoverTransaction(paths AppPaths) error {
	raw, err := os.ReadFile(paths.Transaction)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var journal transactionJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return fmt.Errorf("invalid transaction journal: %w", err)
	}
	if err := validateTransactionJournal(paths, journal); err != nil {
		return fmt.Errorf("invalid transaction journal: %w", err)
	}
	if journal.State == "committed" {
		if err := os.RemoveAll(journal.StageRoot); err != nil {
			return err
		}
		syncDirectory(paths.Staging)
		if err := os.Remove(paths.Transaction); err != nil {
			return err
		}
		syncDirectory(paths.Set)
		return nil
	}
	if journal.State != "prepared" {
		return fmt.Errorf("unknown transaction state %q", journal.State)
	}
	if err := rollbackTransaction(paths, journal); err != nil {
		return err
	}
	return nil
}

func validateTransactionJournal(paths AppPaths, journal transactionJournal) error {
	if !filepath.IsAbs(journal.StageRoot) || filepath.Dir(filepath.Clean(journal.StageRoot)) != filepath.Clean(paths.Staging) || filepath.Base(journal.StageRoot) == "." || filepath.Base(journal.StageRoot) == ".." {
		return fmt.Errorf("transaction staging path is outside the staging directory")
	}
	for _, move := range journal.Moves {
		if !filepath.IsAbs(move.Source) || !filepath.IsAbs(move.Destination) {
			return fmt.Errorf("transaction contains a relative move path")
		}
		switch move.Kind {
		case "install":
			if !pathWithin(journal.StageRoot, move.Source) || !pathWithin(paths.Store, move.Destination) || filepath.Dir(filepath.Clean(move.Destination)) != filepath.Clean(paths.Store) {
				return fmt.Errorf("invalid package installation move")
			}
		case "remove":
			if !pathWithin(paths.Store, move.Source) || filepath.Dir(filepath.Clean(move.Source)) != filepath.Clean(paths.Store) || !pathWithin(journal.StageRoot, move.Destination) {
				return fmt.Errorf("invalid package removal move")
			}
		default:
			return fmt.Errorf("unknown transaction move kind %q", move.Kind)
		}
	}
	for _, link := range journal.Links {
		if link.Name == "" || filepath.Base(link.Name) != link.Name || strings.ContainsAny(link.Name, `/\`) || link.Name == "." || link.Name == ".." {
			return fmt.Errorf("invalid entrypoint name in transaction")
		}
		for _, target := range []string{link.OldTarget, link.NewTarget} {
			if target != "" && (!filepath.IsAbs(target) || !pathWithin(paths.Store, target)) {
				return fmt.Errorf("invalid entrypoint target in transaction")
			}
		}
	}
	if journal.SettingsChanged {
		settings, err := parseSettingsTOML(string(journal.SettingsContent))
		if err != nil {
			return fmt.Errorf("invalid settings content in transaction: %w", err)
		}
		if err := normalizeSettings(&settings); err != nil {
			return fmt.Errorf("invalid settings content in transaction: %w", err)
		}
	}
	if journal.ProfileChanged {
		_, err := profilePath(paths, journal.ProfileName)
		if err != nil {
			return fmt.Errorf("invalid profile in transaction: %w", err)
		}
		if _, err := parseProfileTOML(string(journal.ProfileContent)); err != nil {
			return fmt.Errorf("invalid profile content in transaction: %w", err)
		}
	}
	if journal.CurrentProfileChanged {
		filename := strings.TrimSpace(string(journal.CurrentProfileContent))
		path, err := profilePath(paths, filename)
		if err != nil {
			return fmt.Errorf("invalid current profile in transaction: %w", err)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("current profile in transaction does not exist")
		}
	}
	return nil
}

func applyTransaction(paths AppPaths, journal transactionJournal, db Database) error {
	journal.State = "prepared"
	if err := atomicWriteJSON(paths.Transaction, journal); err != nil {
		_ = os.RemoveAll(journal.StageRoot)
		return err
	}
	fail := func(cause error) error {
		rollbackErr := rollbackTransaction(paths, journal)
		if rollbackErr != nil {
			return fmt.Errorf("%v; rollback failed: %w", cause, rollbackErr)
		}
		return cause
	}
	for _, move := range journal.Moves {
		if err := os.MkdirAll(filepath.Dir(move.Destination), 0o755); err != nil {
			return fail(err)
		}
		if err := os.Rename(move.Source, move.Destination); err != nil {
			return fail(err)
		}
		syncDirectory(filepath.Dir(move.Source))
		syncDirectory(filepath.Dir(move.Destination))
	}
	for _, link := range journal.Links {
		path := filepath.Join(paths.Bin, link.Name)
		if link.NewTarget == "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fail(err)
			}
			syncDirectory(paths.Bin)
		} else if err := atomicSymlink(link.NewTarget, path); err != nil {
			return fail(err)
		}
	}
	if err := saveDatabase(paths.Database, db); err != nil {
		return fail(err)
	}
	if journal.SettingsChanged {
		if err := atomicWriteFile(paths.Settings, journal.SettingsContent, 0o644); err != nil {
			return fail(err)
		}
	}
	if journal.ProfileChanged {
		path, _ := profilePath(paths, journal.ProfileName)
		if err := atomicWriteFile(path, journal.ProfileContent, 0o644); err != nil {
			return fail(err)
		}
	}
	if journal.CurrentProfileChanged {
		if err := atomicWriteFile(paths.CurrentProfile, journal.CurrentProfileContent, 0o600); err != nil {
			return fail(err)
		}
	}
	journal.State = "committed"
	if err := atomicWriteJSON(paths.Transaction, journal); err != nil {
		return fail(err)
	}
	if err := os.RemoveAll(journal.StageRoot); err != nil {
		return err
	}
	syncDirectory(paths.Staging)
	if err := os.Remove(paths.Transaction); err != nil && !os.IsNotExist(err) {
		return err
	}
	syncDirectory(paths.Set)
	return nil
}

func rollbackTransaction(paths AppPaths, journal transactionJournal) error {
	var first error
	for i := len(journal.Links) - 1; i >= 0; i-- {
		link := journal.Links[i]
		path := filepath.Join(paths.Bin, link.Name)
		if link.OldTarget == "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) && first == nil {
				first = err
			}
			syncDirectory(paths.Bin)
		} else if err := atomicSymlink(link.OldTarget, path); err != nil && first == nil {
			first = err
		}
	}
	for i := len(journal.Moves) - 1; i >= 0; i-- {
		move := journal.Moves[i]
		switch move.Kind {
		case "install":
			// If an existing directory was moved aside first, its backup proves
			// that this destination can only contain the newly installed package.
			if _, sourceErr := os.Lstat(move.Source); os.IsNotExist(sourceErr) {
				shouldRemove := true
				for _, previous := range journal.Moves {
					if previous.Kind == "remove" && previous.Source == move.Destination {
						if _, backupErr := os.Lstat(previous.Destination); os.IsNotExist(backupErr) {
							shouldRemove = false
						} else if backupErr != nil {
							first = backupErr
							shouldRemove = false
						}
						break
					}
				}
				if shouldRemove {
					if err := os.RemoveAll(move.Destination); err != nil && first == nil {
						first = err
					} else {
						syncDirectory(filepath.Dir(move.Destination))
					}
				}
			} else if sourceErr != nil && first == nil {
				first = sourceErr
			}
		case "remove":
			if _, err := os.Lstat(move.Destination); err == nil {
				_ = os.MkdirAll(filepath.Dir(move.Source), 0o755)
				if e := os.Rename(move.Destination, move.Source); e != nil && first == nil {
					first = e
				} else {
					syncDirectory(filepath.Dir(move.Destination))
					syncDirectory(filepath.Dir(move.Source))
				}
			} else if os.IsNotExist(err) {
				if _, sourceErr := os.Lstat(move.Source); os.IsNotExist(sourceErr) && first == nil {
					first = fmt.Errorf("cannot restore removed package; both source and backup are missing: %s", move.Source)
				} else if sourceErr != nil && first == nil {
					first = sourceErr
				}
			} else if first == nil {
				first = err
			}
		}
	}
	if journal.DatabaseExisted {
		if err := atomicWriteFile(paths.Database, journal.DatabaseBackup, 0o644); err != nil && first == nil {
			first = err
		}
	} else {
		if err := os.Remove(paths.Database); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	if journal.SettingsChanged {
		if journal.SettingsExisted {
			if err := atomicWriteFile(paths.Settings, journal.SettingsBackup, 0o644); err != nil && first == nil {
				first = err
			}
		} else if err := os.Remove(paths.Settings); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	if journal.ProfileChanged {
		path, pathErr := profilePath(paths, journal.ProfileName)
		if pathErr != nil && first == nil {
			first = pathErr
		} else if pathErr == nil {
			if journal.ProfileExisted {
				if err := atomicWriteFile(path, journal.ProfileBackup, 0o644); err != nil && first == nil {
					first = err
				}
			} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) && first == nil {
				first = err
			}
		}
	}
	if journal.CurrentProfileChanged {
		if journal.CurrentProfileExisted {
			if err := atomicWriteFile(paths.CurrentProfile, journal.CurrentProfileBackup, 0o600); err != nil && first == nil {
				first = err
			}
		} else if err := os.Remove(paths.CurrentProfile); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	if first == nil {
		if err := os.RemoveAll(journal.StageRoot); err != nil {
			first = err
		} else {
			syncDirectory(paths.Staging)
		}
	}
	if first == nil {
		if err := os.Remove(paths.Transaction); err != nil && !os.IsNotExist(err) {
			first = err
		} else {
			syncDirectory(paths.Set)
		}
	}
	return first
}

func buildLinkChanges(paths AppPaths, oldDB, newDB Database, oldSettings, newSettings Settings) ([]transactionLink, error) {
	oldLinks, err := packageLinkMap(paths, oldDB, oldSettings)
	if err != nil {
		return nil, err
	}
	newLinks, err := packageLinkMap(paths, newDB, newSettings)
	if err != nil {
		return nil, err
	}
	all := map[string]bool{}
	for name := range oldLinks {
		all[name] = true
	}
	for name := range newLinks {
		all[name] = true
	}
	// Include stale links created by older NixFlux versions, which published
	// every installed package regardless of settings.enabled.
	entries, readDirErr := os.ReadDir(paths.Bin)
	if readDirErr != nil && !os.IsNotExist(readDirErr) {
		return nil, readDirErr
	}
	for _, entry := range entries {
		path := filepath.Join(paths.Bin, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return nil, statErr
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, linkErr := os.Readlink(path)
		if linkErr != nil {
			return nil, linkErr
		}
		if filepath.IsAbs(target) && pathWithin(paths.Store, target) {
			all[entry.Name()] = true
		}
	}

	changes := make([]transactionLink, 0, len(all))
	for name := range all {
		oldExpected := oldLinks[name]
		newTarget := newLinks[name]
		current, readErr := readLink(filepath.Join(paths.Bin, name))
		if readErr != nil {
			return nil, readErr
		}
		if current != "" && current != oldExpected {
			owned := filepath.IsAbs(current) && pathWithin(paths.Store, current)
			if !owned {
				if newTarget != "" {
					return nil, appError("binary_conflict", name)
				}
				continue
			}
		}
		if current != newTarget {
			changes = append(changes, transactionLink{Name: name, OldTarget: current, NewTarget: newTarget})
		}
	}
	sortTransactionLinks(changes)
	return changes, nil
}

func sortTransactionLinks(links []transactionLink) {
	for i := 1; i < len(links); i++ {
		for j := i; j > 0 && links[j].Name < links[j-1].Name; j-- {
			links[j], links[j-1] = links[j-1], links[j]
		}
	}
}

func migrateLegacyCommandLinks(paths AppPaths, db Database, settings Settings) error {
	desired, err := packageLinkMap(paths, db, settings)
	if err != nil {
		return err
	}
	if filepath.Clean(paths.Bin) != filepath.Clean(paths.LegacyBin) && paths.LegacyBin != "" {
		entries, readErr := os.ReadDir(paths.LegacyBin)
		if os.IsNotExist(readErr) {
			entries = nil
		} else if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			oldPath := filepath.Join(paths.LegacyBin, entry.Name())
			info, statErr := os.Lstat(oldPath)
			if os.IsNotExist(statErr) {
				continue
			}
			if statErr != nil {
				return statErr
			}
			if info.Mode()&os.ModeSymlink == 0 {
				continue
			}
			oldTarget, linkErr := os.Readlink(oldPath)
			if linkErr != nil {
				return linkErr
			}
			if !filepath.IsAbs(oldTarget) || !pathWithin(paths.Store, oldTarget) {
				continue
			}
			if target := desired[entry.Name()]; target != "" {
				if err := os.MkdirAll(paths.Bin, 0o755); err != nil {
					return err
				}
				current, readErr := readLink(filepath.Join(paths.Bin, entry.Name()))
				if readErr != nil {
					return readErr
				}
				if current != "" && current != target {
					owned := filepath.IsAbs(current) && pathWithin(paths.Store, current)
					if !owned {
						return appError("binary_conflict", entry.Name())
					}
				}
				if current != target {
					if err := atomicSymlink(target, filepath.Join(paths.Bin, entry.Name())); err != nil {
						return err
					}
				}
			}
			if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			syncDirectory(paths.LegacyBin)
		}
	}
	return reconcileCommandLinks(paths, db, settings)
}

func reconcileCommandLinks(paths AppPaths, db Database, settings Settings) error {
	desired, err := packageLinkMap(paths, db, settings)
	if err != nil {
		return err
	}
	// Do not publish a dangling or non-executable command. Keeping the target
	// enabled lets env update repair the package without exposing a broken link.
	for name, target := range desired {
		info, statErr := os.Stat(target)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			delete(desired, name)
		}
	}
	if err := os.MkdirAll(paths.Bin, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(paths.Bin)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(paths.Bin, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		wanted := desired[entry.Name()]
		if info.Mode()&os.ModeSymlink == 0 {
			if wanted != "" {
				return appError("binary_conflict", entry.Name())
			}
			continue
		}
		current, linkErr := os.Readlink(path)
		if linkErr != nil {
			return linkErr
		}
		if !filepath.IsAbs(current) || !pathWithin(paths.Store, current) {
			if wanted != "" {
				return appError("binary_conflict", entry.Name())
			}
			continue
		}
		if wanted == "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			syncDirectory(paths.Bin)
			continue
		}
		if current != wanted {
			if err := atomicSymlink(wanted, path); err != nil {
				return err
			}
		}
		delete(desired, entry.Name())
	}
	for name, target := range desired {
		if _, err := os.Lstat(target); err != nil {
			return fmt.Errorf("enabled command target for %s is unavailable: %w", name, err)
		}
		if err := atomicSymlink(target, filepath.Join(paths.Bin, name)); err != nil {
			return err
		}
	}
	if len(desired) > 0 {
		syncDirectory(paths.Bin)
	}
	return nil
}

func packageLinkMap(paths AppPaths, db Database, settings Settings) (map[string]string, error) {
	result := map[string]string{}
	packages := packagesForProfile(db, settings.ProfileName)
	for _, spec := range settings.Enabled {
		name, version, err := parsePackageVersion(spec)
		if err != nil {
			return nil, appError("invalid_config", "enabled entry must be name==version: "+spec)
		}
		pkg, installed := packages[normalizePackageName(name)]
		if !installed || !versionsEqual(pkg.Version, version) {
			continue
		}
		for _, entry := range pkg.Entrypoints {
			clean := filepath.ToSlash(entry)
			if !strings.HasPrefix(clean, "payload/bin/") {
				return nil, fmt.Errorf("invalid entrypoint in database: %s", entry)
			}
			binaryName := filepath.Base(clean)
			target := filepath.Join(paths.Store, pkg.Dir, filepath.FromSlash(clean))
			if previous, ok := result[binaryName]; ok && previous != target {
				return nil, appError("binary_conflict", binaryName)
			}
			result[binaryName] = target
		}
	}
	return result, nil
}

func readLink(path string) (string, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", appError("binary_conflict", filepath.Base(path))
	}
	return os.Readlink(path)
}

func databaseBackup(path string) (bool, []byte, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, raw, nil
}
