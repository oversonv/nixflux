package nixflux

import (
	"bytes"
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
type transactionFileChange struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
	Backup  []byte `json:"backup,omitempty"`
	Content []byte `json:"content,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
	Delete  bool   `json:"delete,omitempty"`
}
type transactionJournal struct {
	State                 string                  `json:"state"`
	StageRoot             string                  `json:"stage_root"`
	DatabaseExisted       bool                    `json:"database_existed"`
	DatabaseBackup        []byte                  `json:"database_backup"`
	SettingsChanged       bool                    `json:"settings_changed,omitempty"`
	SettingsExisted       bool                    `json:"settings_existed,omitempty"`
	SettingsBackup        []byte                  `json:"settings_backup,omitempty"`
	SettingsContent       []byte                  `json:"settings_content,omitempty"`
	ProfileChanged        bool                    `json:"profile_changed,omitempty"`
	ProfileName           string                  `json:"profile_name,omitempty"`
	ProfileExisted        bool                    `json:"profile_existed,omitempty"`
	ProfileBackup         []byte                  `json:"profile_backup,omitempty"`
	ProfileContent        []byte                  `json:"profile_content,omitempty"`
	CurrentProfileChanged bool                    `json:"current_profile_changed,omitempty"`
	CurrentProfileExisted bool                    `json:"current_profile_existed,omitempty"`
	CurrentProfileBackup  []byte                  `json:"current_profile_backup,omitempty"`
	CurrentProfileContent []byte                  `json:"current_profile_content,omitempty"`
	Moves                 []transactionMove       `json:"moves"`
	Links                 []transactionLink       `json:"links"`
	Files                 []transactionFileChange `json:"files,omitempty"`
}

func captureTransactionFileChange(path string, content []byte, mode os.FileMode, remove bool) (transactionFileChange, error) {
	change := transactionFileChange{Path: filepath.Clean(path), Content: append([]byte(nil), content...), Mode: uint32(mode.Perm()), Delete: remove}
	info, err := os.Lstat(change.Path)
	if os.IsNotExist(err) {
		return change, nil
	}
	if err != nil {
		return transactionFileChange{}, err
	}
	if !info.Mode().IsRegular() {
		return transactionFileChange{}, appError("invalid_config", "transaction metadata path must be a regular file")
	}
	backup, err := os.ReadFile(change.Path)
	if err != nil {
		return transactionFileChange{}, err
	}
	change.Existed = true
	change.Backup = backup
	return change, nil
}

func validateTransactionFileChange(paths AppPaths, change transactionFileChange) error {
	if !filepath.IsAbs(change.Path) || filepath.Clean(change.Path) != change.Path || (change.Mode != 0o600 && change.Mode != 0o644 && change.Mode != 0o755) {
		return fmt.Errorf("invalid transaction metadata file path or mode")
	}
	if !change.Existed && len(change.Backup) != 0 {
		return fmt.Errorf("transaction metadata has a backup for a missing file")
	}
	if change.Delete && len(change.Content) != 0 {
		return fmt.Errorf("transaction metadata delete contains replacement content")
	}
	if change.Delete && change.Mode == 0 {
		return fmt.Errorf("transaction metadata delete has no restore mode")
	}

	profile, id, kind, err := transactionProfileMetadataPath(paths, change.Path)
	if err != nil {
		return err
	}
	if err := validateNoSymlinkParents(paths.Profiles, change.Path); err != nil {
		return fmt.Errorf("unsafe transaction metadata path: %w", err)
	}
	if change.Mode == 0o755 && kind != "launcher" {
		return fmt.Errorf("executable mode is allowed only for profile launchers")
	}
	if !change.Delete {
		switch kind {
		case "profile":
			if _, err := parseProfileTOML(string(change.Content)); err != nil {
				return fmt.Errorf("invalid profile content in transaction: %w", err)
			}
		case "profile-lock":
			filename := strings.TrimSpace(string(change.Content))
			path, err := profilePath(paths, filename)
			if err != nil {
				return fmt.Errorf("invalid default profile in transaction: %w", err)
			}
			if info, statErr := os.Lstat(path); statErr != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("default profile in transaction does not exist")
			}
		case "current-profile":
			filename := strings.TrimSpace(string(change.Content))
			path, err := profilePath(paths, filename)
			if err != nil {
				return fmt.Errorf("invalid current profile in transaction: %w", err)
			}
			if info, statErr := os.Lstat(path); statErr != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("current profile in transaction does not exist")
			}
		case "generation-pointer":
			if !generationIDPattern.MatchString(strings.TrimSpace(string(change.Content))) {
				return fmt.Errorf("invalid generation pointer in transaction")
			}
		case "generation-manifest":
			if err := validateGenerationPayload(change.Content, profile, id); err != nil {
				return err
			}
		case "launcher":
			if !strings.HasPrefix(string(change.Content), "#!/") || !strings.Contains(string(change.Content), "\nexec '") {
				return fmt.Errorf("invalid profile launcher content")
			}
		}
	}
	return nil
}

func transactionProfileMetadataPath(paths AppPaths, path string) (profile, id, kind string, err error) {
	switch path {
	case paths.ProfilesLock:
		return "", "", "profile-lock", nil
	case paths.CurrentProfile:
		return "", "", "current-profile", nil
	}
	rel, err := filepath.Rel(paths.Profiles, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", "", fmt.Errorf("transaction metadata path is outside profiles")
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 1 {
		filename, validateErr := profileFilename(strings.TrimSuffix(parts[0], ".toml"))
		if validateErr != nil || filename != parts[0] {
			return "", "", "", fmt.Errorf("invalid profile metadata path")
		}
		return filename, "", "profile", nil
	}
	profile, err = profileFilename(parts[0])
	if err != nil {
		return "", "", "", fmt.Errorf("invalid profile metadata path: %w", err)
	}
	if len(parts) == 2 && parts[1] == ".generation" {
		expected, pathErr := generationPointerPath(paths, profile)
		if pathErr != nil || expected != path {
			return "", "", "", fmt.Errorf("invalid generation pointer path")
		}
		return profile, "", "generation-pointer", nil
	}
	if len(parts) == 3 && parts[1] == "generations" && strings.HasSuffix(parts[2], ".json") {
		id = strings.TrimSuffix(parts[2], ".json")
		expected, pathErr := generationPath(paths, profile, id)
		if pathErr != nil || expected != path {
			return "", "", "", fmt.Errorf("invalid generation manifest path")
		}
		return profile, id, "generation-manifest", nil
	}
	if len(parts) == 4 && parts[1] == "launchers" {
		id = parts[2]
		if _, pathErr := profileLauncherPath(paths, profile, id, parts[3]); pathErr != nil {
			return "", "", "", fmt.Errorf("invalid profile launcher path")
		}
		return profile, id, "launcher", nil
	}
	return "", "", "", fmt.Errorf("unsupported transaction profile metadata path")
}

func recoverTransaction(paths AppPaths) error {
	return recoverTransactionWithSync(paths, syncDirectory)
}

func recoverTransactionWithSync(paths AppPaths, syncDir func(string) error) error {
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
		if err := syncDir(paths.Staging); err != nil {
			return err
		}
		if err := os.Remove(paths.Transaction); err != nil {
			return err
		}
		return syncDir(paths.Set)
	}
	if journal.State != "prepared" {
		return fmt.Errorf("unknown transaction state %q", journal.State)
	}
	if err := rollbackTransactionWithSync(paths, journal, syncDir); err != nil {
		return err
	}
	return nil
}

func validateTransactionJournal(paths AppPaths, journal transactionJournal) error {
	stagingInfo, stagingErr := os.Lstat(paths.Staging)
	if stagingErr != nil || !stagingInfo.IsDir() || stagingInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("transaction staging directory is missing or invalid")
	}
	if !filepath.IsAbs(journal.StageRoot) || filepath.Dir(filepath.Clean(journal.StageRoot)) != filepath.Clean(paths.Staging) || filepath.Base(journal.StageRoot) == "." || filepath.Base(journal.StageRoot) == ".." {
		return fmt.Errorf("transaction staging path is outside the staging directory")
	}
	if stageInfo, err := os.Lstat(journal.StageRoot); err == nil {
		if !stageInfo.IsDir() || stageInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("transaction staging root is not a real directory")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	moveSources := make(map[string]bool, len(journal.Moves))
	moveDestinations := make(map[string]bool, len(journal.Moves))
	for _, move := range journal.Moves {
		if !filepath.IsAbs(move.Source) || !filepath.IsAbs(move.Destination) || filepath.Clean(move.Source) != move.Source || filepath.Clean(move.Destination) != move.Destination {
			return fmt.Errorf("transaction contains a relative move path")
		}
		if moveSources[move.Source] || moveDestinations[move.Destination] {
			return fmt.Errorf("transaction contains duplicate move paths")
		}
		moveSources[move.Source] = true
		moveDestinations[move.Destination] = true
		switch move.Kind {
		case "install":
			newRoot := filepath.Join(journal.StageRoot, "new")
			if !pathWithin(newRoot, move.Source) || filepath.Dir(move.Source) != newRoot || !pathWithin(paths.Store, move.Destination) || filepath.Dir(move.Destination) != filepath.Clean(paths.Store) || filepath.Base(move.Source) != filepath.Base(move.Destination) || !safeStoreDirectoryName(filepath.Base(move.Source)) {
				return fmt.Errorf("invalid package installation move")
			}
			if err := validateNoSymlinkParents(journal.StageRoot, move.Source); err != nil {
				return fmt.Errorf("unsafe package installation source: %w", err)
			}
			if err := validateNoSymlinkParents(paths.Store, move.Destination); err != nil {
				return fmt.Errorf("unsafe package installation destination: %w", err)
			}
			if err := validateMoveDirectory(move.Source); err != nil {
				return err
			}
			if err := validateMoveDirectory(move.Destination); err != nil {
				return err
			}
		case "remove":
			oldRoot := filepath.Join(journal.StageRoot, "old")
			if !pathWithin(paths.Store, move.Source) || filepath.Dir(move.Source) != filepath.Clean(paths.Store) || !safeStoreDirectoryName(filepath.Base(move.Source)) || !pathWithin(oldRoot, move.Destination) || filepath.Dir(move.Destination) != oldRoot || filepath.Base(move.Source) != filepath.Base(move.Destination) {
				return fmt.Errorf("invalid package removal move")
			}
			if err := validateNoSymlinkParents(paths.Store, move.Source); err != nil {
				return fmt.Errorf("unsafe package removal source: %w", err)
			}
			if err := validateNoSymlinkParents(journal.StageRoot, move.Destination); err != nil {
				return fmt.Errorf("unsafe package removal destination: %w", err)
			}
			if err := validateMoveDirectory(move.Source); err != nil {
				return err
			}
			if err := validateMoveDirectory(move.Destination); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown transaction move kind %q", move.Kind)
		}
	}
	linkNames := make(map[string]bool, len(journal.Links))
	for _, link := range journal.Links {
		if link.Name == "" || filepath.Base(link.Name) != link.Name || strings.ContainsAny(link.Name, `/\`) || link.Name == "." || link.Name == ".." {
			return fmt.Errorf("invalid entrypoint name in transaction")
		}
		if linkNames[link.Name] {
			return fmt.Errorf("transaction contains duplicate command link mutations")
		}
		linkNames[link.Name] = true
		for _, target := range []string{link.OldTarget, link.NewTarget} {
			if target != "" && (!filepath.IsAbs(target) || !managedCommandTarget(paths, target)) {
				return fmt.Errorf("invalid entrypoint target in transaction")
			}
			if target != "" {
				root := paths.Store
				if !pathWithin(paths.Store, target) {
					root = paths.Profiles
				}
				if err := validateNoSymlinkParents(root, target); err != nil {
					return fmt.Errorf("unsafe entrypoint target: %w", err)
				}
			}
		}
	}
	for _, change := range journal.Files {
		if err := validateTransactionFileChange(paths, change); err != nil {
			return err
		}
	}
	filePaths := make(map[string]bool, len(journal.Files))
	for _, change := range journal.Files {
		if filePaths[change.Path] {
			return fmt.Errorf("transaction changes one metadata path more than once")
		}
		filePaths[change.Path] = true
		profile, _, kind, _ := transactionProfileMetadataPath(paths, change.Path)
		if kind != "generation-pointer" || change.Delete {
			continue
		}
		id := strings.TrimSpace(string(change.Content))
		manifestPath, pathErr := generationPath(paths, profile, id)
		if pathErr != nil {
			return pathErr
		}
		manifestInTransaction := false
		for _, candidate := range journal.Files {
			if candidate.Path == manifestPath && !candidate.Delete {
				manifestInTransaction = true
				break
			}
		}
		if !manifestInTransaction {
			if _, readErr := readProfileGeneration(paths, profile, id); readErr != nil {
				return fmt.Errorf("generation pointer references an unavailable manifest: %w", readErr)
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

func validateNoSymlinkParents(root, target string) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(target) || filepath.Clean(root) != root || filepath.Clean(target) != target || !pathWithin(root, target) {
		return fmt.Errorf("path is outside its managed root")
	}
	rootInfo, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("managed root must be a real directory: %s", root)
	}
	relative, _ := filepath.Rel(root, target)
	parts := strings.Split(relative, string(os.PathSeparator))
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("managed path contains a non-directory or symbolic link: %s", current)
		}
	}
	return nil
}

func validateMoveDirectory(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("transaction move path must be a real directory: %s", path)
	}
	return nil
}

func applyTransaction(paths AppPaths, journal transactionJournal, db Database) error {
	return applyTransactionObserved(paths, journal, db, nil)
}

// applyTransactionObserved allows tests to stop at durable mutation boundaries,
// leaving the journal behind as if the process had exited at that point.
func applyTransactionObserved(paths AppPaths, journal transactionJournal, db Database, afterMutation func(string) error) error {
	checkpoint := func(name string) error {
		if afterMutation == nil {
			return nil
		}
		return afterMutation(name)
	}
	cleanupUnprepared := func() {
		stageRoot := filepath.Clean(journal.StageRoot)
		stagingInfo, stagingErr := os.Lstat(paths.Staging)
		if filepath.IsAbs(journal.StageRoot) && filepath.Dir(stageRoot) == filepath.Clean(paths.Staging) && filepath.Base(stageRoot) != "." && filepath.Base(stageRoot) != ".." && stagingErr == nil && stagingInfo.IsDir() && stagingInfo.Mode()&os.ModeSymlink == 0 {
			_ = os.RemoveAll(stageRoot)
			syncDirectory(paths.Staging)
		}
	}
	if err := validateTransactionJournal(paths, journal); err != nil {
		cleanupUnprepared()
		return wrapAppError("transaction_failed", "validate transaction before commit", err)
	}
	for _, change := range journal.Files {
		info, err := os.Lstat(change.Path)
		if !change.Existed {
			if err == nil {
				cleanupUnprepared()
				return appError("transaction_failed", "profile metadata path appeared before commit: "+change.Path)
			}
			if !os.IsNotExist(err) {
				cleanupUnprepared()
				return wrapAppError("io_error", "check profile metadata before commit", err)
			}
			continue
		}
		if err != nil {
			cleanupUnprepared()
			return wrapAppError("transaction_failed", "profile metadata changed before commit", err)
		}
		if !info.Mode().IsRegular() {
			cleanupUnprepared()
			return appError("transaction_failed", "profile metadata changed before commit: "+change.Path)
		}
		current, err := os.ReadFile(change.Path)
		if err != nil {
			cleanupUnprepared()
			return wrapAppError("io_error", "check profile metadata before commit", err)
		}
		if !bytes.Equal(current, change.Backup) {
			cleanupUnprepared()
			return appError("transaction_failed", "profile metadata changed before commit: "+change.Path)
		}
	}
	for _, link := range journal.Links {
		path := filepath.Join(paths.Bin, link.Name)
		if err := validateNoSymlinkParents(paths.Bin, path); err != nil {
			cleanupUnprepared()
			return wrapAppError("invalid_config", "unsafe command link path", err)
		}
		current, err := readLink(path)
		if err != nil {
			cleanupUnprepared()
			return err
		}
		if current != link.OldTarget {
			cleanupUnprepared()
			return appError("transaction_failed", "command link changed before commit: "+link.Name)
		}
	}
	if err := syncTree(journal.StageRoot, syncRegularFile, syncDirectory); err != nil {
		cleanupUnprepared()
		return wrapAppError("io_error", "persist staged transaction data", err)
	}
	if err := syncDirectory(paths.Staging); err != nil {
		cleanupUnprepared()
		return wrapAppError("io_error", "persist transaction staging entry", err)
	}
	journal.State = "prepared"
	if err := atomicWriteJSON(paths.Transaction, journal); err != nil {
		_ = os.RemoveAll(journal.StageRoot)
		return err
	}
	if err := checkpoint("prepared_journal"); err != nil {
		return err
	}
	fail := func(cause error) error {
		rollbackErr := rollbackTransaction(paths, journal)
		if rollbackErr != nil {
			return fmt.Errorf("%v; rollback failed: %w", cause, rollbackErr)
		}
		return cause
	}
	for index, move := range journal.Moves {
		if err := ensureDirectory(filepath.Dir(move.Destination), 0o755); err != nil {
			return fail(err)
		}
		if err := os.Rename(move.Source, move.Destination); err != nil {
			return fail(err)
		}
		if err := syncDirectory(filepath.Dir(move.Source)); err != nil {
			return fail(err)
		}
		if err := syncDirectory(filepath.Dir(move.Destination)); err != nil {
			return fail(err)
		}
		if err := checkpoint(fmt.Sprintf("move:%d", index)); err != nil {
			return err
		}
	}
	if err := saveDatabase(paths.Database, db); err != nil {
		return fail(err)
	}
	if err := checkpoint("database"); err != nil {
		return err
	}
	if journal.SettingsChanged {
		if err := atomicWriteFile(paths.Settings, journal.SettingsContent, 0o644); err != nil {
			return fail(err)
		}
		if err := checkpoint("settings"); err != nil {
			return err
		}
	}
	if journal.ProfileChanged {
		path, _ := profilePath(paths, journal.ProfileName)
		if err := atomicWriteFile(path, journal.ProfileContent, 0o644); err != nil {
			return fail(err)
		}
		if err := checkpoint("profile"); err != nil {
			return err
		}
	}
	if journal.CurrentProfileChanged {
		if err := atomicWriteFile(paths.CurrentProfile, journal.CurrentProfileContent, 0o600); err != nil {
			return fail(err)
		}
		if err := checkpoint("current_profile"); err != nil {
			return err
		}
	}
	for index, change := range journal.Files {
		if err := applyTransactionFileChange(change); err != nil {
			return fail(err)
		}
		if err := checkpoint(fmt.Sprintf("file:%d", index)); err != nil {
			return err
		}
	}
	for index, link := range journal.Links {
		path := filepath.Join(paths.Bin, link.Name)
		if link.NewTarget == "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fail(err)
			}
			if err := syncDirectory(paths.Bin); err != nil {
				return fail(err)
			}
		} else if err := atomicSymlink(link.NewTarget, path); err != nil {
			return fail(err)
		}
		if err := checkpoint(fmt.Sprintf("link:%d", index)); err != nil {
			return err
		}
	}
	journal.State = "committed"
	if err := atomicWriteJSON(paths.Transaction, journal); err != nil {
		return fail(err)
	}
	if err := checkpoint("committed_journal"); err != nil {
		return err
	}
	if err := os.RemoveAll(journal.StageRoot); err != nil {
		return err
	}
	if err := syncDirectory(paths.Staging); err != nil {
		return err
	}
	if err := checkpoint("stage_cleanup"); err != nil {
		return err
	}
	if err := os.Remove(paths.Transaction); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncDirectory(paths.Set); err != nil {
		return err
	}
	return checkpoint("transaction_cleanup")
}

func rollbackTransaction(paths AppPaths, journal transactionJournal) error {
	return rollbackTransactionWithSync(paths, journal, syncDirectory)
}

func rollbackTransactionWithSync(paths AppPaths, journal transactionJournal, syncDir func(string) error) error {
	var first error
	recordSyncError := func(path string) {
		if err := syncDir(path); err != nil && first == nil {
			first = err
		}
	}
	for i := len(journal.Files) - 1; i >= 0; i-- {
		change := journal.Files[i]
		if change.Existed {
			if err := atomicWriteFile(change.Path, change.Backup, os.FileMode(change.Mode)); err != nil && first == nil {
				first = err
			}
		} else {
			if err := os.Remove(change.Path); err != nil && !os.IsNotExist(err) {
				if first == nil {
					first = err
				}
			} else {
				recordSyncError(filepath.Dir(change.Path))
			}
		}
	}
	for i := len(journal.Links) - 1; i >= 0; i-- {
		link := journal.Links[i]
		path := filepath.Join(paths.Bin, link.Name)
		if link.OldTarget == "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				if first == nil {
					first = err
				}
			} else {
				recordSyncError(paths.Bin)
			}
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
					if err := os.RemoveAll(move.Destination); err != nil {
						if first == nil {
							first = err
						}
					} else {
						recordSyncError(filepath.Dir(move.Destination))
					}
				}
			} else if sourceErr != nil && first == nil {
				first = sourceErr
			}
		case "remove":
			if _, err := os.Lstat(move.Destination); err == nil {
				if e := ensureDirectory(filepath.Dir(move.Source), 0o755); e != nil {
					if first == nil {
						first = e
					}
				} else if e := os.Rename(move.Destination, move.Source); e != nil {
					if first == nil {
						first = e
					}
				} else {
					recordSyncError(filepath.Dir(move.Destination))
					recordSyncError(filepath.Dir(move.Source))
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
	} else if err := removeFileAndSync(paths.Database, syncDir); err != nil {
		if first == nil {
			first = err
		}
	}
	if journal.SettingsChanged {
		if journal.SettingsExisted {
			if err := atomicWriteFile(paths.Settings, journal.SettingsBackup, 0o644); err != nil && first == nil {
				first = err
			}
		} else if err := removeFileAndSync(paths.Settings, syncDir); err != nil {
			if first == nil {
				first = err
			}
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
			} else if err := removeFileAndSync(path, syncDir); err != nil {
				if first == nil {
					first = err
				}
			}
		}
	}
	if journal.CurrentProfileChanged {
		if journal.CurrentProfileExisted {
			if err := atomicWriteFile(paths.CurrentProfile, journal.CurrentProfileBackup, 0o600); err != nil && first == nil {
				first = err
			}
		} else if err := removeFileAndSync(paths.CurrentProfile, syncDir); err != nil {
			if first == nil {
				first = err
			}
		}
	}
	if first == nil {
		if err := os.RemoveAll(journal.StageRoot); err != nil {
			first = err
		} else {
			recordSyncError(paths.Staging)
		}
	}
	if first == nil {
		if err := removeFileAndSync(paths.Transaction, syncDir); err != nil {
			first = err
		}
	}
	return first
}

func removeFileAndSync(path string, syncDir func(string) error) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return syncDir(filepath.Dir(path))
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
		if filepath.IsAbs(target) && managedCommandTarget(paths, target) {
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
			owned := filepath.IsAbs(current) && managedCommandTarget(paths, current)
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
			if !filepath.IsAbs(oldTarget) || !managedCommandTarget(paths, oldTarget) {
				continue
			}
			if target := desired[entry.Name()]; target != "" {
				if err := ensureDirectory(paths.Bin, 0o755); err != nil {
					return err
				}
				current, readErr := readLink(filepath.Join(paths.Bin, entry.Name()))
				if readErr != nil {
					return readErr
				}
				if current != "" && current != target {
					owned := filepath.IsAbs(current) && managedCommandTarget(paths, current)
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
			if err := syncDirectory(paths.LegacyBin); err != nil {
				return err
			}
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
	if err := ensureDirectory(paths.Bin, 0o755); err != nil {
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
		if !filepath.IsAbs(current) || !managedCommandTarget(paths, current) {
			if wanted != "" {
				return appError("binary_conflict", entry.Name())
			}
			continue
		}
		if wanted == "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := syncDirectory(paths.Bin); err != nil {
				return err
			}
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
			if settings.GenerationID != "" {
				target, err = profileLauncherPath(paths, settings.ProfileName, settings.GenerationID, binaryName)
				if err != nil {
					return nil, err
				}
			}
			if previous, ok := result[binaryName]; ok && previous != target {
				return nil, appError("binary_conflict", binaryName)
			}
			result[binaryName] = target
		}
	}
	return result, nil
}

func managedCommandTarget(paths AppPaths, target string) bool {
	if pathWithin(paths.Store, target) {
		return true
	}
	rel, err := filepath.Rel(paths.Profiles, target)
	if err != nil || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 4 || parts[1] != "launchers" {
		return false
	}
	returnTarget, pathErr := profileLauncherPath(paths, parts[0]+".toml", parts[2], parts[3])
	return pathErr == nil && returnTarget == target
}

func applyTransactionFileChange(change transactionFileChange) error {
	if change.Delete {
		if err := os.Remove(change.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return syncDirectory(filepath.Dir(change.Path))
	}
	return atomicWriteFile(change.Path, change.Content, os.FileMode(change.Mode))
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
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if !info.Mode().IsRegular() {
		return false, nil, appError("invalid_database", "transaction database path must be a regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, nil, err
	}
	return true, raw, nil
}
