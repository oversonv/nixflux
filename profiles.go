package nixnet

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func installProfile(paths AppPaths, name string) error {
	if err := ensureConfiguration(paths); err != nil {
		return err
	}
	filename, err := profileFilename(name)
	if err != nil {
		return err
	}
	path, err := profilePath(paths, filename)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return appError("profile_exists", name)
	} else if !os.IsNotExist(err) {
		return wrapAppError("io_error", "inspect profile path", err)
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	newDB := cloneDatabase(db)
	newDB.ProfileClosures[filename] = map[string]string{}
	newDB.ProfileFrozen[filename] = map[string]string{}
	settings := Settings{Enabled: []string{}, ProfileName: filename, ProfilePath: path}
	generation, err := newProfileGeneration(filename, settings, newDB)
	if err != nil {
		return err
	}
	profileChange, err := captureTransactionFileChange(path, []byte(encodeProfileTOML(nil)), 0o644, false)
	if err != nil {
		return err
	}
	manifestChange, err := generationManifestChange(paths, generation)
	if err != nil {
		return err
	}
	pointerChange, err := generationPointerChange(paths, filename, generation.ID)
	if err != nil {
		return err
	}
	if err := ensureDirectory(paths.Staging, 0o755); err != nil {
		return wrapAppError("io_error", "prepare profile creation transaction", err)
	}
	stageRoot, err := os.MkdirTemp(paths.Staging, "txn-profile-create-")
	if err != nil {
		return wrapAppError("io_error", "prepare profile creation transaction", err)
	}
	existed, backup, err := databaseBackup(paths.Database)
	if err != nil {
		_ = os.RemoveAll(stageRoot)
		return wrapAppError("io_error", "save profile creation recovery data", err)
	}
	journal := transactionJournal{StageRoot: stageRoot, DatabaseExisted: existed, DatabaseBackup: backup, Files: []transactionFileChange{profileChange, manifestChange, pointerChange}}
	if err := applyTransaction(paths, journal, newDB); err != nil {
		return wrapAppError("transaction_failed", "create profile", err)
	}
	return nil
}

func listProfiles(paths AppPaths) ([]string, string, error) {
	if err := ensureConfiguration(paths); err != nil {
		return nil, "", err
	}
	filenames, err := listProfileFilenames(paths)
	if err != nil {
		return nil, "", appError("io_error", err.Error())
	}
	defaultProfile, err := readProfileLock(paths)
	if err != nil {
		return nil, "", err
	}
	return filenames, defaultProfile, nil
}

func listProfileGenerations(paths AppPaths, name string) ([]ProfileGeneration, string, error) {
	filename, err := profileFilename(name)
	if err != nil {
		return nil, "", err
	}
	profileFile, err := profilePath(paths, filename)
	if err != nil {
		return nil, "", err
	}
	profileInfo, err := os.Lstat(profileFile)
	if os.IsNotExist(err) {
		return nil, "", appError("profile_not_found", name)
	}
	if err != nil {
		return nil, "", wrapAppError("io_error", "inspect profile", err)
	}
	if !profileInfo.Mode().IsRegular() {
		return nil, "", appError("invalid_config", "profile must be a regular file")
	}
	dir, err := generationDirectory(paths, filename)
	if err != nil {
		return nil, "", err
	}
	current, err := readCurrentGeneration(paths, filename)
	if err != nil {
		return nil, "", err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []ProfileGeneration{}, current, nil
	}
	if err != nil {
		return nil, "", wrapAppError("io_error", "list profile generations", err)
	}
	generations := make([]ProfileGeneration, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			return nil, "", appError("invalid_database", "generation directory contains an unsupported entry")
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, "", appError("invalid_database", "generation directory contains a non-regular manifest")
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		generation, err := readProfileGeneration(paths, filename, id)
		if err != nil {
			return nil, "", err
		}
		generations = append(generations, generation)
	}
	if current != "" {
		found := false
		for _, generation := range generations {
			if generation.ID == current {
				found = true
				break
			}
		}
		if !found {
			return nil, "", appError("invalid_database", "current generation pointer references a missing manifest")
		}
	}
	sort.Slice(generations, func(i, j int) bool {
		if generations[i].CreatedAt.Equal(generations[j].CreatedAt) {
			return generations[i].ID < generations[j].ID
		}
		return generations[i].CreatedAt.Before(generations[j].CreatedAt)
	})
	return generations, current, nil
}

