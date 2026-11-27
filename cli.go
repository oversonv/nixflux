package nixnet

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var cliContextState = struct {
	sync.RWMutex
	ctx context.Context
}{ctx: context.Background()}

func currentCLIContext() context.Context {
	cliContextState.RLock()
	defer cliContextState.RUnlock()
	return cliContextState.ctx
}

const usage = `usage: nixnet [-h] [--version] [--profile NAME] <group> <action> [argument]

commands:
  version
  env install <repo:name==version>...
  env remove <name==version|latest>...
  env list
  env freeze <name==version>
  env unfreeze <name==version>
  env frozen
  env update <channel>:<package|all>==latest
  env i <repo:name==version>  alias for install
  env r <name==version|latest>  alias for remove
  env u <channel>:<package|all>==latest  alias for update
  env l  alias for list
  env f <name==version>  alias for freeze
  env uf <name==version>  alias for unfreeze
  env lf  alias for frozen
  channel install <url>...
  channel remove <name>...
  channel list
  channel fetch <name>
  profiles list
  profiles install <name>
  profiles remove <name>
  profiles generations <name>
  profiles rollback <name> <generation-id>
  profiles update [name]

options:
  --profile NAME  target an environment profile (env commands; default: current)
  -h, --help   show this message and exit
  --version    show version and exit`

var commandArgumentCounts = map[string]int{
	"env install": 1, "env remove": 1, "env list": 0, "env freeze": 1, "env unfreeze": 1, "env frozen": 0, "env update": 1,
	"channel install": 1, "channel remove": 1, "channel list": 0, "channel fetch": 1,
	"profiles list": 0, "profiles install": 1, "profiles remove": 1, "profiles generations": 1, "profiles rollback": 2, "profiles update": 0,
}

func parseArguments(argv []string) ([]string, string, bool, error) {
	positional := []string{}
	profile := ""
	ended := false
	for i := 0; i < len(argv); i++ {
		token := argv[i]
		if ended || token == "-" || !strings.HasPrefix(token, "-") {
			positional = append(positional, token)
			continue
		}
		switch token {
		case "--":
			ended = true
		case "--profile":
			if i+1 >= len(argv) || strings.TrimSpace(argv[i+1]) == "" {
				return nil, "", false, appError("missing_args", "--profile requires a name")
			}
			if profile != "" {
				return nil, "", false, appError("too_many_args", "--profile was specified more than once")
			}
			profile = argv[i+1]
			i++
		case "-h", "--help":
			fmt.Println(usage)
			return nil, "", true, nil
		case "--version":
			if err := renderLegacyVersion(os.Stdout); err != nil {
				return nil, "", false, err
			}
			return nil, "", true, nil
		default:
			return nil, "", false, appError("unknown_option", token)
		}
	}
	return positional, profile, false, nil
}

