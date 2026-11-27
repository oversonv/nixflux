package nixnet

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

type RepositoryVersion struct {
	URL          string `json:"url"`
	File         string `json:"file"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size,omitempty"`
	Package      string `json:"-"`
	Version      string `json:"-"`
	Architecture string `json:"architecture,omitempty"`
	Dependencies string `json:"dependencies,omitempty"`
	Debian       bool   `json:"-"`
}

type RepositoryIndex struct {
	Packages map[string]map[string]RepositoryVersion `json:"-"`
	Debian   bool                                    `json:"-"`
}

const (
	maxRepositoryPackageSize = int64(256 << 20)
	maxPackagePayloadSize    = int64(512 << 20)
)

func fetchBytes(rawURL string) ([]byte, error) {
	return fetchBytesLimited(rawURL, maxRepositoryPackageSize)
}

func fetchBytesLimited(rawURL string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, appError("request_error", "invalid response size limit")
	}
	origin, err := url.Parse(rawURL)
	if err != nil || origin.Host == "" || (origin.Scheme != "http" && origin.Scheme != "https") {
		return nil, appError("request_error", "invalid HTTP(S) URL")
	}
	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		previous := via[len(via)-1].URL
		if !strings.EqualFold(previous.Host, req.URL.Host) {
			return fmt.Errorf("cross-origin repository redirect was rejected")
		}
		if previous.Scheme == "https" && req.URL.Scheme != "https" {
			return fmt.Errorf("HTTPS downgrade redirect was rejected")
		}
		return nil
	}}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, appError("request_error", err.Error())
	}
	req.Header.Set("User-Agent", "NixNet/0.2 (+Debian APT repository client)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, appError("request_error", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, appError("not_found", rawURL)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, appError("http_error", fmt.Sprintf("HTTP %d for %s", resp.StatusCode, rawURL))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, appError("request_error", err.Error())
	}
	if int64(len(data)) > maxBytes {
		return nil, appError("request_error", "response exceeds size limit")
	}
	return data, nil
}

func loadRepositoryIndex(channel Channel) (RepositoryIndex, error) {
	return loadAPTRepositoryIndex(channel)
}

func resolveRepositoryFile(baseURL, file string) (string, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/") + "/")
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return "", appError("invalid_repository", "invalid repository base URL")
	}
	ref, err := url.Parse(strings.TrimSpace(file))
	if err != nil || ref.IsAbs() || ref.Host != "" || ref.User != nil || ref.RawQuery != "" || ref.Fragment != "" || strings.Contains(ref.Path, "%") || !validRepositoryRelativePath(ref.Path) {
		return "", appError("invalid_repository", "package filename must be a safe repository-relative path")
	}
	resolved := base.ResolveReference(ref)
	if !strings.EqualFold(resolved.Host, base.Host) || resolved.Scheme != base.Scheme || !strings.HasPrefix(resolved.Path, base.Path) {
		return "", appError("invalid_repository", "package URL must remain inside the repository origin and path")
	}
	return resolved.String(), nil
}