func rollbackProfile(paths AppPaths, name, id string) error {
	return rollbackProfileWithPlan(paths, name, id, nil)
}

func rollbackProfileWithPlan(paths AppPaths, name, id string, onPlan func([]PackageTransactionItem)) error {
	if err := ensureConfiguration(paths); err != nil {
		return err
	}
	filename, err := profileFilename(name)
	if err != nil {
		return err
	}
	generation, err := readProfileGeneration(paths, filename, id)
	if err != nil {
		return err
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	if err := validateProfileGeneration(paths, generation, db); err != nil {
		return err
	}
	settings, err := loadNamedSettings(paths, name)
	if err != nil {
		return err
	}
	active, err := loadSettings(paths)
	if err != nil {
		return err
	}
	plan := newPackagePlan()
	plan.Resolved = make(map[string]InstalledPackage, len(generation.Closure))
	plan.Frozen = generation.Frozen
	plan.ActivateGeneration = generation.ID
	for name, artifactID := range generation.Closure {
		pkg, exists := db.Artifacts[artifactID]
		if !exists {
			return appError("invalid_database", "generation references a missing Store artifact: "+artifactID)
		}
		pkg.Frozen = versionsEqual(generation.Frozen[name], pkg.Version)
		plan.Resolved[normalizePackageName(name)] = pkg
	}
	newSettings := cloneSettings(settings)
	newSettings.Enabled = append([]string{}, generation.Enabled...)
	newActive := active
	if active.ProfileName == filename {
		newActive = cloneSettings(newSettings)
	}
	if onPlan != nil {
		current := packagesForProfile(db, filename)
		changes := packagePlanChanges(paths, current, plan)
		for key, pkg := range current {
			if _, retained := plan.Resolved[key]; !retained {
				changes = append(changes, PackageTransactionItem{Package: pkg, Action: "Remove"})
			}
		}
		onPlan(changes)
	}
	return commitPackagePlan(paths, db, plan, settings, newSettings, active, newActive, false)
}

func renderProfileGenerations(paths AppPaths, name string) error {
	generations, current, err := listProfileGenerations(paths, name)
	if err != nil {
		return err
	}
	if len(generations) == 0 {
		fmt.Println("No generations.")
		return nil
	}
	for _, generation := range generations {
		marker := ""
		if generation.ID == current {
			marker = " " + uvAccent("(current)")
		}
		fmt.Printf("%s  %s  %s %s%s\n", uvEmphasis(generation.ID), uvDim(generation.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")), uvEmphasis(fmt.Sprintf("%d", len(generation.Enabled))), uvDim("roots"), marker)
	}
	return nil
}

func removeProfile(paths AppPaths, name string) error {
	if err := ensureConfiguration(paths); err != nil {
		return err
	}
	filename, err := profileFilename(name)
	if err != nil {
		return err
	}
	path, err := profilePath(paths, filename)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return appError("profile_not_found", name)
	}
	if err != nil {
		return appError("io_error", err.Error())
	}
	if !info.Mode().IsRegular() {
		return appError("invalid_config", "profile is not a regular file")
	}
	profiles, err := listProfileFilenames(paths)
	if err != nil {
		return appError("io_error", err.Error())
	}
	remaining := make([]string, 0, len(profiles)-1)
	for _, candidate := range profiles {
		if candidate != filename {
			remaining = append(remaining, candidate)
		}
	}
	defaultProfile, err := readProfileLock(paths)
	if err != nil {
		return err
	}
	currentProfile, err := readCurrentProfile(paths)
	if err != nil {
		return err
	}
	newDefault := defaultProfile
	if defaultProfile == filename {
		newDefault = ""
		if len(remaining) > 0 {
			newDefault = newestProfile(paths, remaining)
		}
	}
	newCurrent := currentProfile
	if currentProfile == filename {
		newCurrent = newDefault
		if newCurrent == "" && len(remaining) > 0 {
			newCurrent = newestProfile(paths, remaining)
		}
	}

	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	active, err := loadProfileSettings(paths, currentProfile)
	if err != nil {
		return err
	}
	var next Settings
	if newCurrent != "" {
		next, err = loadProfileSettings(paths, newCurrent)
		if err != nil {
			return err
		}
	} else {
		channels, _, channelErr := readChannels(paths.Channels)
		if channelErr != nil {
			return channelErr
		}
		next = Settings{Channels: channels, Enabled: []string{}}
	}
	if currentProfile == filename && next.ProfileName != "" {
		if err := ensureProfileClosure(paths, &db, next); err != nil {
			return err
		}
	}

	newDB := cloneDatabase(db)
	delete(newDB.ProfileClosures, filename)
	delete(newDB.ProfileFrozen, filename)
	links, err := buildLinkChanges(paths, db, newDB, active, next)
	if err != nil {
		return err
	}
	if err := ensureDirectory(paths.Staging, 0o755); err != nil {
		return wrapAppError("io_error", "prepare profile removal transaction", err)
	}
	stageRoot, err := os.MkdirTemp(paths.Staging, "txn-profile-remove-")
	if err != nil {
		return wrapAppError("io_error", "prepare profile removal transaction", err)
	}
	cleanupStage := func() { _ = os.RemoveAll(stageRoot) }
	files := make([]transactionFileChange, 0, 5)
	profileChange, err := captureTransactionFileChange(path, nil, 0o644, true)
	if err != nil {
		cleanupStage()
		return err
	}
	files = append(files, profileChange)

	generationDir, err := generationDirectory(paths, filename)
	if err != nil {
		cleanupStage()
		return err
	}
	if entries, readErr := os.ReadDir(generationDir); readErr == nil {
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") {
				cleanupStage()
				return appError("invalid_database", "profile generation directory contains an unsupported entry")
			}
			change, changeErr := captureTransactionFileChange(filepath.Join(generationDir, entry.Name()), nil, 0o644, true)
			if changeErr != nil {
				cleanupStage()
				return changeErr
			}
			files = append(files, change)
		}
	} else if !os.IsNotExist(readErr) {
		cleanupStage()
		return wrapAppError("io_error", "list profile generation files", readErr)
	}
	if pointer, pointerErr := generationPointerPath(paths, filename); pointerErr == nil {
		change, changeErr := captureTransactionFileChange(pointer, nil, 0o600, true)
		if changeErr != nil {
			cleanupStage()
			return changeErr
		}
		files = append(files, change)
	} else {
		cleanupStage()
		return pointerErr
	}
	if newDefault == "" {
		change, changeErr := captureTransactionFileChange(paths.ProfilesLock, nil, 0o600, true)
		if changeErr != nil {
			cleanupStage()
			return changeErr
		}
		files = append(files, change)
	} else {
		change, changeErr := captureTransactionFileChange(paths.ProfilesLock, []byte(newDefault+"\n"), 0o600, false)
		if changeErr != nil {
			cleanupStage()
			return changeErr
		}
		files = append(files, change)
	}
	if newCurrent == "" {
		change, changeErr := captureTransactionFileChange(paths.CurrentProfile, nil, 0o600, true)
		if changeErr != nil {
			cleanupStage()
			return changeErr
		}
		files = append(files, change)
	} else if currentProfile == filename {
		change, changeErr := captureTransactionFileChange(paths.CurrentProfile, []byte(newCurrent+"\n"), 0o600, false)
		if changeErr != nil {
			cleanupStage()
			return changeErr
		}
		files = append(files, change)
	}
	exists, backup, err := databaseBackup(paths.Database)
	if err != nil {
		cleanupStage()
		return wrapAppError("io_error", "save profile removal recovery data", err)
	}
	journal := transactionJournal{StageRoot: stageRoot, DatabaseExisted: exists, DatabaseBackup: backup, Links: links, Files: files}
	if err := applyTransaction(paths, journal, newDB); err != nil {
		return wrapAppError("transaction_failed", "remove profile", err)
	}
	_ = os.Remove(generationDir)
	_ = os.Remove(filepath.Dir(generationDir))
	syncDirectory(paths.Profiles)
	return nil
}

func renderProfiles(paths AppPaths) error {
	profiles, defaultProfile, err := listProfiles(paths)
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		fmt.Println(uvDim("No profiles."))
		return nil
	}
	for _, filename := range profiles {
		name := strings.TrimSuffix(filename, ".toml")
		if filename == defaultProfile {
			fmt.Printf("%s %s\n", uvEmphasis(name), uvAccent("(default)"))
		} else {
			fmt.Println(uvEmphasis(name))
		}
	}
	return nil
}
