package nixflux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type packagePlan struct {
	Artifacts map[string]PackageArtifact
	Resolved  map[string]InstalledPackage
	Resolving map[string]bool
	Replace   map[string]bool
	Indexes   map[string]RepositoryIndex
}

func newPackagePlan() *packagePlan {
	return &packagePlan{Artifacts: map[string]PackageArtifact{}, Resolved: map[string]InstalledPackage{}, Resolving: map[string]bool{}, Replace: map[string]bool{}, Indexes: map[string]RepositoryIndex{}}
}

func clonePackagePlan(source *packagePlan) *packagePlan {
	clone := newPackagePlan()
	clone.Indexes = source.Indexes
	for key, value := range source.Artifacts {
		clone.Artifacts[key] = value
	}
	for key, value := range source.Resolved {
		clone.Resolved[key] = value
	}
	for key, value := range source.Resolving {
		clone.Resolving[key] = value
	}
	for key, value := range source.Replace {
		clone.Replace[key] = value
	}
	return clone
}

func commitResolvedPlan(destination, source *packagePlan) {
	destination.Artifacts = source.Artifacts
	destination.Resolved = source.Resolved
	destination.Resolving = source.Resolving
	destination.Replace = source.Replace
	destination.Indexes = source.Indexes
}

func resolvePackage(paths AppPaths, settings Settings, db Database, plan *packagePlan, repo, name, version string, allowDowngrade bool) error {
	key := normalizePackageName(name)
	if existing, ok := plan.Artifacts[key]; ok {
		cmp, compareErr := compareVersions(existing.Manifest.Version, version)
		if compareErr != nil || cmp != 0 {
			return appError("dependency_conflict", name+" requires incompatible versions")
		}
		return nil
	}
	if plan.Resolving[key] {
		return appError("dependency_cycle", name)
	}
	channel, ok := repositoryChannel(settings, repo)
	if !ok {
		return appError("channel_not_found", repo)
	}
	index, err := loadRepositoryIndexCached(channel, plan.Indexes)
	if err != nil {
		return err
	}
	var entry RepositoryVersion
	var resolvedVersion string
	var found bool
	if strings.EqualFold(version, "latest") {
		entry, resolvedVersion, found = latestRepositoryVersion(index.Packages[key])
	} else {
		entry, resolvedVersion, found = lookupRepositoryVersion(index.Packages[key], version)
	}
	if !found {
		return appError("version_not_found", name+" "+version)
	}
	version = resolvedVersion

	installed, installedExists := db.Packages[key]
	useInstalled := false
	if installedExists {
		if versionsEqual(installed.Version, version) {
			if verifyStoredPackage(paths, installed) == nil {
				useInstalled = true
			} else {
				plan.Replace[key] = true
			}
		}
		if installed.Frozen && !versionsEqual(installed.Version, version) {
			return appError("package_frozen", installed.Name+"=="+installed.Version)
		}
		cmp, compareErr := compareVersions(version, installed.Version)
		if compareErr != nil {
			return appError("invalid_version", compareErr.Error())
		}
		if cmp < 0 && !allowDowngrade {
			return appError("downgrade_requires_set_update", installed.Name+" "+installed.Version+" -> "+version)
		}
	}

	plan.Resolving[key] = true
	var artifact *PackageArtifact
	var cachedArtifact *InstalledPackage
	dependencies := []string{}
	if useInstalled {
		dependencies = append(dependencies, installed.Dependencies...)
	} else {
		for _, candidate := range db.Artifacts {
			if normalizePackageName(candidate.Name) != key || !versionsEqual(candidate.Version, version) || !strings.EqualFold(candidate.Hash, entry.SHA256) {
				continue
			}
			if verifyStoredPackage(paths, candidate) == nil {
				manifestRaw, readErr := os.ReadFile(filepath.Join(paths.Store, candidate.Dir, "package.toml"))
				manifest, parseErr := parsePackageTOML(manifestRaw)
				if readErr != nil || parseErr != nil || entry.Architecture != "" && !strings.EqualFold(entry.Architecture, manifest.Architecture) {
					continue
				}
				copy := candidate
				cachedArtifact = &copy
				break
			}
		}
		if cachedArtifact != nil {
			dependencies = append(dependencies, cachedArtifact.Dependencies...)
		} else {
			resolved, downloadErr := downloadRepositoryPackage(channel.URL, name, version, entry, repo)
			if downloadErr != nil {
				delete(plan.Resolving, key)
				return downloadErr
			}
			if entry.Architecture != "" && !strings.EqualFold(entry.Architecture, resolved.Manifest.Architecture) {
				delete(plan.Resolving, key)
				return appError("invalid_package", "DEB Architecture does not match the APT Packages index")
			}
			artifact = &resolved
			dependencies = append(dependencies, resolved.Manifest.Dependencies...)
		}
	}

	for _, dependency := range dependencies {
		if dependencyErr := resolveDependencyGroup(paths, settings, db, plan, dependency, repo, allowDowngrade); dependencyErr != nil {
			delete(plan.Resolving, key)
			return appError("dependency_not_found", name+" requires "+dependency+": "+dependencyErr.Error())
		}
	}
	delete(plan.Resolving, key)
	if artifact != nil {
		plan.Artifacts[key] = *artifact
		plan.Resolved[key] = installedPackageFromArtifact(*artifact)
	} else if cachedArtifact != nil {
		plan.Resolved[key] = *cachedArtifact
	} else if useInstalled {
		plan.Resolved[key] = installed
	}
	return nil
}

