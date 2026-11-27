package nixnet

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxAPTIndexCompressed   = int64(128 << 20)
	maxAPTIndexDecompressed = int64(128 << 20)
	maxAPTReleaseAge        = 7 * 24 * time.Hour
)

type aptRelease struct {
	Architectures []string
	Components    []string
	Date          time.Time
	ValidUntil    time.Time
	SHA256        map[string]aptIndexDigest
}

type aptIndexDigest struct {
	SHA256 string
	Size   int64
}

func defaultAPTChannel(channel Channel) Channel {
	parsed, _ := url.Parse(channel.URL)
	path := strings.TrimRight(parsed.Path, "/")
	if channel.Distribution == "" || channel.Component == "" {
		switch {
		case strings.HasSuffix(path, "/termux-main"):
			if channel.Distribution == "" {
				channel.Distribution = "stable"
			}
			if channel.Component == "" {
				channel.Component = "main"
			}
		case strings.HasSuffix(path, "/termux-root"):
			if channel.Distribution == "" {
				channel.Distribution = "root"
			}
			if channel.Component == "" {
				channel.Component = "stable"
			}
		case strings.HasSuffix(path, "/termux-x11"):
			if channel.Distribution == "" {
				channel.Distribution = "x11"
			}
			if channel.Component == "" {
				channel.Component = "main"
			}
		}
	}
	if channel.Distribution == "" {
		channel.Distribution = "stable"
	}
	if channel.Component == "" {
		channel.Component = "main"
	}
	if channel.Architecture == "" {
		channel.Architecture = runtimeDebianArchitecture()
	}
	return channel
}

func runtimeDebianArchitecture() string {
	if runtimeGOOS == "android" {
		switch runtimeGOARCH {
		case "arm64":
			return "aarch64"
		case "arm":
			return "arm"
		case "386":
			return "i686"
		case "amd64":
			return "x86_64"
		}
	}
	switch runtimeGOARCH {
	case "arm64":
		return "arm64"
	case "arm":
		return "armhf"
	case "386":
		return "i386"
	case "amd64":
		return "amd64"
	case "ppc64le":
		return "ppc64el"
	case "riscv64":
		return "riscv64"
	case "s390x":
		return "s390x"
	default:
		return runtimeGOARCH
	}
}

func loadAPTRepositoryIndex(channel Channel) (RepositoryIndex, error) {
	channel = defaultAPTChannel(channel)
	if channel.URL == "" || !validAPTToken(channel.Distribution) || !validAPTToken(channel.Component) || !validDebianArchitecture(channel.Architecture) {
		return RepositoryIndex{}, appError("invalid_repository", "APT channel has an invalid URL, distribution, component, or architecture")
	}
	base, err := url.Parse(strings.TrimRight(channel.URL, "/") + "/")
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return RepositoryIndex{}, appError("invalid_repository", "invalid APT repository URL")
	}

	releasePath := "dists/" + channel.Distribution + "/Release"
	releaseURL := base.ResolveReference(&url.URL{Path: releasePath}).String()
	releaseRaw, err := fetchBytesLimited(releaseURL, 8<<20)
	if err != nil {
		return RepositoryIndex{}, err
	}
	signatureURL := releaseURL + ".gpg"
	signatureRaw, err := fetchBytesLimited(signatureURL, 1<<20)
	if err != nil {
		return RepositoryIndex{}, appError("repository_signature", "cannot fetch detached Release signature: "+err.Error())
	}
	if err := verifyAPTReleaseSignature(releaseRaw, signatureRaw, channel); err != nil {
		return RepositoryIndex{}, appError("repository_signature", err.Error())
	}
	release, err := parseAPTRelease(string(releaseRaw))
	if err != nil {
		return RepositoryIndex{}, appError("invalid_repository", "invalid Release metadata: "+err.Error())
	}
	if err := validateAPTReleaseFreshness(release, time.Now()); err != nil {
		return RepositoryIndex{}, appError("invalid_repository", err.Error())
	}
	if !containsString(release.Components, channel.Component) && len(release.Components) > 0 {
		return RepositoryIndex{}, appError("invalid_repository", "component "+channel.Component+" is not published by this distribution")
	}
	if len(release.Architectures) > 0 && !containsString(release.Architectures, channel.Architecture) {
		return RepositoryIndex{}, appError("unsupported_architecture", channel.Architecture)
	}

	architectures := []string{channel.Architecture}
	if channel.Architecture != "all" {
		architectures = append(architectures, "all")
	}
	result := RepositoryIndex{Packages: map[string]map[string]RepositoryVersion{}, Debian: true}
	loadedAny := false
	loadedNative := false
	for _, arch := range architectures {
		prefix := channel.Component + "/binary-" + arch + "/Packages"
		indexRaw, err := fetchAPTIndexAt(base, channel.Distribution, prefix, release)
		if err != nil {
			if isApplicationError(err, "not_found") && arch == "all" {
				continue
			}
			if isApplicationError(err, "not_found") && arch == channel.Architecture {
				return RepositoryIndex{}, appError("not_found", "APT Packages index for architecture "+arch)
			}
			if isApplicationError(err, "not_found") && arch == "all" {
				continue
			}
			return RepositoryIndex{}, err
		}
		loadedAny = true
		if arch == channel.Architecture {
			loadedNative = true
		}
		entries, err := parseAPT_Packages(indexRaw)
		if err != nil {
			return RepositoryIndex{}, appError("invalid_repository", "invalid Packages index: "+err.Error())
		}
		for _, item := range entries {
			if item.Architecture != "all" && item.Architecture != channel.Architecture {
				continue
			}
			key := normalizePackageName(item.Package)
			if result.Packages[key] == nil {
				result.Packages[key] = map[string]RepositoryVersion{}
			}
			if previous, exists := result.Packages[key][item.Version]; exists {
				if previous.File != item.File || previous.SHA256 != item.SHA256 {
					return RepositoryIndex{}, appError("invalid_repository", "conflicting duplicate package entry: "+item.Package+"=="+item.Version)
				}
				continue
			}
			result.Packages[key][item.Version] = item
		}
	}
	if !loadedAny || !loadedNative {
		return RepositoryIndex{}, appError("not_found", "APT Packages indexes for "+channel.Architecture)
	}
	return result, nil
}

