package nixflux

import (
	"fmt"
	"os"
)

type applicationError struct {
	code   string
	detail string
}

var errorMessages = map[string]string{
	"missing_args": "Missing required CLI arguments.", "too_many_args": "Too many CLI arguments.", "unknown_option": "Unknown option.", "unknown_command": "Unknown command or subcommand.",
	"invalid_env_install_format": "Package must be specified as <repo:name==version>.", "invalid_env_remove_format": "Package must be specified as <name==version>.", "invalid_package_name": "Package name does not conform to PEP 508.", "invalid_version": "Version does not conform to Debian or PEP 440 syntax.",
	"channel_not_found": "Channel was not found in settings.toml.", "channel_already_exists": "Channel with this name already exists.", "package_not_installed": "Matching package in store was not found.", "package_not_frozen": "Package is not frozen.", "version_not_found": "Requested version was not found in repository.",
	"timeout": "The site has timed out.", "request_error": "Network error occurred while connecting to repository.", "not_found": "Requested resource was not found in repository.", "http_error": "HTTP error occurred while fetching repository data.", "invalid_json": "Failed to parse JSON response.", "io_error": "Failed to read or write files or directories.",
	"invalid_config": "settings.toml is invalid.", "invalid_database": "Installed package database is invalid or unsupported.", "invalid_repository": "Repository index is invalid.", "invalid_package": "Package archive is invalid.", "checksum_mismatch": "Package integrity verification failed.", "binary_conflict": "A binary with this name is already owned by another file or package.", "package_frozen": "Package is frozen and cannot change versions.", "downgrade_requires_set_update": "Downgrade is only allowed through 'set update'.", "dependency_not_found": "A required dependency was not found.", "dependency_conflict": "Installed packages have conflicting dependency requirements.", "dependency_cycle": "A circular dependency was detected.", "package_required": "Package is required by another installed package.", "store_corrupt": "Installed package data failed an integrity check.",
	"transaction_failed": "Package operation failed and its transaction was rolled back.",
}
var usageErrorCodes = map[string]bool{"missing_args": true, "too_many_args": true, "unknown_option": true, "unknown_command": true, "invalid_env_install_format": true, "invalid_env_remove_format": true, "invalid_package_name": true, "invalid_version": true}

func appError(code, detail string) error { return &applicationError{code: code, detail: detail} }
func (e *applicationError) Error() string {
	message, ok := errorMessages[e.code]
	if !ok {
		message = "Unknown error occurred."
	}
	if e.detail != "" {
		return fmt.Sprintf("%s (%s)", message, e.detail)
	}
	return message
}
func reportError(err error) int {
	if appErr, ok := err.(*applicationError); ok {
		fmt.Fprintln(os.Stderr, appErr.Error())
		if usageErrorCodes[appErr.code] {
			return 2
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, err.Error())
	return 1
}