func downloadRepositoryPackage(baseURL, packageName, version string, entry RepositoryVersion, source string) (PackageArtifact, error) {
	path := entry.URL
	if path == "" {
		path = entry.File
	}
	if path == "" || entry.SHA256 == "" {
		return PackageArtifact{}, appError("invalid_repository", "package entry requires url/file and sha256")
	}
	if entry.Size > maxRepositoryPackageSize {
		return PackageArtifact{}, appError("invalid_package", "package archive exceeds the 256 MiB download limit")
	}
	artifactURL, err := resolveRepositoryFile(baseURL, path)
	if err != nil {
		return PackageArtifact{}, err
	}
	raw, err := fetchBytesLimited(artifactURL, maxRepositoryPackageSize)
	if err != nil {
		return PackageArtifact{}, err
	}
	if entry.Size > 0 && int64(len(raw)) != entry.Size {
		return PackageArtifact{}, appError("checksum_mismatch", packageName+"=="+version+" (size mismatch)")
	}
	expected := strings.ToLower(strings.TrimSpace(entry.SHA256))
	if len(expected) != 64 {
		return PackageArtifact{}, appError("invalid_repository", "sha256 must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return PackageArtifact{}, appError("invalid_repository", "invalid sha256")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != expected {
		return PackageArtifact{}, appError("checksum_mismatch", packageName+"=="+version)
	}
	storeRaw := raw
	if bytes.HasPrefix(raw, []byte("!<arch>\n")) {
		storeRaw, err = debToZip(raw)
		if err != nil {
			return PackageArtifact{}, appError("invalid_package", err.Error())
		}
	}
	manifest, files, err := inspectPackageArchive(storeRaw)
	if err != nil {
		return PackageArtifact{}, appError("invalid_package", err.Error())
	}
	identityMatches := versionsEqual(manifest.Version, version)
	if entry.Debian {
		if cmp, compareErr := compareDebianVersions(manifest.Version, version); compareErr == nil {
			identityMatches = cmp == 0
		} else {
			identityMatches = false
		}
	}
	if normalizePackageName(manifest.Name) != normalizePackageName(packageName) || !identityMatches {
		return PackageArtifact{}, appError("invalid_package", "archive identity does not match repository index")
	}
	manifestHash, checksumsHash, err := packageMetadataHashes(storeRaw)
	if err != nil {
		return PackageArtifact{}, err
	}
	modes, err := packageFileModes(storeRaw, manifest)
	if err != nil {
		return PackageArtifact{}, appError("invalid_package", err.Error())
	}
	installedSize, err := packagePayloadSize(storeRaw)
	if err != nil {
		return PackageArtifact{}, appError("invalid_package", err.Error())
	}
	return PackageArtifact{Manifest: manifest, Raw: storeRaw, Hash: packageHash(raw), ManifestHash: manifestHash, ChecksumsHash: checksumsHash, Files: files, Modes: modes, Source: source, InstalledSize: installedSize}, nil
}

func packagePayloadSize(raw []byte) (int64, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return 0, err
	}
	var size int64
	for _, file := range reader.File {
		if strings.HasPrefix(file.Name, "payload/") && !file.FileInfo().IsDir() {
			if file.UncompressedSize64 > uint64(maxPackagePayloadSize)-uint64(size) {
				return 0, fmt.Errorf("package payload exceeds the 512 MiB expanded size limit")
			}
			if file.UncompressedSize64 > uint64(^uint64(0)>>1)-uint64(size) {
				return 0, fmt.Errorf("package payload size overflows int64")
			}
			size += int64(file.UncompressedSize64)
		}
	}
	return size, nil
}

func repositoryChannel(settings Settings, name string) (Channel, bool) {
	for _, ch := range settings.Channels {
		if ch.Name == name {
			return ch, true
		}
	}
	return Channel{}, false
}

func repositoryIndexCacheKey(channel Channel) string {
	channel = defaultAPTChannel(channel)
	return strings.Join([]string{channel.Name, channel.URL, channel.Distribution, channel.Component, channel.Architecture}, "\x00")
}

func loadRepositoryIndexCached(channel Channel, cache map[string]RepositoryIndex) (RepositoryIndex, error) {
	if cache != nil {
		key := repositoryIndexCacheKey(channel)
		if index, exists := cache[key]; exists {
			return index, nil
		}
		index, err := loadRepositoryIndex(channel)
		if err != nil {
			return RepositoryIndex{}, err
		}
		cache[key] = index
		return index, nil
	}
	return loadRepositoryIndex(channel)
}

func findRepositoryVersion(settings Settings, requestedName, version, preferred string) (string, Channel, string, RepositoryVersion, error) {
	return findRepositoryVersionCached(settings, requestedName, version, preferred, nil)
}

func findRepositoryVersionCached(settings Settings, requestedName, version, preferred string, cache map[string]RepositoryIndex) (string, Channel, string, RepositoryVersion, error) {
	ordered := orderedChannels(settings, preferred)
	var lastErr error
	for _, ch := range ordered {
		index, err := loadRepositoryIndexCached(ch, cache)
		if err != nil {
			lastErr = err
			continue
		}
		entry, candidate, ok := lookupRepositoryVersion(index.Packages[normalizePackageName(requestedName)], version)
		if ok {
			return ch.Name, ch, candidate, entry, nil
		}
	}
	if lastErr != nil {
		return "", Channel{}, "", RepositoryVersion{}, appError("version_not_found", requestedName+" "+version+"; last repository error: "+lastErr.Error())
	}
	return "", Channel{}, "", RepositoryVersion{}, appError("version_not_found", requestedName+" "+version)
}

