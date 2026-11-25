package nixflux

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const usage = `usage: nixflux [-h] [--version] <group> <action> [argument]

commands:
  version
  env install <repo:name==version>
  env remove <name==version>
  env list
  env freeze <name==version>
  env unfreeze <name==version>
  env frozen
  env update
  set update
  channel install <url>
  channel remove <name>
  channel list
  channel fetch <name>

options:
  -h, --help   show this message and exit
  --version    show version and exit`

var commandArgumentCounts = map[string]int{
	"env install": 1, "env remove": 1, "env list": 0, "env freeze": 1, "env unfreeze": 1, "env frozen": 0, "env update": 0,
	"set update": 0, "channel install": 1, "channel remove": 1, "channel list": 0, "channel fetch": 1,
}

func parseArguments(argv []string) ([]string, bool, error) {
	positional := []string{}
	ended := false
	for _, token := range argv {
		if ended || token == "-" || !strings.HasPrefix(token, "-") {
			positional = append(positional, token)
			continue
		}
		switch token {
		case "--":
			ended = true
		case "-h", "--help":
			fmt.Println(usage)
			return nil, true, nil
		case "--version":
			if err := renderLegacyVersion(os.Stdout); err != nil {
				return nil, false, err
			}
			return nil, true, nil
		default:
			return nil, false, appError("unknown_option", token)
		}
	}
	return positional, false, nil
}

func runCLI(argv []string) int {
	positional, handled, err := parseArguments(argv)
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
	command := group + " " + action
	expected, ok := commandArgumentCounts[command]
	if !ok {
		return reportError(appError("unknown_command", command))
	}
	args := positional[2:]
	if len(args) < expected {
		return reportError(appError("missing_args", ""))
	}
	if len(args) > expected {
		return reportError(appError("too_many_args", strings.Join(args[expected:], " ")))
	}
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	// Validate command arguments before touching configuration or package state.
	switch command {
	case "env install":
		if _, _, _, err := parseRepoPackageSpec(arg); err != nil {
			return reportError(err)
		}
	case "env remove", "env freeze", "env unfreeze":
		if _, _, err := parsePackageVersion(arg); err != nil {
			return reportError(err)
		}
	case "channel install":
		if strings.TrimSpace(arg) == "" {
			return reportError(appError("invalid_config", "channel URL cannot be empty"))
		}
	case "channel remove", "channel fetch":
		if !channelNamePattern.MatchString(arg) {
			return reportError(appError("invalid_config", "invalid channel name"))
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
	if err := migrateLegacyCommandLinks(paths, db, settings); err != nil {
		return reportError(err)
	}
	started := time.Now()
	switch command {
	case "env install":
		before := db
		if err := envInstall(paths, arg); err != nil {
			return reportError(err)
		}
		after, loadErr := loadDatabase(paths.Database)
		if loadErr != nil {
			return reportError(loadErr)
		}
		installedVersion := versionFromSpec(arg)
		if installed, exists := after.Packages[normalizePackageName(packageNameFromSpec(arg))]; exists {
			installedVersion = installed.Version
		}
		renderInstallSuccess(packageNameFromSpec(arg), installedVersion, started, countChangedDependencies(before, after, packageNameFromSpec(arg)))
	case "env remove":
		name, version, _ := parsePackageVersion(arg)
		if err := envRemove(paths, arg); err != nil {
			return reportError(err)
		}
		renderSimpleSuccess("Removed", name, version)
	case "env list":
		db, err := loadDatabase(paths.Database)
		if err != nil {
			return reportError(err)
		}
		for _, line := range listInstalled(db) {
			fmt.Println(line)
		}
	case "env freeze", "env unfreeze":
		name, version, _ := parsePackageVersion(arg)
		frozen := action == "freeze"
		if err := setFreeze(paths, arg, frozen); err != nil {
			return reportError(err)
		}
		verb := "Unfrozen"
		if frozen {
			verb = "Frozen"
		}
		renderSimpleSuccess(verb, name, version)
	case "env frozen":
		db, err := loadDatabase(paths.Database)
		if err != nil {
			return reportError(err)
		}
		lines := frozenPackages(db)
		if len(lines) == 0 {
			fmt.Println("No frozen packages.")
			break
		}
		for _, line := range lines {
			fmt.Println(line)
		}
	case "set update", "env update":
		if err := setUpdate(paths); err != nil {
			return reportError(err)
		}
		if len(settings.Enabled) == 0 {
			fmt.Println("No enabled packages to update.")
		} else {
			fmt.Printf("Updated packages from settings.toml.\nCompleted in %.2fs\n", time.Since(started).Seconds())
		}
	case "channel install":
		if err := channelInstall(paths, arg); err != nil {
			return reportError(err)
		}
		fmt.Printf("Channel %s installed.\n", deriveChannelName(arg))
	case "channel remove":
		if err := channelRemove(paths, arg); err != nil {
			return reportError(err)
		}
		fmt.Printf("Channel %s removed.\n", arg)
	case "channel list":
		for _, entry := range settings.Channels {
			fmt.Printf("%s: %s\n", entry.Name, entry.URL)
		}
	case "channel fetch":
		channel, ok := repositoryChannel(settings, arg)
		if !ok {
			return reportError(appError("channel_not_found", arg))
		}
		status, err := fetchChannelStatus(channel)
		if err != nil {
			return reportError(err)
		}
		renderChannelFetch(channel, status)
	}
	return 0
}

func packageNameFromSpec(spec string) string {
	_, name, _, _ := parseRepoPackageSpec(spec)
	return name
}
func versionFromSpec(spec string) string {
	_, _, version, _ := parseRepoPackageSpec(spec)
	return version
}

// RunCLI executes the NixFlux command-line interface and returns its process exit code.
func RunCLI(argv []string) int {
	return runCLI(argv)
}

func countChangedDependencies(before, after Database, rootName string) int {
	rootKey := normalizePackageName(rootName)
	count := 0
	for key, installed := range after.Packages {
		if key == rootKey {
			continue
		}
		previous, existed := before.Packages[key]
		if !existed || !versionsEqual(previous.Version, installed.Version) {
			count++
		}
	}
	return count
}
