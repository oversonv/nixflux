package nixflux

import (
	"regexp"
	"strings"
)

var packageNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9+._-]*[A-Za-z0-9+])?$`)

func isValidPackageName(name string) bool { return packageNamePattern.MatchString(name) }
func validatePackageName(name string) error {
	if !isValidPackageName(name) {
		return appError("invalid_package_name", name)
	}
	return nil
}
func validateVersion(version string) error {
	if !isValidVersion(version) {
		return appError("invalid_version", version)
	}
	return nil
}
func normalizePackageName(name string) string {
	return strings.ToLower(regexp.MustCompile(`[-_.]+`).ReplaceAllString(name, "-"))
}
func parseRepoPackageSpec(spec string) (string, string, string, error) {
	repo, rest, found := strings.Cut(spec, ":")
	if !found || repo == "" || rest == "" {
		return "", "", "", appError("invalid_env_install_format", spec)
	}
	name, version, has := strings.Cut(rest, "==")
	if name == "" || !has || version == "" {
		return "", "", "", appError("invalid_env_install_format", spec)
	}
	if err := validatePackageName(name); err != nil {
		return "", "", "", err
	}
	if !strings.EqualFold(version, "latest") {
		if err := validateVersion(version); err != nil {
			return "", "", "", err
		}
	}
	return repo, name, version, nil
}
func parsePackageVersion(spec string) (string, string, error) {
	return parsePackageVersionWithLatest(spec, false)
}

func parsePackageVersionWithLatest(spec string, allowLatest bool) (string, string, error) {
	name, version, found := strings.Cut(spec, "==")
	if !found || name == "" || version == "" {
		return "", "", appError("invalid_env_remove_format", spec)
	}
	if err := validatePackageName(name); err != nil {
		return "", "", err
	}
	if !(allowLatest && strings.EqualFold(version, "latest")) {
		if err := validateVersion(version); err != nil {
			return "", "", err
		}
	}
	return name, version, nil
}
