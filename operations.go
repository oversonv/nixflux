package nixflux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type packagePlan struct {
	Artifacts map[string]PackageArtifact
	Resolving map[string]bool
	Replace   map[string]bool
	Indexes   map[string]RepositoryIndex
}

func newPackagePlan() *packagePlan {
	return &packagePlan{Artifacts: map[string]PackageArtifact{}, Resolving: map[string]bool{}, Replace: map[string]bool{}, Indexes: map[string]RepositoryIndex{}}
}

func clonePackagePlan(source *packagePlan) *packagePlan {
	clone := newPackagePlan()
	clone.Indexes = source.Indexes
	for key, value := range source.Artifacts {
		clone.Artifacts[key] = value
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
	dependencies := []string{}
	if useInstalled {
		dependencies = append(dependencies, installed.Dependencies...)
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

	for _, dependency := range dependencies {
		if dependencyErr := resolveDependencyGroup(paths, settings, db, plan, dependency, repo, allowDowngrade); dependencyErr != nil {
			delete(plan.Resolving, key)
			return appError("dependency_not_found", name+" requires "+dependency+": "+dependencyErr.Error())
		}
	}
	delete(plan.Resolving, key)
	if artifact != nil {
		plan.Artifacts[key] = *artifact
	}
	return nil
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

func commitPackagePlan(paths AppPaths, db Database, plan *packagePlan, oldSettings, newSettings Settings) error {
	if err := normalizeSettings(&newSettings); err != nil {
		return err
	}
	settingsChanged := encodeSettingsTOML(oldSettings) != encodeSettingsTOML(newSettings)
	if len(plan.Artifacts) == 0 && !settingsChanged {
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
	newDB := Database{SchemaVersion: databaseSchemaVersion, Packages: map[string]InstalledPackage{}, LegacyPackages: map[string]InstalledPackage{}}
	for k, v := range oldDB.Packages {
		newDB.Packages[k] = v
	}
	for k, v := range oldDB.LegacyPackages {
		newDB.LegacyPackages[k] = v
	}
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
		if old, exists := newDB.Packages[key]; exists && old.Dir != dir {
			oldPath := filepath.Join(paths.Store, old.Dir)
			if _, e := os.Lstat(oldPath); e == nil {
				backup := filepath.Join(stageRoot, "old", old.Dir)
				moves = append(moves, transactionMove{Kind: "remove", Source: oldPath, Destination: backup})
			} else if !os.IsNotExist(e) {
				_ = os.RemoveAll(stageRoot)
				return appError("io_error", e.Error())
			}
		}
		frozen := false
		if previous, exists := newDB.Packages[key]; exists && versionsEqual(previous.Version, a.Manifest.Version) {
			frozen = previous.Frozen
		}
		newDB.Packages[key] = InstalledPackage{Name: a.Manifest.Name, Version: a.Manifest.Version, Hash: a.Hash, ManifestHash: a.ManifestHash, ChecksumsHash: a.ChecksumsHash, Dir: dir, Source: a.Source, Dependencies: append([]string{}, a.Manifest.Dependencies...), Files: append([]string{}, a.Files...), Entrypoints: append([]string{}, a.Manifest.Entrypoints...), Modes: cloneFileModes(a.Modes), Frozen: frozen}
	}
	if err := validateInstalledDependencies(newDB); err != nil {
		_ = os.RemoveAll(stageRoot)
		return err
	}
	links, err := buildLinkChanges(paths, oldDB, newDB, oldSettings, newSettings)
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
	if settingsChanged {
		settingsExisted, settingsBackup, settingsErr := databaseBackup(paths.Settings)
		if settingsErr != nil {
			_ = os.RemoveAll(stageRoot)
			return appError("io_error", settingsErr.Error())
		}
		journal.SettingsChanged = true
		journal.SettingsExisted = settingsExisted
		journal.SettingsBackup = settingsBackup
		journal.SettingsContent = []byte(encodeSettingsTOML(newSettings))
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
	settings, err := loadSettings(paths)
	if err != nil {
		return err
	}
	newSettings := cloneSettings(settings)
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	plan := newPackagePlan()
	if err := resolvePackage(paths, settings, db, plan, repo, name, version, false); err != nil {
		return err
	}
	resolvedVersion := version
	if artifact, ok := plan.Artifacts[normalizePackageName(name)]; ok {
		resolvedVersion = artifact.Manifest.Version
	} else if installed, ok := db.Packages[normalizePackageName(name)]; ok {
		resolvedVersion = installed.Version
	}
	if strings.EqualFold(resolvedVersion, "latest") || !isValidVersion(resolvedVersion) {
		return appError("invalid_version", "resolver did not produce a concrete package version")
	}
	if err := setEnabledPackage(&newSettings, name, resolvedVersion); err != nil {
		return err
	}
	return commitPackagePlan(paths, db, plan, settings, newSettings)
}

func envRemove(paths AppPaths, spec string) error {
	name, version, err := parsePackageVersion(spec)
	if err != nil {
		return err
	}
	settings, err := loadSettings(paths)
	if err != nil {
		return err
	}
	newSettings := cloneSettings(settings)
	removeEnabledPackage(&newSettings, name, version)
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	key := normalizePackageName(name)
	pkg, ok := db.Packages[key]
	if !ok || !versionsEqual(pkg.Version, version) {
		legacyKey := ""
		var legacy InstalledPackage
		for candidateKey, candidate := range db.LegacyPackages {
			if normalizePackageName(candidate.Name) == key && versionsEqual(candidate.Version, version) {
				legacyKey, legacy = candidateKey, candidate
				break
			}
		}
		if legacyKey == "" {
			return appError("package_not_installed", spec)
		}
		if err := os.MkdirAll(paths.Staging, 0o755); err != nil {
			return appError("io_error", err.Error())
		}
		stageRoot, err := os.MkdirTemp(paths.Staging, "legacy-remove-")
		if err != nil {
			return appError("io_error", err.Error())
		}
		newDB := Database{SchemaVersion: databaseSchemaVersion, Packages: map[string]InstalledPackage{}, LegacyPackages: map[string]InstalledPackage{}}
		for k, v := range db.Packages {
			newDB.Packages[k] = v
		}
		for k, v := range db.LegacyPackages {
			if k != legacyKey {
				newDB.LegacyPackages[k] = v
			}
		}
		moves := []transactionMove{}
		oldPath := filepath.Join(paths.Store, legacy.Dir)
		if _, e := os.Lstat(oldPath); e == nil {
			moves = append(moves, transactionMove{Kind: "remove", Source: oldPath, Destination: filepath.Join(stageRoot, "old", legacy.Dir)})
		} else if !os.IsNotExist(e) {
			_ = os.RemoveAll(stageRoot)
			return appError("io_error", e.Error())
		}
		links, linkErr := buildLinkChanges(paths, db, newDB, settings, newSettings)
		if linkErr != nil {
			_ = os.RemoveAll(stageRoot)
			return linkErr
		}
		exists, backup, err := databaseBackup(paths.Database)
		if err != nil {
			_ = os.RemoveAll(stageRoot)
			return appError("io_error", err.Error())
		}
		journal := transactionJournal{StageRoot: stageRoot, DatabaseExisted: exists, DatabaseBackup: backup, Moves: moves, Links: links}
		if encodeSettingsTOML(settings) != encodeSettingsTOML(newSettings) {
			settingsExisted, settingsBackup, settingsErr := databaseBackup(paths.Settings)
			if settingsErr != nil {
				_ = os.RemoveAll(stageRoot)
				return appError("io_error", settingsErr.Error())
			}
			journal.SettingsChanged = true
			journal.SettingsExisted = settingsExisted
			journal.SettingsBackup = settingsBackup
			journal.SettingsContent = []byte(encodeSettingsTOML(newSettings))
		}
		return applyTransaction(paths, journal, newDB)
	}
	for otherKey, other := range db.Packages {
		if otherKey == key {
			continue
		}
		for _, dep := range other.Dependencies {
			alternatives, parseErr := parseDependencyGroup(dep)
			if parseErr != nil {
				return appError("invalid_database", parseErr.Error())
			}
			mentionsTarget := false
			otherAlternativeWorks := false
			for _, alternative := range alternatives {
				depName, constraint := alternative[0], alternative[1]
				if normalizePackageName(depName) == key {
					mentionsTarget = true
					continue
				}
				if installed, exists := db.Packages[normalizePackageName(depName)]; exists {
					if satisfies, e := versionSatisfies(installed.Version, constraint); e == nil && satisfies {
						otherAlternativeWorks = true
					}
				}
				if version, exists := systemPackageVersion(depName); exists {
					if satisfies, e := versionSatisfies(version, constraint); e == nil && satisfies {
						otherAlternativeWorks = true
					}
				}
			}
			if mentionsTarget && !otherAlternativeWorks {
				return appError("package_required", other.Name+" requires "+dep)
			}
		}
	}
	newDB := Database{SchemaVersion: databaseSchemaVersion, Packages: map[string]InstalledPackage{}, LegacyPackages: map[string]InstalledPackage{}}
	for k, v := range db.LegacyPackages {
		newDB.LegacyPackages[k] = v
	}
	for k, v := range db.Packages {
		if k != key {
			newDB.Packages[k] = v
		}
	}
	if err := os.MkdirAll(paths.Staging, 0o755); err != nil {
		return appError("io_error", err.Error())
	}
	stageRoot, err := os.MkdirTemp(paths.Staging, "remove-")
	if err != nil {
		return appError("io_error", err.Error())
	}
	oldPath := filepath.Join(paths.Store, pkg.Dir)
	moves := []transactionMove{}
	if _, e := os.Lstat(oldPath); e == nil {
		moves = append(moves, transactionMove{Kind: "remove", Source: oldPath, Destination: filepath.Join(stageRoot, "old", pkg.Dir)})
	} else if !os.IsNotExist(e) {
		_ = os.RemoveAll(stageRoot)
		return appError("io_error", e.Error())
	}
	links, err := buildLinkChanges(paths, db, newDB, settings, newSettings)
	if err != nil {
		_ = os.RemoveAll(stageRoot)
		return err
	}
	exists, backup, err := databaseBackup(paths.Database)
	if err != nil {
		_ = os.RemoveAll(stageRoot)
		return err
	}
	journal := transactionJournal{StageRoot: stageRoot, DatabaseExisted: exists, DatabaseBackup: backup, Moves: moves, Links: links}
	if encodeSettingsTOML(settings) != encodeSettingsTOML(newSettings) {
		settingsExisted, settingsBackup, settingsErr := databaseBackup(paths.Settings)
		if settingsErr != nil {
			_ = os.RemoveAll(stageRoot)
			return appError("io_error", settingsErr.Error())
		}
		journal.SettingsChanged = true
		journal.SettingsExisted = settingsExisted
		journal.SettingsBackup = settingsBackup
		journal.SettingsContent = []byte(encodeSettingsTOML(newSettings))
	}
	return applyTransaction(paths, journal, newDB)
}

func setFreeze(paths AppPaths, spec string, frozen bool) error {
	name, version, err := parsePackageVersion(spec)
	if err != nil {
		return err
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	key := normalizePackageName(name)
	p, ok := db.Packages[key]
	if !ok || !versionsEqual(p.Version, version) {
		return appError("package_not_installed", spec)
	}
	if p.Frozen == frozen {
		if frozen {
			return nil
		}
		return appError("package_not_frozen", spec)
	}
	p.Frozen = frozen
	db.Packages[key] = p
	return saveDatabase(paths.Database, db)
}

func setUpdate(paths AppPaths) error {
	settings, err := loadSettings(paths)
	if err != nil {
		return err
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	plan := newPackagePlan()
	for _, spec := range settings.Enabled {
		name, version, err := parsePackageVersion(spec)
		if err != nil {
			return err
		}
		preferredRepo := ""
		if installed, exists := db.Packages[normalizePackageName(name)]; exists {
			preferredRepo = installed.Source
		}
		repo, _, resolvedVersion, _, err := findRepositoryVersionCached(settings, name, version, preferredRepo, plan.Indexes)
		if err != nil {
			return err
		}
		if err := resolvePackage(paths, settings, db, plan, repo, name, resolvedVersion, true); err != nil {
			return err
		}
	}
	return commitPackagePlan(paths, db, plan, settings, settings)
}

func cloneSettings(settings Settings) Settings {
	clone := Settings{
		Channels: append([]Channel(nil), settings.Channels...),
		Enabled:  append([]string(nil), settings.Enabled...),
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