func lookupRepositoryVersion(versions map[string]RepositoryVersion, wanted string) (RepositoryVersion, string, bool) {
	if entry, ok := versions[wanted]; ok {
		return entry, wanted, true
	}
	candidates := make([]string, 0, len(versions))
	for candidate := range versions {
		candidates = append(candidates, candidate)
	}
	sortStrings(candidates)
	for _, candidate := range candidates {
		if versionsEqual(candidate, wanted) {
			return versions[candidate], candidate, true
		}
	}
	return RepositoryVersion{}, "", false
}

func latestRepositoryVersion(versions map[string]RepositoryVersion) (RepositoryVersion, string, bool) {
	if len(versions) == 0 {
		return RepositoryVersion{}, "", false
	}
	versionNames := make([]string, 0, len(versions))
	for version := range versions {
		versionNames = append(versionNames, version)
	}
	best := versionNames[0]
	for _, candidate := range versionNames[1:] {
		var cmp int
		var err error
		if versions[candidate].Debian || versions[best].Debian {
			cmp, err = compareDebianVersions(candidate, best)
		} else {
			cmp, err = compareVersions(candidate, best)
		}
		if err == nil && cmp > 0 {
			best = candidate
		}
	}
	return versions[best], best, true
}

func orderedChannels(settings Settings, preferred string) []Channel {
	ordered := make([]Channel, 0, len(settings.Channels))
	if preferred != "" {
		if ch, ok := repositoryChannel(settings, preferred); ok {
			ordered = append(ordered, ch)
		}
	}
	for _, ch := range settings.Channels {
		if ch.Name != preferred {
			ordered = append(ordered, ch)
		}
	}
	return ordered
}

func selectRepositoryVersion(settings Settings, name, constraint, preferred string) (string, Channel, string, RepositoryVersion, error) {
	return selectRepositoryVersionCached(settings, name, constraint, preferred, nil)
}

func selectRepositoryVersionCached(settings Settings, name, constraint, preferred string, cache map[string]RepositoryIndex) (string, Channel, string, RepositoryVersion, error) {
	type candidate struct {
		repo    string
		channel Channel
		version string
		entry   RepositoryVersion
	}
	var stable, prerelease []candidate
	for _, ch := range orderedChannels(settings, preferred) {
		index, err := loadRepositoryIndexCached(ch, cache)
		if err != nil {
			continue
		}
		versions := index.Packages[normalizePackageName(name)]
		versionNames := make([]string, 0, len(versions))
		for version := range versions {
			versionNames = append(versionNames, version)
		}
		sortStrings(versionNames)
		for _, version := range versionNames {
			ok, err := versionSatisfies(version, constraint)
			if err != nil || !ok {
				continue
			}
			item := candidate{repo: ch.Name, channel: ch, version: version, entry: versions[version]}
			if !item.entry.Debian && isPEP440Prerelease(version) {
				prerelease = append(prerelease, item)
			} else {
				stable = append(stable, item)
			}
		}
	}
	candidates := stable
	if specifierRequestsPrerelease(constraint) {
		candidates = append(append([]candidate{}, stable...), prerelease...)
	} else if len(candidates) == 0 {
		candidates = prerelease
	}
	if len(candidates) == 0 {
		return "", Channel{}, "", RepositoryVersion{}, appError("dependency_not_found", name+constraint)
	}
	selected := candidates[0]
	for _, item := range candidates[1:] {
		var cmp int
		var err error
		if item.entry.Debian || selected.entry.Debian {
			cmp, err = compareDebianVersions(item.version, selected.version)
		} else {
			cmp, err = compareVersions(item.version, selected.version)
		}
		if err == nil && cmp > 0 {
			selected = item
		}
	}
	return selected.repo, selected.channel, selected.version, selected.entry, nil
}

func specifierRequestsPrerelease(constraints string) bool {
	for _, part := range strings.Split(constraints, ",") {
		part = strings.TrimSpace(part)
		for _, op := range []string{">=", "<=", "===", "==", "!=", "~=", ">>", "<<", ">", "<"} {
			if strings.HasPrefix(part, op) {
				wanted := strings.TrimSpace(strings.TrimPrefix(part, op))
				if parsed := parsePEP440(wanted); parsed.valid && (parsed.preRank < 3 || parsed.dev != nil) {
					return true
				}
				break
			}
		}
	}
	return false
}