func installedPackageFromArtifact(artifact PackageArtifact) InstalledPackage {
	return InstalledPackage{
		Name: artifact.Manifest.Name, Version: artifact.Manifest.Version,
		Hash: artifact.Hash, ManifestHash: artifact.ManifestHash,
		ChecksumsHash: artifact.ChecksumsHash,
		Dir:           packageDirectory(artifact.Hash, artifact.Manifest.Name, artifact.Manifest.Version),
		Source:        artifact.Source, Dependencies: append([]string{}, artifact.Manifest.Dependencies...),
		Files: append([]string{}, artifact.Files...), Entrypoints: append([]string{}, artifact.Manifest.Entrypoints...),
		Modes: cloneFileModes(artifact.Modes),
	}
}

func resolveDependencyGroup(paths AppPaths, settings Settings, db Database, plan *packagePlan, dependency, preferredRepo string, allowDowngrade bool) error {
	alternatives, err := parseDependencyGroup(dependency)
	if err != nil {
		return err
	}
	var lastErr error
	for _, alternative := range alternatives {
		name, constraint := alternative[0], alternative[1]
		key := normalizePackageName(name)
		if planned, exists := plan.Artifacts[key]; exists {
			if satisfied, checkErr := versionSatisfies(planned.Manifest.Version, constraint); checkErr == nil && satisfied {
				return nil
			}
		}
		if installed, exists := db.Packages[key]; exists {
			if satisfied, checkErr := versionSatisfies(installed.Version, constraint); checkErr == nil && satisfied && verifyStoredPackage(paths, installed) == nil {
				candidatePlan := clonePackagePlan(plan)
				if closureErr := ensureInstalledDependencies(paths, settings, db, candidatePlan, installed, installed.Source, allowDowngrade); closureErr == nil {
					commitResolvedPlan(plan, candidatePlan)
					return nil
				} else {
					lastErr = closureErr
				}
			}
		}
		if systemVersion, exists := systemPackageVersion(name); exists {
			if satisfied, checkErr := versionSatisfies(systemVersion, constraint); checkErr == nil && satisfied {
				return nil
			}
		}
	}

	for _, alternative := range alternatives {
		name, constraint := alternative[0], alternative[1]
		repo, _, resolvedVersion, _, selectErr := selectRepositoryVersionCached(settings, name, constraint, preferredRepo, plan.Indexes)
		if selectErr != nil {
			lastErr = selectErr
			continue
		}
		candidatePlan := clonePackagePlan(plan)
		if resolveErr := resolvePackage(paths, settings, db, candidatePlan, repo, name, resolvedVersion, allowDowngrade); resolveErr != nil {
			lastErr = resolveErr
			continue
		}
		commitResolvedPlan(plan, candidatePlan)
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return appError("dependency_not_found", dependency)
}

func ensureInstalledDependencies(paths AppPaths, settings Settings, db Database, plan *packagePlan, installed InstalledPackage, preferredRepo string, allowDowngrade bool) error {
	key := normalizePackageName(installed.Name)
	if plan.Resolving[key] {
		return appError("dependency_cycle", installed.Name)
	}
	plan.Resolving[key] = true
	defer delete(plan.Resolving, key)
	for _, dependency := range installed.Dependencies {
		if err := resolveDependencyGroup(paths, settings, db, plan, dependency, preferredRepo, allowDowngrade); err != nil {
			return fmt.Errorf("%s requires %s: %w", installed.Name, dependency, err)
		}
	}
	return nil
}

func commitPackagePlan(paths AppPaths, db Database, plan *packagePlan, oldSettings, newSettings, oldActive, newActive Settings, switchLoaded bool) error {
	if err := normalizeSettings(&newSettings); err != nil {
		return err
	}
	profileChanged := encodeProfileTOML(oldSettings.Enabled) != encodeProfileTOML(newSettings.Enabled)
	loadedChanged := switchLoaded && oldActive.ProfileName != newActive.ProfileName
	activeChanged := encodeProfileTOML(oldActive.Enabled) != encodeProfileTOML(newActive.Enabled) || oldActive.ProfileName != newActive.ProfileName
	closureChanged := db.SchemaVersion < databaseSchemaVersion
	if plan.Resolved != nil && newSettings.ProfileName != "" {
		oldClosure := db.ProfileClosures[newSettings.ProfileName]
		if len(oldClosure) != len(plan.Resolved) {
			closureChanged = true
		} else {
			for name, pkg := range plan.Resolved {
				if oldClosure[name] != pkg.Dir {
					closureChanged = true
					break
				}
			}
		}
	}
	if len(plan.Artifacts) == 0 && !profileChanged && !closureChanged && !loadedChanged && !activeChanged {
		return nil
	}
	if err := os.MkdirAll(paths.Store, 0o755); err != nil {
		return appError("io_error", err.Error())
	}
	if err := os.MkdirAll(paths.Bin, 0o755); err != nil {
		return appError("io_error", err.Error())
	}
	if err := os.MkdirAll(paths.Staging, 0o755); err != nil {
		return appError("io_error", err.Error())
	}
	stageRoot, err := os.MkdirTemp(paths.Staging, "txn-")
	if err != nil {
		return appError("io_error", err.Error())
	}
	oldDB := db
	newDB := cloneDatabase(db)
	keys := make([]string, 0, len(plan.Artifacts))
	for k := range plan.Artifacts {
		keys = append(keys, k)
	}
	sortStrings(keys)
	moves := []transactionMove{}
	for _, key := range keys {
		a := plan.Artifacts[key]
		dir := packageDirectory(a.Hash, a.Manifest.Name, a.Manifest.Version)
		final := filepath.Join(paths.Store, dir)
		needsExtract := plan.Replace[key]
		if _, statErr := os.Lstat(final); statErr == nil {
			checkRecord := InstalledPackage{Name: a.Manifest.Name, Version: a.Manifest.Version, Dir: dir, Files: a.Files, Dependencies: a.Manifest.Dependencies, Entrypoints: a.Manifest.Entrypoints, Modes: a.Modes}
			if verifyStoredPackage(paths, checkRecord) != nil {
				needsExtract = true
			}
		} else if os.IsNotExist(statErr) {
			needsExtract = true
		} else {
			_ = os.RemoveAll(stageRoot)
			return appError("io_error", statErr.Error())
		}
		if needsExtract {
			if _, statErr := os.Lstat(final); statErr == nil {
				backup := filepath.Join(stageRoot, "old", "corrupt-"+dir)
				moves = append(moves, transactionMove{Kind: "remove", Source: final, Destination: backup})
			}
			stage := filepath.Join(stageRoot, "new", dir)
			if _, err := extractPackageArchive(a.Raw, stage, a.Manifest); err != nil {
				_ = os.RemoveAll(stageRoot)
				return appError("invalid_package", err.Error())
			}
			moves = append(moves, transactionMove{Kind: "install", Source: stage, Destination: final})
		}
		pkg := installedPackageFromArtifact(a)
		newDB.Artifacts[dir] = pkg
	}
	if plan.Resolved != nil && newSettings.ProfileName != "" {
		setProfileClosure(&newDB, newSettings.ProfileName, plan.Resolved)
	}
	profileDB := Database{Packages: packagesForProfile(newDB, newSettings.ProfileName)}
	if err := validateInstalledDependencies(profileDB); err != nil {
		_ = os.RemoveAll(stageRoot)
		return err
	}
	links, err := buildLinkChanges(paths, oldDB, newDB, oldActive, newActive)
	if err != nil {
		_ = os.RemoveAll(stageRoot)
		return err
	}
	exists, backup, err := databaseBackup(paths.Database)
	if err != nil {
		_ = os.RemoveAll(stageRoot)
		return appError("io_error", err.Error())
	}
	journal := transactionJournal{StageRoot: stageRoot, DatabaseExisted: exists, DatabaseBackup: backup, Moves: moves, Links: links}
	if profileChanged {
		profileExisted, profileBackup, profileErr := databaseBackup(newSettings.ProfilePath)
		if profileErr != nil {
			_ = os.RemoveAll(stageRoot)
			return appError("io_error", profileErr.Error())
		}
		journal.ProfileChanged = true
		journal.ProfileName = newSettings.ProfileName
		journal.ProfileExisted = profileExisted
		journal.ProfileBackup = profileBackup
		journal.ProfileContent = []byte(encodeProfileTOML(newSettings.Enabled))
	}
	if loadedChanged {
		currentExisted, currentBackup, currentErr := databaseBackup(paths.CurrentProfile)
		if currentErr != nil {
			_ = os.RemoveAll(stageRoot)
			return appError("io_error", currentErr.Error())
		}
		journal.CurrentProfileChanged = true
		journal.CurrentProfileExisted = currentExisted
		journal.CurrentProfileBackup = currentBackup
		journal.CurrentProfileContent = []byte(newActive.ProfileName + "\n")
	}
	if err := applyTransaction(paths, journal, newDB); err != nil {
		return appError("transaction_failed", err.Error())
	}
	return nil
}

func validateInstalledDependencies(db Database) error {
	for _, pkg := range db.Packages {
		for _, dep := range pkg.Dependencies {
			alternatives, err := parseDependencyGroup(dep)
			if err != nil {
				return err
			}
			if !dependencyGroupSatisfiedInDatabase(db, alternatives) {
				return appError("dependency_conflict", pkg.Name+" requires "+dep)
			}
		}
	}
	return nil
}

func dependencySatisfied(paths AppPaths, db Database, plan *packagePlan, alternatives [][2]string) bool {
	for _, alternative := range alternatives {
		name, constraint := alternative[0], alternative[1]
		key := normalizePackageName(name)
		if planned, ok := plan.Artifacts[key]; ok {
			if satisfies, err := versionSatisfies(planned.Manifest.Version, constraint); err == nil && satisfies {
				return true
			}
		}
		if installed, ok := db.Packages[key]; ok {
			if satisfies, err := versionSatisfies(installed.Version, constraint); err == nil && satisfies {
				if verifyStoredPackage(paths, installed) == nil {
					return true
				}
			}
		}
		if version, ok := systemPackageVersion(name); ok {
			if satisfies, err := versionSatisfies(version, constraint); err == nil && satisfies {
				return true
			}
		}
	}
	return false
}

func dependencyGroupSatisfiedInDatabase(db Database, alternatives [][2]string) bool {
	for _, alternative := range alternatives {
		name, constraint := alternative[0], alternative[1]
		if installed, ok := db.Packages[normalizePackageName(name)]; ok {
			if satisfies, err := versionSatisfies(installed.Version, constraint); err == nil && satisfies {
				return true
			}
		}
		if version, ok := systemPackageVersion(name); ok {
			if satisfies, err := versionSatisfies(version, constraint); err == nil && satisfies {
				return true
			}
		}
	}
	return false
}

func systemPackageVersion(name string) (string, bool) {
	prefix := strings.TrimSpace(os.Getenv("PREFIX"))
	if prefix == "" && runtimeGOOS == "android" {
		prefix = "/data/data/com.termux/files/usr"
	}
	if prefix == "" {
		return "", false
	}
	statusPath := filepath.Join(prefix, "var", "lib", "dpkg", "status")
	raw, err := os.ReadFile(statusPath)
	if err != nil {
		return "", false
	}
	for _, paragraph := range splitControlParagraphs(string(raw)) {
		fields, parseErr := parseControlFieldsStrict(paragraph)
		if parseErr != nil || !strings.EqualFold(fields["package"], name) {
			continue
		}
		status := strings.Fields(fields["status"])
		if len(status) != 3 || status[1] != "ok" || status[2] != "installed" || fields["version"] == "" {
			return "", false
		}
		architecture := strings.TrimSpace(fields["architecture"])
		if architecture != "all" && !strings.EqualFold(architecture, runtimeDebianArchitecture()) {
			return "", false
		}
		if !parseDebianVersion(fields["version"]).valid {
			return "", false
		}
		return fields["version"], true
	}
	return "", false
}

func envInstall(paths AppPaths, spec string) error {
	repo, name, version, err := parseRepoPackageSpec(spec)
	if err != nil {
		return err
	}
	if version == "" {
		return appError("invalid_env_install_format", spec+"; version is required")
	}
	settings, err := loadDefaultSettings(paths)
	if err != nil {
		return err
	}
	active, err := loadSettings(paths)
	if err != nil {
		return err
	}
	newSettings := cloneSettings(settings)
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	ensureProfileClosure(paths, &db, settings)
	profileDB := databaseForProfile(db, settings)
	plan := newPackagePlan()
	if err := resolvePackage(paths, settings, profileDB, plan, repo, name, version, false); err != nil {
		return err
	}
	resolvedVersion := version
	if resolved, ok := plan.Resolved[normalizePackageName(name)]; ok {
		resolvedVersion = resolved.Version
	} else if installed, ok := profileDB.Packages[normalizePackageName(name)]; ok {
		resolvedVersion = installed.Version
	}
	if strings.EqualFold(resolvedVersion, "latest") || !isValidVersion(resolvedVersion) {
		return appError("invalid_version", "resolver did not produce a concrete package version")
	}
	if err := setEnabledPackage(&newSettings, name, resolvedVersion); err != nil {
		return err
	}
	for _, target := range newSettings.Enabled {
		rootName, rootVersion, parseErr := parsePackageVersion(target)
		if parseErr != nil {
			return parseErr
		}
		if normalizePackageName(rootName) == normalizePackageName(name) {
			continue
		}
		preferredRepo := ""
		if installed, exists := profileDB.Packages[normalizePackageName(rootName)]; exists {
			preferredRepo = installed.Source
		}
		rootRepo, _, selectedVersion, _, selectErr := findRepositoryVersionCached(settings, rootName, rootVersion, preferredRepo, plan.Indexes)
		if selectErr != nil {
			return selectErr
		}
		if err := resolvePackage(paths, settings, profileDB, plan, rootRepo, rootName, selectedVersion, false); err != nil {
			return err
		}
	}
	newActive := active
	if active.ProfilePath == settings.ProfilePath {
		newActive = newSettings
	}
	return commitPackagePlan(paths, db, plan, settings, newSettings, active, newActive, false)
}

func envRemove(paths AppPaths, spec string) error {
	name, version, err := parsePackageVersion(spec)
	if err != nil {
		return err
	}
	settings, err := loadDefaultSettings(paths)
	if err != nil {
		return err
	}
	active, err := loadSettings(paths)
	if err != nil {
		return err
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	ensureProfileClosure(paths, &db, settings)
	available := packagesForProfile(db, settings.ProfileName)
	installed, exists := available[normalizePackageName(name)]
	if !exists || !versionsEqual(installed.Version, version) {
		return appError("package_not_installed", spec)
	}
	newSettings := cloneSettings(settings)
	removeEnabledPackage(&newSettings, name, version)
	closure, err := closureFromEnabled(paths, newSettings, available)
	if err != nil {
		return err
	}
	plan := newPackagePlan()
	plan.Resolved = closure
	newActive := active
	if active.ProfilePath == settings.ProfilePath {
		newActive = newSettings
	}
	return commitPackagePlan(paths, db, plan, settings, newSettings, active, newActive, false)
}

func closureFromEnabled(paths AppPaths, settings Settings, available map[string]InstalledPackage) (map[string]InstalledPackage, error) {
	closure := map[string]InstalledPackage{}
	visiting := map[string]bool{}
	var include func(string, string) error
	include = func(name, version string) error {
		key := normalizePackageName(name)
		if existing, ok := closure[key]; ok {
			if versionsEqual(existing.Version, version) {
				return nil
			}
			return appError("dependency_conflict", name+" has incompatible profile versions")
		}
		pkg, ok := available[key]
		if !ok || !versionsEqual(pkg.Version, version) || verifyStoredPackage(paths, pkg) != nil {
			return appError("package_not_installed", name+"=="+version+"; run profiles update to repair the profile closure")
		}
		if visiting[key] {
			return appError("dependency_cycle", name)
		}
		visiting[key] = true
		closure[key] = pkg
		for _, dependency := range pkg.Dependencies {
			alternatives, err := parseDependencyGroup(dependency)
			if err != nil {
				return err
			}
			satisfied := false
			for _, alternative := range alternatives {
				dependencyName, constraint := alternative[0], alternative[1]
				if systemVersion, ok := systemPackageVersion(dependencyName); ok {
					if match, _ := versionSatisfies(systemVersion, constraint); match {
						satisfied = true
						break
					}
				}
				if candidate, ok := available[normalizePackageName(dependencyName)]; ok {
					if match, _ := versionSatisfies(candidate.Version, constraint); match {
						if err := include(dependencyName, candidate.Version); err != nil {
							return err
						}
						satisfied = true
						break
					}
				}
			}
			if !satisfied {
				return appError("dependency_not_found", pkg.Name+" requires "+dependency)
			}
		}
		delete(visiting, key)
		return nil
	}
	for _, spec := range settings.Enabled {
		name, version, err := parsePackageVersion(spec)
		if err != nil {
			return nil, err
		}
		if err := include(name, version); err != nil {
			return nil, err
		}
	}
	return closure, nil
}

func setFreeze(paths AppPaths, spec string, frozen bool) error {
	name, version, err := parsePackageVersion(spec)
	if err != nil {
		return err
	}
	settings, err := loadDefaultSettings(paths)
	if err != nil {
		return err
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	ensureProfileClosure(paths, &db, settings)
	key := normalizePackageName(name)
	p, ok := packagesForProfile(db, settings.ProfileName)[key]
	if !ok || !versionsEqual(p.Version, version) {
		return appError("package_not_installed", spec)
	}
	currentlyFrozen := versionsEqual(db.ProfileFrozen[settings.ProfileName][key], version)
	if currentlyFrozen == frozen {
		if frozen {
			return nil
		}
		return appError("package_not_frozen", spec)
	}
	if db.ProfileFrozen[settings.ProfileName] == nil {
		db.ProfileFrozen[settings.ProfileName] = map[string]string{}
	}
	if frozen {
		db.ProfileFrozen[settings.ProfileName][key] = version
	} else {
		delete(db.ProfileFrozen[settings.ProfileName], key)
	}
	return saveDatabase(paths.Database, db)
}

func setUpdate(paths AppPaths) error {
	return updateProfile(paths, "", true)
}

func updateProfile(paths AppPaths, name string, makeCurrent bool) error {
	var settings Settings
	var err error
	if name == "" {
		settings, err = loadDefaultSettings(paths)
	} else {
		settings, err = loadNamedSettings(paths, name)
	}
	if err != nil {
		return err
	}
	newSettings := cloneSettings(settings)
	newSettings.Enabled = []string{}
	active, err := loadSettings(paths)
	if err != nil {
		return err
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	ensureProfileClosure(paths, &db, settings)
	profileDB := databaseForProfile(db, settings)
	plan := newPackagePlan()
	for _, spec := range settings.Enabled {
		packageName, oldVersion, err := parsePackageVersion(spec)
		if err != nil {
			return err
		}
		preferredRepo := ""
		if installed, exists := profileDB.Packages[normalizePackageName(packageName)]; exists {
			preferredRepo = installed.Source
		}
		repo, _, resolvedVersion, _, err := findRepositoryVersionCached(settings, packageName, oldVersion, preferredRepo, plan.Indexes)
		if err != nil {
			return err
		}
		if err := resolvePackage(paths, settings, profileDB, plan, repo, packageName, resolvedVersion, true); err != nil {
			return err
		}
		if strings.EqualFold(resolvedVersion, "latest") || !isValidVersion(resolvedVersion) {
			return appError("invalid_version", "resolver did not produce a concrete package version")
		}
		if err := setEnabledPackage(&newSettings, packageName, resolvedVersion); err != nil {
			return err
		}
	}
	newActive := active
	if makeCurrent || active.ProfilePath == settings.ProfilePath {
		newActive = newSettings
	}
	return commitPackagePlan(paths, db, plan, settings, newSettings, active, newActive, makeCurrent)
}

func cloneSettings(settings Settings) Settings {
	clone := Settings{
		Channels:    append([]Channel(nil), settings.Channels...),
		Enabled:     append([]string(nil), settings.Enabled...),
		ProfileName: settings.ProfileName,
		ProfilePath: settings.ProfilePath,
	}
	return clone
}

func setEnabledPackage(settings *Settings, name, version string) error {
	if settings == nil {
		return appError("invalid_config", "settings are unavailable")
	}
	if err := validatePackageName(name); err != nil {
		return err
	}
	if err := validateVersion(version); err != nil {
		return err
	}
	key := normalizePackageName(name)
	updated := make([]string, 0, len(settings.Enabled)+1)
	for _, spec := range settings.Enabled {
		existingName, _, err := parsePackageVersion(spec)
		if err != nil {
			return appError("invalid_config", "enabled entry must be name==version: "+spec)
		}
		if normalizePackageName(existingName) != key {
			updated = append(updated, spec)
		}
	}
	updated = append(updated, name+"=="+version)
	settings.Enabled = updated
	return normalizeSettings(settings)
}

func removeEnabledPackage(settings *Settings, name, _ string) {
	if settings == nil {
		return
	}
	key := normalizePackageName(name)
	updated := make([]string, 0, len(settings.Enabled))
	for _, spec := range settings.Enabled {
		existingName, _, err := parsePackageVersion(spec)
		if err == nil && normalizePackageName(existingName) == key {
			continue
		}
		updated = append(updated, spec)
	}
	settings.Enabled = updated
	_ = normalizeSettings(settings)
}

func cloneFileModes(modes map[string]uint32) map[string]uint32 {
	if modes == nil {
		return nil
	}
	clone := make(map[string]uint32, len(modes))
	for path, mode := range modes {
		clone[path] = mode
	}
	return clone
}
