package nixnet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type packagePlan struct {
	Artifacts          map[string]PackageArtifact
	Resolved           map[string]InstalledPackage
	Frozen             map[string]string
	ActivateGeneration string
	Resolving          map[string]bool
	Replace            map[string]bool
	Indexes            map[string]RepositoryIndex
	DownloadSizes      map[string]int64
}

func newPackagePlan() *packagePlan {
	return &packagePlan{Artifacts: map[string]PackageArtifact{}, Resolved: map[string]InstalledPackage{}, Resolving: map[string]bool{}, Replace: map[string]bool{}, Indexes: map[string]RepositoryIndex{}, DownloadSizes: map[string]int64{}}
}

func clonePackagePlan(source *packagePlan) *packagePlan {
	clone := newPackagePlan()
	clone.Indexes = source.Indexes
	for key, value := range source.Artifacts {
		clone.Artifacts[key] = value
	}
	for key, value := range source.DownloadSizes {
		clone.DownloadSizes[key] = value
	}
	for key, value := range source.Resolved {
		clone.Resolved[key] = value
	}
	if source.Frozen != nil {
		clone.Frozen = map[string]string{}
		for key, value := range source.Frozen {
			clone.Frozen[key] = value
		}
	}
	clone.ActivateGeneration = source.ActivateGeneration
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
	destination.DownloadSizes = source.DownloadSizes
	destination.Resolved = source.Resolved
	destination.Frozen = source.Frozen
	destination.ActivateGeneration = source.ActivateGeneration
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
				if readErr != nil || parseErr != nil || entry.Architecture != "" && !strings.EqualFold(goArchitectureForDebian(entry.Architecture), manifest.Architecture) {
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
			if entry.Architecture != "" && !strings.EqualFold(goArchitectureForDebian(entry.Architecture), resolved.Manifest.Architecture) {
				delete(plan.Resolving, key)
				return appError("invalid_package", "DEB Architecture does not match the APT Packages index")
			}
			artifact = &resolved
			plan.DownloadSizes[key] = entry.Size
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
					// Keep reused dependencies in the resulting profile closure. Merely
					// checking an installed package here made re-installing an existing
					// root silently drop its transitive dependencies from the closure.
					candidatePlan.Resolved[key] = installed
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
	defer func() {
		delete(plan.Resolving, key)
	}()
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
	profileChanged := oldSettings.EnabledNeedsNormalization || encodeProfileTOML(oldSettings.Enabled) != encodeProfileTOML(newSettings.Enabled)
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
	frozenChanged := false
	if plan.Frozen != nil && newSettings.ProfileName != "" {
		currentFrozen := db.ProfileFrozen[newSettings.ProfileName]
		if len(currentFrozen) != len(plan.Frozen) {
			frozenChanged = true
		} else {
			for name, version := range plan.Frozen {
				if !versionsEqual(currentFrozen[normalizePackageName(name)], version) {
					frozenChanged = true
					break
				}
			}
		}
	}
	activeGeneration := ""
	if newSettings.ProfileName != "" {
		var generationErr error
		activeGeneration, generationErr = readCurrentGeneration(paths, newSettings.ProfileName)
		if generationErr != nil {
			return generationErr
		}
	}
	if plan.ActivateGeneration != "" && plan.ActivateGeneration != activeGeneration {
		closureChanged = true
	}
	if len(plan.Artifacts) == 0 && !profileChanged && !closureChanged && !frozenChanged && !loadedChanged && !activeChanged {
		return nil
	}
	if err := ensureDirectory(paths.Store, 0o755); err != nil {
		return appError("io_error", err.Error())
	}
	if err := ensureDirectory(paths.Bin, 0o755); err != nil {
		return appError("io_error", err.Error())
	}
	if err := ensureDirectory(paths.Staging, 0o755); err != nil {
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
		if plan.Frozen != nil {
			newDB.ProfileFrozen[newSettings.ProfileName] = map[string]string{}
			for name, version := range plan.Frozen {
				newDB.ProfileFrozen[newSettings.ProfileName][normalizePackageName(name)] = version
			}
		}
	}
	profileDB := Database{Packages: packagesForProfile(newDB, newSettings.ProfileName)}
	if err := validateInstalledDependencies(profileDB); err != nil {
		_ = os.RemoveAll(stageRoot)
		return err
	}
	fileChanges := []transactionFileChange{}
	if plan.ActivateGeneration != "" {
		newSettings.GenerationID = plan.ActivateGeneration
		if newActive.ProfilePath == newSettings.ProfilePath {
			newActive.GenerationID = plan.ActivateGeneration
		}
		pointerChange, changeErr := generationPointerChange(paths, newSettings.ProfileName, plan.ActivateGeneration)
		if changeErr != nil {
			_ = os.RemoveAll(stageRoot)
			return changeErr
		}
		fileChanges = append(fileChanges, pointerChange)
	} else if newSettings.ProfileName != "" && (profileChanged || closureChanged || frozenChanged) {
		generation, generationErr := newProfileGeneration(newSettings.ProfileName, newSettings, newDB)
		if generationErr != nil {
			_ = os.RemoveAll(stageRoot)
			return generationErr
		}
		newSettings.GenerationID = generation.ID
		if newActive.ProfilePath == newSettings.ProfilePath {
			newActive.GenerationID = generation.ID
		}
		manifestChange, changeErr := generationManifestChange(paths, generation)
		if changeErr != nil {
			_ = os.RemoveAll(stageRoot)
			return changeErr
		}
		pointerChange, changeErr := generationPointerChange(paths, newSettings.ProfileName, generation.ID)
		if changeErr != nil {
			_ = os.RemoveAll(stageRoot)
			return changeErr
		}
		fileChanges = append(fileChanges, manifestChange, pointerChange)
	}
	launcherChanges, launcherErr := profileLauncherChanges(paths, newDB, newSettings)
	if launcherErr != nil {
		_ = os.RemoveAll(stageRoot)
		return launcherErr
	}
	fileChanges = append(fileChanges, launcherChanges...)
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
	journal := transactionJournal{StageRoot: stageRoot, DatabaseExisted: exists, DatabaseBackup: backup, Moves: moves, Links: links, Files: fileChanges}
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
	return envInstallWithPlan(paths, spec, nil)
}

type packageInstallPlan struct {
	Changes         []PackageTransactionItem
	Root            InstalledPackage
	Previous        InstalledPackage
	HadPrevious     bool
	ExplicitlyAdded bool
	ResolvedCount   int
}

func envInstallWithPlan(paths AppPaths, spec string, onPlan func(packageInstallPlan)) error {
	return envInstallWithProgress(paths, spec, nil, onPlan)
}

func envInstallWithProgress(paths AppPaths, spec string, onStart func(), onPlan func(packageInstallPlan)) error {
	return envInstallForProfileWithProgress(paths, spec, "", onStart, onPlan)
}

func envInstallForProfileWithProgress(paths AppPaths, spec, profile string, onStart func(), onPlan func(packageInstallPlan)) error {
	repo, name, version, err := parseRepoPackageSpec(spec)
	if err != nil {
		return err
	}
	if version == "" {
		return appError("invalid_env_install_format", spec+"; version is required")
	}
	settings, err := loadEnvironmentSettings(paths, profile)
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
	if err := ensureProfileClosure(paths, &db, settings); err != nil {
		return err
	}
	profileDB := databaseForProfile(db, settings)
	plan := newPackagePlan()
	// Resolve the requested package version before showing progress. This catches
	// missing packages and versions without briefly claiming an install started.
	rootRepo, _, selectedVersion, _, err := findRepositoryVersionCached(settings, name, version, repo, plan.Indexes)
	if err != nil {
		return err
	}
	if onStart != nil {
		onStart()
	}
	if err := resolvePackage(paths, settings, profileDB, plan, rootRepo, name, selectedVersion, false); err != nil {
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
	if onPlan != nil {
		root := plan.Resolved[normalizePackageName(name)]
		previous, hadPrevious := profileDB.Packages[normalizePackageName(name)]
		if root.Name == "" && hadPrevious {
			root = previous
		}
		resolvedCount := len(plan.Resolved)
		if resolvedCount == 0 {
			resolvedCount = 1
		}
		explicitlyEnabled := false
		for _, enabledSpec := range settings.Enabled {
			enabledName, _, parseErr := parsePackageVersion(enabledSpec)
			if parseErr == nil && normalizePackageName(enabledName) == normalizePackageName(name) {
				explicitlyEnabled = true
				break
			}
		}
		onPlan(packageInstallPlan{
			Changes:         packagePlanChanges(paths, profileDB.Packages, plan),
			Root:            root,
			Previous:        previous,
			HadPrevious:     hadPrevious,
			ExplicitlyAdded: !explicitlyEnabled,
			ResolvedCount:   resolvedCount,
		})
	}
	newActive := active
	if active.ProfilePath == settings.ProfilePath {
		newActive = newSettings
	}
	return commitPackagePlan(paths, db, plan, settings, newSettings, active, newActive, false)
}

func packagePlanChanges(paths AppPaths, current map[string]InstalledPackage, plan *packagePlan) []PackageTransactionItem {
	changes := make([]PackageTransactionItem, 0)
	for key, pkg := range plan.Resolved {
		installed, exists := current[key]
		if exists && installed.Dir == pkg.Dir && !plan.Replace[key] {
			continue
		}
		item := PackageTransactionItem{Package: pkg, DownloadSize: plan.DownloadSizes[key], Action: packageChangeAction(installed, pkg, exists)}
		if artifact, ok := plan.Artifacts[key]; ok {
			item.InstalledSize = artifact.InstalledSize
		} else if size, err := packageStorePayloadSize(paths, pkg); err == nil {
			item.InstalledSize = size
		}
		changes = append(changes, item)
	}
	return changes
}

func packageClosureChanges(current, resolved map[string]InstalledPackage) []PackageTransactionItem {
	changes := make([]PackageTransactionItem, 0)
	for key, installed := range current {
		retained, exists := resolved[key]
		if !exists || retained.Dir != installed.Dir {
			changes = append(changes, PackageTransactionItem{Package: installed, Action: "Remove"})
		}
	}
	return changes
}

func packageStorePayloadSize(paths AppPaths, pkg InstalledPackage) (int64, error) {
	root := filepath.Join(paths.Store, pkg.Dir, "payload")
	var size int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if info.Size() > int64(^uint64(0)>>1)-size {
				return fmt.Errorf("package payload size overflows int64")
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func envRemove(paths AppPaths, spec string) error {
	return envRemoveWithPlan(paths, spec, nil)
}

func envRemoveWithPlan(paths AppPaths, spec string, onPlan func([]PackageTransactionItem)) error {
	return envRemoveWithProgress(paths, spec, nil, onPlan)
}

func envRemoveWithProgress(paths AppPaths, spec string, onStart func(), onPlan func([]PackageTransactionItem)) error {
	return envRemoveForProfileWithProgress(paths, spec, "", onStart, onPlan)
}

func envRemoveForProfileWithProgress(paths AppPaths, spec, profile string, onStart func(), onPlan func([]PackageTransactionItem)) error {
	name, version, err := parsePackageVersionWithLatest(spec, true)
	if err != nil {
		return err
	}
	settings, err := loadEnvironmentSettings(paths, profile)
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
	if err := ensureProfileClosure(paths, &db, settings); err != nil {
		return err
	}
	available := packagesForProfile(db, settings.ProfileName)
	installed, exists := available[normalizePackageName(name)]
	if !exists || (!strings.EqualFold(version, "latest") && !versionsEqual(installed.Version, version)) {
		return appError("package_not_installed", spec)
	}
	version = installed.Version
	if onStart != nil {
		onStart()
	}
	newSettings := cloneSettings(settings)
	removeEnabledPackage(&newSettings, name, version)
	closure, err := closureFromEnabled(paths, newSettings, available)
	if err != nil {
		return err
	}
	plan := newPackagePlan()
	plan.Resolved = closure
	if onPlan != nil {
		onPlan(packageClosureChanges(available, closure))
	}
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
	return setFreezeForProfile(paths, spec, "", frozen)
}

func setFreezeForProfile(paths AppPaths, spec, profile string, frozen bool) error {
	name, version, err := parsePackageVersion(spec)
	if err != nil {
		return err
	}
	settings, err := loadEnvironmentSettings(paths, profile)
	if err != nil {
		return err
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	if err := ensureProfileClosure(paths, &db, settings); err != nil {
		return err
	}
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
	active, err := loadSettings(paths)
	if err != nil {
		return err
	}
	plan := newPackagePlan()
	plan.Resolved = packagesForProfile(db, settings.ProfileName)
	plan.Frozen = map[string]string{}
	for packageName, packageVersion := range db.ProfileFrozen[settings.ProfileName] {
		plan.Frozen[packageName] = packageVersion
	}
	if frozen {
		plan.Frozen[key] = version
	} else {
		delete(plan.Frozen, key)
	}
	return commitPackagePlan(paths, db, plan, settings, settings, active, active, false)
}

func setUpdate(paths AppPaths) error {
	return updateProfile(paths, "", true)
}

func updateProfile(paths AppPaths, name string, makeCurrent bool) error {
	return updateProfileWithPlan(paths, name, makeCurrent, nil)
}

type packageUpdatePlan struct {
	Changes       []PackageTransactionItem
	ResolvedCount int
}

type envUpdatePlan struct {
	Channel     string
	Selector    string
	Profile     string
	TargetCount int
	Changes     []PackageTransactionItem
	Frozen      []InstalledPackage
	Unavailable []InstalledPackage
}

// updateEnvironmentWithPlan upgrades roots in the currently loaded profile.
// The channel is mandatory so an update never silently changes package source.
func updateEnvironmentWithPlan(paths AppPaths, spec string, onStart func(), onPlan func(envUpdatePlan)) error {
	return updateEnvironmentForProfileWithPlan(paths, spec, "", onStart, onPlan)
}

func updateEnvironmentForProfileWithPlan(paths AppPaths, spec, profile string, onStart func(), onPlan func(envUpdatePlan)) error {
	repo, selector, err := parseEnvUpdateSpec(spec)
	if err != nil {
		return err
	}
	settings, err := loadEnvironmentSettings(paths, profile)
	if err != nil {
		return err
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	if err := ensureProfileClosure(paths, &db, settings); err != nil {
		return err
	}
	profileDB := databaseForProfile(db, settings)
	channel, exists := repositoryChannel(settings, repo)
	if !exists {
		return appError("channel_not_found", repo)
	}
	selected := map[string]InstalledPackage{}
	selectedKey := normalizePackageName(selector)
	for _, enabled := range settings.Enabled {
		name, _, parseErr := parsePackageVersion(enabled)
		if parseErr != nil {
			return parseErr
		}
		pkg, installed := profileDB.Packages[normalizePackageName(name)]
		if !installed {
			continue
		}
		if strings.EqualFold(selector, "all") {
			if pkg.Source == repo {
				selected[normalizePackageName(name)] = pkg
			}
		} else if normalizePackageName(name) == selectedKey {
			selected[normalizePackageName(name)] = pkg
		}
	}
	if !strings.EqualFold(selector, "all") && len(selected) == 0 {
		return appError("package_not_installed", selector+" in the currently loaded profile")
	}

	result := envUpdatePlan{Channel: repo, Selector: selector, Profile: strings.TrimSuffix(settings.ProfileName, ".toml"), TargetCount: len(selected)}
	if len(selected) == 0 {
		if onPlan != nil {
			onPlan(result)
		}
		return nil
	}
	updateKeys := make([]string, 0, len(selected))
	for key, pkg := range selected {
		if pkg.Frozen {
			result.Frozen = append(result.Frozen, pkg)
			continue
		}
		updateKeys = append(updateKeys, key)
	}
	sortStrings(updateKeys)
	if len(updateKeys) == 0 {
		if onPlan != nil {
			onPlan(result)
		}
		return nil
	}
	if onStart != nil {
		onStart()
	}
	plan := newPackagePlan()
	index, err := loadRepositoryIndexCached(channel, plan.Indexes)
	if err != nil {
		return err
	}
	newSettings := cloneSettings(settings)
	for _, key := range updateKeys {
		installed := selected[key]
		entry, latest, found := latestRepositoryVersion(index.Packages[key])
		if !found {
			if !strings.EqualFold(selector, "all") {
				return appError("version_not_found", installed.Name+" latest in channel "+repo)
			}
			result.Unavailable = append(result.Unavailable, installed)
			continue
		}
		comparison, compareErr := compareRepositoryVersions(latest, installed.Version, entry.Debian)
		if compareErr != nil {
			return appError("invalid_version", compareErr.Error())
		}
		if comparison <= 0 {
			continue
		}
		if err := resolvePackage(paths, settings, profileDB, plan, repo, installed.Name, latest, false); err != nil {
			return err
		}
		if err := setEnabledPackage(&newSettings, installed.Name, latest); err != nil {
			return err
		}
	}
	available := make(map[string]InstalledPackage, len(profileDB.Packages)+len(plan.Resolved))
	for key, pkg := range profileDB.Packages {
		available[key] = pkg
	}
	for key, pkg := range plan.Resolved {
		available[key] = pkg
	}
	closure, err := closureFromEnabled(paths, newSettings, available)
	if err != nil {
		return err
	}
	plan.Resolved = closure
	result.Changes = packagePlanChanges(paths, profileDB.Packages, plan)
	for key, pkg := range profileDB.Packages {
		if _, retained := closure[key]; !retained {
			result.Changes = append(result.Changes, PackageTransactionItem{Package: pkg, Action: "Remove"})
		}
	}
	if onPlan != nil {
		onPlan(result)
	}
	active, err := loadSettings(paths)
	if err != nil {
		return err
	}
	newActive := active
	if active.ProfilePath == settings.ProfilePath {
		newActive = newSettings
	}
	return commitPackagePlan(paths, db, plan, settings, newSettings, active, newActive, false)
}

func compareRepositoryVersions(candidate, installed string, debian bool) (int, error) {
	if debian {
		return compareDebianVersions(candidate, installed)
	}
	return compareVersions(candidate, installed)
}

func updateProfileWithPlan(paths AppPaths, name string, makeCurrent bool, onPlan func(packageUpdatePlan)) error {
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
	if onPlan != nil {
		onPlan(packageUpdatePlan{Changes: packagePlanChanges(paths, profileDB.Packages, plan), ResolvedCount: len(settings.Enabled)})
	}
	return commitPackagePlan(paths, db, plan, settings, newSettings, active, newActive, makeCurrent)
}

func packageChangeAction(previous, next InstalledPackage, hadPrevious bool) string {
	if !hadPrevious {
		return "Install"
	}
	comparison, err := compareVersions(next.Version, previous.Version)
	if err == nil {
		switch {
		case comparison > 0:
			return "Update"
		case comparison < 0:
			return "Downgrade"
		}
	}
	return "Reinstall"
}

func cloneSettings(settings Settings) Settings {
	clone := Settings{
		Channels:                  append([]Channel(nil), settings.Channels...),
		Enabled:                   append([]string(nil), settings.Enabled...),
		ProfileName:               settings.ProfileName,
		ProfilePath:               settings.ProfilePath,
		GenerationID:              settings.GenerationID,
		EnabledNeedsNormalization: settings.EnabledNeedsNormalization,
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