func versionSatisfies(version, constraints string) (bool, error) {
	if strings.TrimSpace(constraints) == "" {
		return true, nil
	}
	for _, part := range strings.Split(constraints, ",") {
		part = strings.TrimSpace(part)
		op := ""
		for _, candidate := range []string{">=", "<=", "===", "==", "!=", "~=", ">>", "<<", ">", "<"} {
			if strings.HasPrefix(part, candidate) {
				op = candidate
				break
			}
		}
		if op == "" {
			return false, fmt.Errorf("unsupported version constraint %q", part)
		}
		wanted := strings.TrimSpace(strings.TrimPrefix(part, op))
		if wanted == "" {
			return false, fmt.Errorf("missing version in constraint %q", part)
		}
		if op == "===" {
			if !strings.EqualFold(version, wanted) {
				return false, nil
			}
			continue
		}

		if strings.HasSuffix(wanted, ".*") {
			if op != "==" && op != "!=" {
				return false, fmt.Errorf("wildcard versions are only valid with == and !=")
			}
			prefix := parsePEP440(strings.TrimSuffix(wanted, ".*"))
			if !prefix.valid || prefix.preRank != 3 || prefix.post != nil || prefix.dev != nil || len(prefix.local) != 0 {
				return false, fmt.Errorf("invalid PEP 440 wildcard version %q", wanted)
			}
			matched, err := versionMatchesReleasePrefix(version, prefix, len(prefix.release))
			if err != nil {
				return false, err
			}
			if op == "!=" {
				matched = !matched
			}
			if !matched {
				return false, nil
			}
			continue
		}

		candidateVersion := parsePEP440(version)
		wantedVersion := parsePEP440(wanted)
		if !candidateVersion.valid || !wantedVersion.valid || strings.ContainsAny(version+wanted, ":~-") || op == ">>" || op == "<<" {
			return compareVersionConstraint(version, wanted, op)
		}
		if op != "==" && op != "!=" && len(wantedVersion.local) != 0 {
			return false, fmt.Errorf("local versions are not valid with %s", op)
		}
		if len(wantedVersion.local) == 0 {
			candidateVersion.local = nil
		}
		cmp := compareParsedPEP440(candidateVersion, wantedVersion)
		pass := false
		switch op {
		case "==", "=":
			pass = cmp == 0
		case "!=":
			pass = cmp != 0
		case ">=":
			pass = cmp >= 0
		case "<=":
			pass = cmp <= 0
		case ">", ">>":
			pass = cmp > 0
			if pass && candidateVersion.post != nil && wantedVersion.post == nil && sameRelease(candidateVersion, wantedVersion) && candidateVersion.preRank == wantedVersion.preRank && compareNumericStrings(candidateVersion.preNum, wantedVersion.preNum) == 0 {
				pass = false
			}
		case "<", "<<":
			pass = cmp < 0
			if pass && (candidateVersion.preRank < 3 || candidateVersion.dev != nil) && wantedVersion.preRank == 3 && wantedVersion.dev == nil && sameRelease(candidateVersion, wantedVersion) {
				pass = false
			}
		case "~=":
			if len(wantedVersion.release) < 2 {
				return false, fmt.Errorf("~= requires at least two release components")
			}
			if cmp >= 0 {
				pass, _ = versionMatchesReleasePrefix(version, wantedVersion, len(wantedVersion.release)-1)
			}
		}
		if !pass {
			return false, nil
		}
	}
	return true, nil
}

func packageMetadataHashes(raw []byte) (string, string, error) {
	if bytes.HasPrefix(raw, []byte("!<arch>\n")) {
		converted, err := debToZip(raw)
		if err != nil {
			return "", "", err
		}
		raw = converted
	}
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", "", err
	}
	var manifest, checksums []byte
	for _, file := range reader.File {
		switch filepath.ToSlash(file.Name) {
		case "package.toml":
			manifest, err = readZipFile(file)
		case "checksums.sha256":
			checksums, err = readZipFile(file)
		}
		if err != nil {
			return "", "", err
		}
	}
	if manifest == nil || checksums == nil {
		return "", "", fmt.Errorf("package metadata files are missing")
	}
	manifestSum := sha256.Sum256(manifest)
	checksumsSum := sha256.Sum256(checksums)
	return hex.EncodeToString(manifestSum[:]), hex.EncodeToString(checksumsSum[:]), nil
}
