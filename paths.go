package nixnet

import (
	"os"
	"path/filepath"
)

type AppPaths struct {
	Root           string
	Store          string
	Bin            string
	Set            string
	Settings       string
	Channels       string
	Profiles       string
	ProfilesLock   string
	CurrentProfile string
	Database       string
	Transaction    string
	Staging        string
	Lock           string
	LegacyChannels string
	LegacyBin      string
}

func fetchPathHome() (string, error) { return os.UserHomeDir() }

func pathsForHome(home string) AppPaths {
	return pathsForHomeForPlatform(home, runtimeGOOS, os.Getenv("PREFIX"))
}

func pathsForHomeForPlatform(home, goos, prefix string) AppPaths {
	root := filepath.Join(home, "nix")
	set := filepath.Join(root, "set")
	legacyBin := filepath.Join(root, "bin")
	bin := legacyBin
	if goos == "android" {
		bin = ""
		if filepath.IsAbs(prefix) {
			bin = filepath.Join(filepath.Clean(prefix), "bin")
		}
	}
	return AppPaths{
		Root:           root,
		Store:          filepath.Join(root, "store"),
		Bin:            bin,
		Set:            set,
		Settings:       filepath.Join(set, "settings.toml"),
		Channels:       filepath.Join(set, "channels.json"),
		Profiles:       filepath.Join(root, "profiles"),
		ProfilesLock:   filepath.Join(root, "profiles", "profiles.lock"),
		CurrentProfile: filepath.Join(root, "profiles", ".current"),
		Database:       filepath.Join(set, "store.json"),
		Transaction:    filepath.Join(set, "transaction.json"),
		Staging:        filepath.Join(root, "staging"),
		// Keep the historical lock path so old NixFlux and new NixNet
		// processes still serialize against the same runtime state.
		Lock:           filepath.Join(set, "nixflux.lock"),
		LegacyChannels: filepath.Join(set, "channels.json"),
		LegacyBin:      legacyBin,
	}
}
