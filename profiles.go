package nixflux

import (
	"fmt"
	"os"
	"path/filepath"
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
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		return appError("profile_exists", name)
	}
	if err != nil {
		return appError("io_error", err.Error())
	}
	if _, err := file.WriteString(encodeProfileTOML(nil)); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return appError("io_error", err.Error())
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return appError("io_error", err.Error())
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return appError("io_error", err.Error())
	}
	syncDirectory(filepath.Dir(path))
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

	if currentProfile == filename {
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
		if len(remaining) > 0 {
			if err := commitPackagePlan(paths, db, newPackagePlan(), active, active, active, next, true); err != nil {
				return err
			}
		} else if err := reconcileCommandLinks(paths, db, next); err != nil {
			return err
		}
	}

	if newDefault == "" {
		if err := os.Remove(paths.ProfilesLock); err != nil && !os.IsNotExist(err) {
			return appError("io_error", err.Error())
		}
	} else if err := writeProfileLock(paths, newDefault); err != nil {
		return err
	}
	if newCurrent == "" {
		if err := os.Remove(paths.CurrentProfile); err != nil && !os.IsNotExist(err) {
			return appError("io_error", err.Error())
		}
	} else if currentProfile == filename {
		if err := atomicWriteFile(paths.CurrentProfile, []byte(newCurrent+"\n"), 0o600); err != nil {
			return appError("io_error", err.Error())
		}
	}
	db, err := loadDatabase(paths.Database)
	if err != nil {
		return err
	}
	delete(db.ProfileClosures, filename)
	delete(db.ProfileFrozen, filename)
	if err := saveDatabase(paths.Database, db); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return appError("io_error", err.Error())
	}
	syncDirectory(paths.Profiles)
	return nil
}

func renderProfiles(paths AppPaths) error {
	profiles, defaultProfile, err := listProfiles(paths)
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		fmt.Println("No profiles.")
		return nil
	}
	for _, filename := range profiles {
		name := strings.TrimSuffix(filename, ".toml")
		if filename == defaultProfile {
			fmt.Printf("%s (default)\n", name)
		} else {
			fmt.Println(name)
		}
	}
	return nil
}