func verifyAPTReleaseSignature(release, signature []byte, channel Channel) error {
	if len(release) == 0 || len(signature) == 0 {
		return fmt.Errorf("Release metadata or detached signature is empty")
	}
	gpgv, err := exec.LookPath("gpgv")
	if err != nil {
		return fmt.Errorf("gpgv is required to verify APT repository metadata")
	}
	keyrings, err := aptTrustedKeyrings()
	if err != nil {
		return err
	}
	if len(keyrings) == 0 {
		return fmt.Errorf("no trusted APT keyrings found; install the repository keyring or set NIXNET_KEYRING (NIXFLUX_KEYRING is also supported)")
	}
	tempDir, err := os.MkdirTemp("", "nixnet-apt-verify-")
	if err != nil {
		return fmt.Errorf("cannot create signature verification workspace: %w", err)
	}
	defer os.RemoveAll(tempDir)
	releasePath := filepath.Join(tempDir, "Release")
	signaturePath := filepath.Join(tempDir, "Release.gpg")
	if err := os.WriteFile(releasePath, release, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(signaturePath, signature, 0o600); err != nil {
		return err
	}
	args := []string{"--status-fd=1"}
	for _, keyring := range keyrings {
		args = append(args, "--keyring", keyring)
	}
	args = append(args, signaturePath, releasePath)
	cmd := exec.Command(gpgv, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("APT Release signature verification failed: %s", message)
	}
	return nil
}

func aptTrustedKeyrings() ([]string, error) {
	var patterns []string
	customKeyring := strings.TrimSpace(os.Getenv("NIXNET_KEYRING"))
	if customKeyring == "" {
		customKeyring = strings.TrimSpace(os.Getenv("NIXFLUX_KEYRING"))
	}
	if custom := customKeyring; custom != "" {
		patterns = append(patterns, custom)
	} else {
		prefix := strings.TrimSpace(os.Getenv("PREFIX"))
		if runtimeGOOS == "android" {
			if prefix == "" {
				prefix = "/data/data/com.termux/files/usr"
			}
			patterns = append(patterns,
				filepath.Join(prefix, "share", "termux-keyring"),
				filepath.Join(prefix, "etc", "apt", "trusted.gpg.d"),
				filepath.Join(prefix, "etc", "apt", "trusted.gpg"),
			)
		} else {
			patterns = append(patterns,
				"/etc/apt/trusted.gpg.d",
				"/etc/apt/trusted.gpg",
				"/usr/share/keyrings",
			)
		}
	}

	keyrings := []string{}
	seen := map[string]bool{}
	for _, candidate := range patterns {
		matches := []string{}
		info, statErr := os.Stat(candidate)
		if statErr == nil && info.IsDir() {
			matches, _ = filepath.Glob(filepath.Join(candidate, "*.gpg"))
		} else if statErr == nil && info.Mode().IsRegular() {
			matches = []string{candidate}
		} else {
			var globErr error
			matches, globErr = filepath.Glob(candidate)
			if globErr != nil {
				return nil, fmt.Errorf("invalid keyring path %q: %w", candidate, globErr)
			}
		}
		for _, match := range matches {
			resolved, err := filepath.Abs(match)
			if err != nil || seen[resolved] {
				continue
			}
			fileInfo, err := os.Stat(match)
			if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Size() == 0 {
				continue
			}
			seen[resolved] = true
			keyrings = append(keyrings, match)
		}
	}
	return keyrings, nil
}

func fetchAPTIndexAt(base *url.URL, distribution, prefix string, release aptRelease) ([]byte, error) {
	candidates := []struct {
		suffix string
		kind   string
	}{
		{".gz", "gz"},
		{".bz2", "bz2"},
		{"", "plain"},
		{".xz", "xz"},
	}
	var lastErr error
	for _, candidate := range candidates {
		relative := prefix + candidate.suffix
		digest, listed := release.SHA256[relative]
		if !listed {
			continue
		}
		indexURL := base.ResolveReference(&url.URL{Path: "dists/" + distribution + "/" + relative}).String()
		raw, err := fetchBytesLimited(indexURL, maxAPTIndexCompressed)
		if err != nil {
			lastErr = err
			if isApplicationError(err, "not_found") {
				continue
			}
			return nil, err
		}
		if int64(len(raw)) != digest.Size {
			return nil, appError("invalid_repository", "Packages index size does not match Release metadata: "+relative)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != digest.SHA256 {
			return nil, appError("checksum_mismatch", "APT index "+relative)
		}
		decoded, err := decompressAPTIndex(raw, candidate.kind)
		if err != nil {
			return nil, appError("invalid_repository", "cannot decompress "+relative+": "+err.Error())
		}
		return decoded, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, appError("not_found", "APT Packages index "+prefix)
}

func decompressAPTIndex(raw []byte, kind string) ([]byte, error) {
	var reader io.Reader
	switch kind {
	case "plain":
		reader = bytes.NewReader(raw)
	case "gz":
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	case "bz2":
		reader = bzip2.NewReader(bytes.NewReader(raw))
	case "xz":
		return runXZDecompress(raw, maxAPTIndexDecompressed)
	default:
		return nil, fmt.Errorf("unsupported compression %q", kind)
	}
	output, err := io.ReadAll(io.LimitReader(reader, maxAPTIndexDecompressed+1))
	if err != nil {
		return nil, err
	}
	if int64(len(output)) > maxAPTIndexDecompressed {
		return nil, fmt.Errorf("decompressed Packages index exceeds size limit")
	}
	return output, nil
}

func parseAPTRelease(raw string) (aptRelease, error) {
	fields, err := parseControlFieldsStrict(raw)
	if err != nil {
		return aptRelease{}, err
	}
	result := aptRelease{
		Architectures: strings.Fields(fields["architectures"]),
		Components:    strings.Fields(fields["components"]),
		SHA256:        map[string]aptIndexDigest{},
	}
	for fieldName, destination := range map[string]*time.Time{"date": &result.Date, "valid-until": &result.ValidUntil} {
		value := strings.TrimSpace(fields[fieldName])
		if value == "" {
			continue
		}
		parsed, parseErr := time.Parse(time.RFC1123, value)
		if parseErr != nil {
			return aptRelease{}, fmt.Errorf("invalid %s timestamp: %w", fieldName, parseErr)
		}
		*destination = parsed
	}
	if !result.Date.IsZero() && !result.ValidUntil.IsZero() && result.ValidUntil.Before(result.Date) {
		return aptRelease{}, fmt.Errorf("Valid-Until precedes Date")
	}
	if len(result.Architectures) == 0 || len(result.Components) == 0 {
		return aptRelease{}, fmt.Errorf("Release metadata must declare Architectures and Components")
	}
	for _, arch := range result.Architectures {
		if !validDebianArchitecture(arch) {
			return aptRelease{}, fmt.Errorf("invalid architecture in Release metadata: %q", arch)
		}
	}
	for _, component := range result.Components {
		if !validAPTToken(component) {
			return aptRelease{}, fmt.Errorf("invalid component in Release metadata: %q", component)
		}
	}
	rawSHA := fields["sha256"]
	if rawSHA != "" {
		for _, line := range strings.Split(rawSHA, "\n") {
			parts := strings.Fields(line)
			if len(parts) == 0 {
				continue
			}
			if len(parts) != 3 {
				return aptRelease{}, fmt.Errorf("invalid SHA256 entry")
			}
			size, parseErr := strconv.ParseInt(parts[1], 10, 64)
			if parseErr != nil || size <= 0 || len(parts[0]) != 64 || !validRepositoryRelativePath(parts[2]) {
				return aptRelease{}, fmt.Errorf("invalid SHA256 entry")
			}
			if _, decodeErr := hex.DecodeString(parts[0]); decodeErr != nil {
				return aptRelease{}, fmt.Errorf("invalid SHA256 digest")
			}
			if _, duplicate := result.SHA256[parts[2]]; duplicate {
				return aptRelease{}, fmt.Errorf("duplicate SHA256 path %q", parts[2])
			}
			result.SHA256[parts[2]] = aptIndexDigest{SHA256: strings.ToLower(parts[0]), Size: size}
		}
	}
	if len(result.SHA256) == 0 {
		return aptRelease{}, fmt.Errorf("Release metadata has no SHA256 index hashes")
	}
	return result, nil
}

func validateAPTReleaseFreshness(release aptRelease, now time.Time) error {
	if release.Date.IsZero() {
		return fmt.Errorf("APT Release metadata is missing its signed Date")
	}
	if release.Date.After(now.Add(10 * time.Minute)) {
		return fmt.Errorf("APT Release metadata Date is too far in the future")
	}
	if now.Sub(release.Date) > maxAPTReleaseAge {
		return fmt.Errorf("APT Release metadata is older than the maximum age of %s", maxAPTReleaseAge)
	}
	if !release.ValidUntil.IsZero() && !now.Before(release.ValidUntil) {
		return fmt.Errorf("APT Release metadata has expired")
	}
	return nil
}

func parseAPT_Packages(raw []byte) ([]RepositoryVersion, error) {
	paragraphs := splitControlParagraphs(string(raw))
	result := make([]RepositoryVersion, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		fields, fieldErr := parseControlFieldsStrict(paragraph)
		if fieldErr != nil {
			return nil, fieldErr
		}
		name := fields["package"]
		version := fields["version"]
		architecture := fields["architecture"]
		filename := fields["filename"]
		sha := strings.ToLower(strings.TrimSpace(fields["sha256"]))
		if name == "" && version == "" && filename == "" {
			continue
		}
		if !isValidPackageName(name) || !parseDebianVersion(version).valid {
			return nil, fmt.Errorf("invalid Package or Version field: %q %q", name, version)
		}
		if architecture == "" || !validDebianArchitecture(architecture) {
			return nil, fmt.Errorf("invalid Architecture field for %s", name)
		}
		if !validRepositoryRelativePath(filename) {
			return nil, fmt.Errorf("invalid Filename field for %s", name)
		}
		size, sizeErr := strconv.ParseInt(strings.TrimSpace(fields["size"]), 10, 64)
		if sizeErr != nil || size <= 0 {
			return nil, fmt.Errorf("missing or invalid Size field for %s", name)
		}
		if len(sha) != 64 {
			return nil, fmt.Errorf("missing or invalid SHA256 field for %s", name)
		}
		if _, err := hex.DecodeString(sha); err != nil {
			return nil, fmt.Errorf("invalid SHA256 field for %s", name)
		}
		result = append(result, RepositoryVersion{
			File: filename, SHA256: sha, Size: size,
			Architecture: architecture, Dependencies: combineDependencyFields(fields["pre-depends"], fields["depends"]),
			Debian: true,
		})
		// Package names are held in the map key; keep it alongside this record
		// in an unexported side field until the caller adds the entry.
		result[len(result)-1].Package = name
		result[len(result)-1].Version = version
	}
	return result, nil
}

func combineDependencyFields(preDepends, depends string) string {
	values := []string{}
	if strings.TrimSpace(preDepends) != "" {
		values = append(values, preDepends)
	}
	if strings.TrimSpace(depends) != "" {
		values = append(values, depends)
	}
	return strings.Join(values, ", ")
}

func validAPTToken(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("+.-_", ch)) {
			return false
		}
	}
	return true
}

func parseControlFieldsStrict(raw string) (map[string]string, error) {
	fields := map[string]string{}
	current := ""
	for lineNumber, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if current == "" {
				return nil, fmt.Errorf("orphan continuation on line %d", lineNumber+1)
			}
			fields[current] += "\n" + strings.TrimLeft(line, " \t")
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || !validControlFieldName(key) {
			return nil, fmt.Errorf("invalid control field on line %d", lineNumber+1)
		}
		current = strings.ToLower(key)
		if _, exists := fields[current]; exists {
			return nil, fmt.Errorf("duplicate control field %q", key)
		}
		fields[current] = strings.TrimSpace(value)
	}
	return fields, nil
}

func validControlFieldName(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if !(ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			return false
		}
	}
	return true
}

func splitControlParagraphs(raw string) []string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	paragraphs := []string{}
	var current []string
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if len(current) > 0 {
				paragraphs = append(paragraphs, strings.Join(current, "\n"))
				current = nil
			}
			continue
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		paragraphs = append(paragraphs, strings.Join(current, "\n"))
	}
	return paragraphs
}

func validDebianArchitecture(value string) bool {
	if value == "all" {
		return true
	}
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validRepositoryRelativePath(value string) bool {
	if value == "" || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func isApplicationError(err error, code string) bool {
	var typed *applicationError
	return errors.As(err, &typed) && typed.code == code
}