func runCLI(argv []string) int {
	positional, profile, handled, err := parseArguments(argv)
	if handled {
		return 0
	}
	if err != nil {
		return reportError(err)
	}
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if len(positional) == 1 && positional[0] == "version" {
		if err := renderVersion(os.Stdout); err != nil {
			return reportError(appError("io_error", err.Error()))
		}
		return 0
	}
	if len(positional) < 2 {
		return reportError(appError("missing_args", ""))
	}
	group, action := positional[0], positional[1]
	if group == "env" {
		action = canonicalEnvAction(action)
	}
	command := group + " " + action
	expected, ok := commandArgumentCounts[command]
	if !ok {
		return reportError(appError("unknown_command", command))
	}
	if profile != "" {
		if !strings.HasPrefix(command, "env ") {
			return reportError(appError("invalid_config", "--profile can only be used with env commands"))
		}
		if _, err := profileFilename(profile); err != nil {
			return reportError(err)
		}
	}
	args := positional[2:]
	if command == "profiles update" && len(args) > 1 {
		return reportError(appError("too_many_args", strings.Join(args[1:], " ")))
	}
	if len(args) < expected {
		if command == "env update" {
			return reportError(appError("invalid_env_update_format", ""))
		}
		return reportError(appError("missing_args", ""))
	}
	variadic := command == "env install" || command == "env remove" || command == "channel install" || command == "channel remove"
	if command != "profiles update" && !variadic && len(args) > expected {
		return reportError(appError("too_many_args", strings.Join(args[expected:], " ")))
	}
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	// Validate command arguments before touching configuration or package state.
	switch command {
	case "env install":
		for _, spec := range args {
			if _, _, _, err := parseRepoPackageSpec(spec); err != nil {
				return reportError(err)
			}
		}
	case "env update":
		if _, _, err := parseEnvUpdateSpec(arg); err != nil {
			return reportError(err)
		}
	case "env remove":
		for _, spec := range args {
			if _, _, err := parsePackageVersionWithLatest(spec, true); err != nil {
				return reportError(err)
			}
		}
	case "env freeze", "env unfreeze":
		if _, _, err := parsePackageVersion(arg); err != nil {
			return reportError(err)
		}
	case "channel install":
		for _, value := range args {
			if strings.TrimSpace(value) == "" {
				return reportError(appError("invalid_config", "channel URL cannot be empty"))
			}
		}
	case "channel remove":
		for _, value := range args {
			if !channelNamePattern.MatchString(value) {
				return reportError(appError("invalid_config", "invalid channel name"))
			}
		}
	case "channel fetch":
		if !channelNamePattern.MatchString(arg) {
			return reportError(appError("invalid_config", "invalid channel name"))
		}
	case "profiles install", "profiles remove", "profiles update", "profiles generations", "profiles rollback":
		if arg != "" {
			if _, err := profileFilename(arg); err != nil {
				return reportError(err)
			}
		}
		if command == "profiles rollback" && !generationIDPattern.MatchString(args[1]) {
			return reportError(appError("invalid_config", "invalid profile generation ID"))
		}
	}
	home, err := fetchPathHome()
	if err != nil {
		return reportError(appError("io_error", err.Error()))
	}
	paths := pathsForHome(home)
	if paths.Bin == "" {
		return reportError(appError("invalid_config", "PREFIX must be an absolute Termux prefix on Android"))
	}
	unlock, err := acquireLock(paths.Lock)
	if err != nil {
		return reportError(appError("io_error", err.Error()))
	}
	defer unlock()
	if err := recoverTransaction(paths); err != nil {
		return reportError(appError("transaction_failed", err.Error()))
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return reportError(err)
	}
	settings, err := loadSettings(paths)
	if err != nil {
		return reportError(err)
	}
	if strings.HasPrefix(command, "env ") && profile == "" && command != "env install" {
		if err := ensureProfileClosure(paths, &db, settings); err != nil {
			return reportError(err)
		}
		if err := ensureProfileLaunchers(paths, db, settings); err != nil {
			return reportError(err)
		}
		if err := migrateLegacyCommandLinks(paths, db, settings); err != nil {
			return reportError(err)
		}
	}
	switch command {
	case "env install":
		packages := []PackageTransactionItem{}
		var planSummary packageInstallPlan
		operationStarted := time.Now()
		resolvedAt := operationStarted
		resolutionElapsed := time.Duration(0)
		var progress *operationProgress
		installErr := envInstallManyForProfileWithProgress(paths, args, profile, func() {
			progress = startOperationProgress("Install")
			if progress == nil {
				fmt.Println(uvDim("Resolving dependencies..."))
			}
		}, func(resolved packageInstallPlan) {
			planSummary = resolved
			packages = resolved.Changes
			resolutionElapsed = time.Since(operationStarted)
			resolvedAt = time.Now()
		})
		if installErr != nil {
			if progress != nil {
				progress.finish()
			}
			if currentCLIContext().Err() == nil {
				fmt.Fprintln(os.Stderr, "Install failed.")
			}
			return reportError(installErr)
		}
		if progress != nil {
			var result strings.Builder
			renderResolvedTo(&result, planSummary.ResolvedCount, resolutionElapsed)
			if len(packages) == 0 {
				renderInstallNoopTo(&result, planSummary)
			} else {
				renderPackageChangesTo(&result, packages, time.Since(resolvedAt))
			}
			progress.completeWithOutput(result.String())
		} else {
			renderResolved(planSummary.ResolvedCount, resolutionElapsed)
			if len(packages) == 0 {
				renderInstallNoop(planSummary)
			} else {
				renderPackageChanges(packages, time.Since(resolvedAt))
			}
		}
	case "env remove":
		name, version, _ := parsePackageVersionWithLatest(arg, true)
		if strings.EqualFold(version, "latest") {
			targetProfile, profileErr := loadEnvironmentSettings(paths, profile)
			if profileErr != nil {
				return reportError(profileErr)
			}
			if installed, exists := packagesForProfile(db, targetProfile.ProfileName)[normalizePackageName(name)]; exists {
				version = installed.Version
			}
		}
		packages := []PackageTransactionItem{}
		operationStarted := time.Now()
		resolvedAt := operationStarted
		var progress *operationProgress
		removeErr := envRemoveManyForProfileWithProgress(paths, args, profile, func() {
			progress = startOperationProgress("Remove")
			if progress == nil {
				fmt.Println(uvDim("Checking dependent packages..."))
			}
		}, func(resolved []PackageTransactionItem) {
			packages = resolved
			resolvedAt = time.Now()
		})
		if progress != nil {
			if removeErr == nil {
				progress.complete()
			} else {
				progress.finish()
			}
		}
		if removeErr != nil {
			if currentCLIContext().Err() == nil {
				fmt.Fprintln(os.Stderr, "Remove failed.")
			}
			return reportError(removeErr)
		}
		fmt.Printf("%s %s\n", uvDim("Dependency check complete in"), uvEmphasis(formatOperationDuration(time.Since(operationStarted))))
		if len(packages) == 0 {
			if len(args) == 1 {
				fmt.Printf("%s %s from explicit profile requests; files remain required by another package.\n", uvAction("Removed"), uvPackageRef(name, version))
			} else {
				fmt.Printf("%s %d package requests; package files remain required by another package.\n", uvAction("Removed"), len(args))
			}
		} else {
			renderPackageChanges(packages, time.Since(resolvedAt))
		}
	case "env list":
		targetProfile, err := loadEnvironmentSettings(paths, profile)
		if err != nil {
			return reportError(err)
		}
		db, err := loadDatabase(paths.Database)
		if err != nil {
			return reportError(err)
		}
		if err := ensureProfileClosure(paths, &db, targetProfile); err != nil {
			return reportError(err)
		}
		lines := listInstalled(databaseForProfile(db, targetProfile))
		if len(lines) == 0 {
			fmt.Printf("%s\n", uvDim("No packages are installed in profile "+strings.TrimSuffix(targetProfile.ProfileName, ".toml")+"."))
		}
		renderPackageListing(lines)
	case "env freeze", "env unfreeze":
		name, version, _ := parsePackageVersion(arg)
		frozen := action == "freeze"
		if err := setFreezeForProfile(paths, arg, profile, frozen); err != nil {
			return reportError(err)
		}
		verb := "Unfrozen"
		if frozen {
			verb = "Frozen"
		}
		renderSimpleSuccess(verb, name, version)
	case "env frozen":
		targetProfile, err := loadEnvironmentSettings(paths, profile)
		if err != nil {
			return reportError(err)
		}
		db, err := loadDatabase(paths.Database)
		if err != nil {
			return reportError(err)
		}
		if err := ensureProfileClosure(paths, &db, targetProfile); err != nil {
			return reportError(err)
		}
		lines := frozenPackages(databaseForProfile(db, targetProfile))
		if len(lines) == 0 {
			fmt.Println(uvDim("No frozen packages."))
			break
		}
		renderPackageListing(lines)
	case "env update":
		updatePlan := envUpdatePlan{}
		operationStarted := time.Now()
		var progress *operationProgress
		updateErr := updateEnvironmentForProfileWithPlan(paths, arg, profile, func() {
			progress = startOperationProgress("Updates")
			if progress == nil {
				fmt.Println(uvDim("Resolving package updates in the current profile..."))
			}
		}, func(resolved envUpdatePlan) {
			updatePlan = resolved
		})
		if progress != nil {
			if updateErr == nil {
				progress.complete()
			} else {
				progress.finish()
			}
		}
		if updateErr != nil {
			if currentCLIContext().Err() == nil {
				fmt.Fprintln(os.Stderr, "Package update failed.")
			}
			return reportError(updateErr)
		}
		renderEnvironmentUpdateResult(updatePlan, time.Since(operationStarted))
	case "channel install":
		if err := channelInstallMany(paths, args); err != nil {
			return reportError(err)
		}
		for _, repoURL := range args {
			renderActionSuccess("Added channel", deriveChannelName(repoURL), repoURL)
		}
	case "channel remove":
		if err := channelRemoveMany(paths, args); err != nil {
			return reportError(err)
		}
		for _, name := range args {
			renderActionSuccess("Removed channel", name, "")
		}
	case "channel list":
		if len(settings.Channels) == 0 {
			fmt.Println(uvDim("No package channels are configured."))
		}
		for _, entry := range settings.Channels {
			fmt.Printf("%s  %s\n", uvEmphasis(entry.Name), uvDim(entry.URL))
		}
	case "channel fetch":
		channel, ok := repositoryChannel(settings, arg)
		if !ok {
			return reportError(appError("channel_not_found", arg))
		}
		progress := startOperationProgress("Checking channel")
		status, err := fetchChannelStatus(channel)
		if err == nil {
			progress.complete()
		} else {
			progress.finish()
		}
		if err != nil {
			return reportError(err)
		}
		renderChannelFetch(channel, status)
	case "profiles list":
		if err := renderProfiles(paths); err != nil {
			return reportError(err)
		}
	case "profiles install":
		if err := installProfile(paths, arg); err != nil {
			return reportError(err)
		}
		renderActionSuccess("Created profile", arg, "")
	case "profiles remove":
		if err := removeProfile(paths, arg); err != nil {
			return reportError(err)
		}
		renderActionSuccess("Removed profile", arg, "")
	case "profiles generations":
		if err := renderProfileGenerations(paths, arg); err != nil {
			return reportError(err)
		}
	case "profiles rollback":
		var changes []PackageTransactionItem
		var progress *operationProgress
		operationStarted := time.Now()
		err := rollbackProfileWithPlan(paths, arg, args[1], func(resolved []PackageTransactionItem) {
			changes = resolved
			progress = startOperationProgress("Updates")
			if progress == nil {
				fmt.Printf("%s\n", uvDim("Restoring profile generation..."))
			}
		})
		if progress != nil {
			if err == nil {
				progress.complete()
			} else {
				progress.finish()
			}
		}
		if err != nil {
			return reportError(err)
		}
		if len(changes) == 0 {
			renderActionSuccess("Restored profile", arg, "to generation "+args[1]+" (no package changes)")
		} else {
			renderPackageChanges(changes, time.Since(operationStarted))
			renderActionSuccess("Restored profile", arg, "to generation "+args[1])
		}
	case "profiles update":
		updatePlan := packageUpdatePlan{}
		operationStarted := time.Now()
		resolvedAt := operationStarted
		resolutionElapsed := time.Duration(0)
		progress := startOperationProgress("Updates")
		if progress == nil {
			fmt.Printf("%s\n", uvDim("Resolving packages in profile "+profileDisplayName(arg)+"..."))
		}
		updateErr := updateProfileWithPlan(paths, arg, true, func(resolved packageUpdatePlan) {
			updatePlan = resolved
			resolutionElapsed = time.Since(operationStarted)
			resolvedAt = time.Now()
		})
		if updateErr == nil {
			progress.complete()
		} else {
			progress.finish()
		}
		if updateErr != nil {
			if currentCLIContext().Err() == nil {
				fmt.Fprintf(os.Stderr, "Update of profile %s failed.\n", profileDisplayName(arg))
			}
			return reportError(updateErr)
		}
		renderResolved(updatePlan.ResolvedCount, resolutionElapsed)
		var updatedSettings Settings
		if arg == "" {
			updatedSettings, err = loadDefaultSettings(paths)
		} else {
			updatedSettings, err = loadNamedSettings(paths, arg)
		}
		if err != nil {
			return reportError(err)
		}
		if len(updatedSettings.Enabled) == 0 {
			fmt.Printf("%s %s %s\n", uvDim("Profile"), uvEmphasis(profileDisplayName(arg)), uvDim("has no enabled packages to update."))
		} else {
			renderUpdateResult(updatePlan, time.Since(resolvedAt))
		}
	}
	return 0
}

func canonicalEnvAction(action string) string {
	switch action {
	case "i":
		return "install"
	case "r":
		return "remove"
	case "u":
		return "update"
	case "l":
		return "list"
	case "f":
		return "freeze"
	case "uf":
		return "unfreeze"
	case "lf":
		return "frozen"
	default:
		return action
	}
}

func packageNameFromSpec(spec string) string {
	_, name, _, _ := parseRepoPackageSpec(spec)
	return name
}

func profileDisplayName(name string) string {
	if name == "" {
		return "default"
	}
	return name
}

func versionFromSpec(spec string) string {
	_, _, version, _ := parseRepoPackageSpec(spec)
	return version
}

// RunCLI executes the NixNet command-line interface and returns its process exit code.
func RunCLI(argv []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cliContextState.Lock()
	previous := cliContextState.ctx
	cliContextState.ctx = ctx
	cliContextState.Unlock()
	defer func() {
		cliContextState.Lock()
		cliContextState.ctx = previous
		cliContextState.Unlock()
	}()
	code := runCLI(argv)
	if ctx.Err() != nil {
		return reportError(context.Canceled)
	}
	return code
}
