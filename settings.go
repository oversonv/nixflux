package nixflux

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Channel struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	Distribution string `json:"distribution,omitempty"`
	Component    string `json:"component,omitempty"`
	Architecture string `json:"architecture,omitempty"`
}

type Settings struct {
	Channels    []Channel `json:"channels"`
	Enabled     []string  `json:"enabled"`
	ProfileName string    `json:"-"`
	ProfilePath string    `json:"-"`
}

func defaultSettings() Settings {
	channels := []Channel{}
	if runtimeGOOS == "android" {
		channels = append(channels, Channel{Name: "termux", URL: "https://packages-cf.termux.dev/apt/termux-main", Distribution: "stable", Component: "main"})
	}
	return Settings{Channels: channels, Enabled: []string{}}
}

func loadSettings(paths AppPaths) (Settings, error) {
	if err := ensureConfiguration(paths); err != nil {
		return Settings{}, err
	}
	filename, err := readCurrentProfile(paths)
	if err != nil {
		return Settings{}, err
	}
	return loadProfileSettings(paths, filename)
}

func loadDefaultSettings(paths AppPaths) (Settings, error) {
	if err := ensureConfiguration(paths); err != nil {
		return Settings{}, err
	}
	filename, err := readProfileLock(paths)
	if err != nil {
		return Settings{}, err
	}
	return loadProfileSettings(paths, filename)
}

func loadNamedSettings(paths AppPaths, name string) (Settings, error) {
	if err := ensureConfiguration(paths); err != nil {
		return Settings{}, err
	}
	filename, err := profileFilename(name)
	if err != nil {
		return Settings{}, err
	}
	return loadProfileSettings(paths, filename)
}

func ensureConfiguration(paths AppPaths) error {
	if err := os.MkdirAll(paths.Set, 0o755); err != nil {
		return appError("io_error", err.Error())
	}
	if info, err := os.Lstat(paths.Profiles); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return appError("invalid_config", "profiles path must be a real directory")
		}
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(paths.Profiles, 0o755); err != nil {
			return appError("io_error", err.Error())
		}
	} else {
		return appError("io_error", err.Error())
	}

	var legacy Settings
	legacyExists := false
	if raw, err := os.ReadFile(paths.Settings); err == nil {
		legacyExists = true
		if strings.TrimSpace(string(raw)) != "enabled = true" {
			legacy, err = parseSettingsTOML(string(raw))
			if err != nil {
				return appError("invalid_config", err.Error())
			}
		} else {
			legacy = defaultSettings()
		}
	} else if !os.IsNotExist(err) {
		return appError("io_error", err.Error())
	}

	channels, channelsExist, err := readChannels(paths.Channels)
	if err != nil {
		return err
	}
	if !channelsExist {
		channels = defaultSettings().Channels
	}
	if legacyExists {
		channels = mergeChannels(channels, legacy.Channels)
	}
	channelSettings := Settings{Channels: channels, Enabled: []string{}}
	if err := normalizeSettings(&channelSettings); err != nil {
		return err
	}
	if !channelsExist || legacyExists {
		if err := writeChannels(paths.Channels, channelSettings.Channels); err != nil {
			return appError("io_error", err.Error())
		}
	}

	filename, lockExists, err := readOptionalProfileLock(paths)
	if err != nil {
		return err
	}
	if !lockExists {
		profiles, listErr := listProfileFilenames(paths)
		if listErr != nil {
			return listErr
		}
		if len(profiles) == 0 {
			filename = "default.toml"
			if err := writeProfile(paths, filename, legacy.Enabled); err != nil {
				return err
			}
		} else {
			filename = newestProfile(paths, profiles)
			if legacyExists {
				settings, loadErr := loadProfileSettings(paths, filename)
				if loadErr != nil {
					return loadErr
				}
				settings.Enabled = mergeProfileEnabled(settings.Enabled, legacy.Enabled)
				if err := writeProfile(paths, filename, settings.Enabled); err != nil {
					return err
				}
			}
		}
		if err := writeProfileLock(paths, filename); err != nil {
			return err
		}
	} else if legacyExists {
		settings, loadErr := loadProfileSettings(paths, filename)
		if loadErr != nil {
			return loadErr
		}
		settings.Enabled = mergeProfileEnabled(settings.Enabled, legacy.Enabled)
		if err := writeProfile(paths, filename, settings.Enabled); err != nil {
			return err
		}
	}
	if _, err := profilePath(paths, filename); err != nil {
		return err
	}
	profileInfo, err := os.Lstat(filepath.Join(paths.Profiles, filename))
	if err != nil || !profileInfo.Mode().IsRegular() {
		return appError("invalid_config", "profiles.lock references a missing or invalid profile: "+filename)
	}
	if _, err := readCurrentProfile(paths); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(paths.CurrentProfile); os.IsNotExist(err) {
		if err := atomicWriteFile(paths.CurrentProfile, []byte(filename+"\n"), 0o600); err != nil {
			return appError("io_error", err.Error())
		}
	}
	if legacyExists {
		archive := paths.Settings + ".migrated"
		for suffix := 1; ; suffix++ {
			if _, err := os.Lstat(archive); os.IsNotExist(err) {
				break
			} else if err != nil {
				return appError("io_error", err.Error())
			}
			archive = fmt.Sprintf("%s.migrated.%d", paths.Settings, suffix)
		}
		if err := os.Rename(paths.Settings, archive); err != nil {
			return appError("io_error", err.Error())
		}
		syncDirectory(paths.Set)
	}
	return nil
}

var profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func profileFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !profileNamePattern.MatchString(name) {
		return "", appError("invalid_config", "profile name must contain only letters, numbers, underscores, and hyphens")
	}
	return name + ".toml", nil
}

func profilePath(paths AppPaths, filename string) (string, error) {
	if filename == "profiles.lock" || filepath.Base(filename) != filename || !strings.HasSuffix(filename, ".toml") {
		return "", appError("invalid_config", "invalid profile filename")
	}
	name := strings.TrimSuffix(filename, ".toml")
	valid, err := profileFilename(name)
	if err != nil || valid != filename {
		return "", appError("invalid_config", "invalid profile filename")
	}
	path := filepath.Join(paths.Profiles, filename)
	if !pathWithin(paths.Profiles, path) {
		return "", appError("invalid_config", "profile path is outside the profiles directory")
	}
	return path, nil
}

func readOptionalProfileLock(paths AppPaths) (string, bool, error) {
	info, err := os.Lstat(paths.ProfilesLock)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, appError("io_error", err.Error())
	}
	if !info.Mode().IsRegular() {
		return "", true, appError("invalid_config", "profiles.lock must be a regular file")
	}
	raw, err := os.ReadFile(paths.ProfilesLock)
	if err != nil {
		return "", false, appError("io_error", err.Error())
	}
	filename := strings.TrimSpace(string(raw))
	if _, err := profilePath(paths, filename); err != nil {
		return "", true, appError("invalid_config", "profiles.lock contains an invalid profile filename")
	}
	return filename, true, nil
}

func readProfileLock(paths AppPaths) (string, error) {
	filename, exists, err := readOptionalProfileLock(paths)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", appError("invalid_config", "profiles.lock is missing")
	}
	info, err := os.Lstat(filepath.Join(paths.Profiles, filename))
	if err != nil || !info.Mode().IsRegular() {
		return "", appError("invalid_config", "profiles.lock references a missing profile: "+filename)
	}
	return filename, nil
}

func readCurrentProfile(paths AppPaths) (string, error) {
	info, err := os.Lstat(paths.CurrentProfile)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", appError("invalid_config", "current profile marker must be a regular file")
	}
	raw, err := os.ReadFile(paths.CurrentProfile)
	if err != nil {
		return "", err
	}
	filename := strings.TrimSpace(string(raw))
	if _, err := profilePath(paths, filename); err != nil {
		return "", appError("invalid_config", "current profile marker contains an invalid filename")
	}
	info, err = os.Lstat(filepath.Join(paths.Profiles, filename))
	if err != nil || !info.Mode().IsRegular() {
		return "", appError("invalid_config", "current profile does not exist: "+filename)
	}
	return filename, nil
}

func loadProfileSettings(paths AppPaths, filename string) (Settings, error) {
	path, err := profilePath(paths, filename)
	if err != nil {
		return Settings{}, err
	}
	channels, exists, err := readChannels(paths.Channels)
	if err != nil {
		return Settings{}, err
	}
	if !exists {
		return Settings{}, appError("invalid_config", "channels.json is missing")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Settings{}, appError("invalid_config", "profile must be a regular file: "+filename)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Settings{}, appError("invalid_config", "profile cannot be read: "+filename)
	}
	enabled, err := parseProfileTOML(string(raw))
	if err != nil {
		return Settings{}, appError("invalid_config", filename+": "+err.Error())
	}
	settings := Settings{Channels: channels, Enabled: enabled, ProfileName: filename, ProfilePath: path}
	if err := normalizeSettings(&settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func parseProfileTOML(input string) ([]string, error) {
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	section := ""
	var value strings.Builder
	inArray := false
	seenEnabled := false
	for scanner.Scan() {
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if line == "" {
			continue
		}
		if !inArray && strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section != "profile" {
				return nil, fmt.Errorf("unknown section %q", section)
			}
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok || section != "profile" || strings.TrimSpace(key) != "enabled" || seenEnabled && !inArray {
			return nil, fmt.Errorf("expected one profile.enabled array")
		}
		if !inArray {
			seenEnabled = true
			value.Reset()
			value.WriteString(strings.TrimSpace(raw))
		} else {
			value.WriteByte('\n')
			value.WriteString(line)
		}
		inArray = tomlValueNeedsMore(value.String())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if inArray || !seenEnabled {
		return nil, fmt.Errorf("profile.enabled must be a TOML array")
	}
	items, err := splitTOMLArray(value.String())
	if err != nil {
		return nil, err
	}
	enabled := make([]string, 0, len(items))
	for _, item := range items {
		entry, err := parseTOMLString(item)
		if err != nil {
			return nil, err
		}
		enabled = append(enabled, entry)
	}
	return enabled, nil
}

func encodeProfileTOML(enabled []string) string {
	var b strings.Builder
	b.WriteString("[profile]\nenabled = [\n")
	for _, entry := range enabled {
		b.WriteString("  ")
		b.WriteString(strconv.Quote(entry))
		b.WriteString(",\n")
	}
	b.WriteString("]\n")
	return b.String()
}

func writeProfile(paths AppPaths, filename string, enabled []string) error {
	path, err := profilePath(paths, filename)
	if err != nil {
		return err
	}
	settings := Settings{Channels: []Channel{}, Enabled: enabled}
	if err := normalizeSettings(&settings); err != nil {
		return err
	}
	return atomicWriteFile(path, []byte(encodeProfileTOML(settings.Enabled)), 0o644)
}

func writeProfileLock(paths AppPaths, filename string) error {
	if _, err := profilePath(paths, filename); err != nil {
		return err
	}
	return atomicWriteFile(paths.ProfilesLock, []byte(filename+"\n"), 0o600)
}

func listProfileFilenames(paths AppPaths) ([]string, error) {
	entries, err := os.ReadDir(paths.Profiles)
	if err != nil {
		return nil, err
	}
	filenames := []string{}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			if _, err := profilePath(paths, entry.Name()); err == nil {
			filenames = append(filenames, entry.Name())
			}
		}
	}
	sort.Strings(filenames)
	return filenames, nil
}

func newestProfile(paths AppPaths, filenames []string) string {
	newest := filenames[0]
	newestInfo, _ := os.Stat(filepath.Join(paths.Profiles, newest))
	for _, filename := range filenames[1:] {
		info, err := os.Stat(filepath.Join(paths.Profiles, filename))
		if err != nil {
			continue
		}
		if newestInfo == nil || info.ModTime().After(newestInfo.ModTime()) || info.ModTime().Equal(newestInfo.ModTime()) && filename < newest {
			newest, newestInfo = filename, info
		}
	}
	return newest
}

func readChannels(path string) ([]Channel, bool, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, appError("io_error", err.Error())
	}
	var wrapper struct {
		Channels []Channel `json:"channels"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		var channels []Channel
		if arrayErr := json.Unmarshal(raw, &channels); arrayErr != nil {
			return nil, true, appError("invalid_config", "channels.json: "+err.Error())
		}
		wrapper.Channels = channels
	}
	return wrapper.Channels, true, nil
}

func writeChannels(path string, channels []Channel) error {
	data, err := json.MarshalIndent(struct {
		Channels []Channel `json:"channels"`
	}{Channels: channels}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, append(data, '\n'), 0o644)
}

func mergeChannels(primary, additional []Channel) []Channel {
	merged := append([]Channel(nil), primary...)
	seen := map[string]bool{}
	for _, channel := range primary {
		seen[channel.Name] = true
	}
	for _, channel := range additional {
		if !seen[channel.Name] {
			merged = append(merged, channel)
			seen[channel.Name] = true
		}
	}
	return merged
}

var channelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func normalizeSettings(settings *Settings) error {
	channels := make([]Channel, 0, len(settings.Channels))
	seenChannels := map[string]bool{}
	for _, channel := range settings.Channels {
		channel.Name = strings.TrimSpace(channel.Name)
		channel.URL = strings.TrimSpace(channel.URL)
		channel = defaultAPTChannel(channel)
		parsedURL, urlErr := url.Parse(channel.URL)
		if !channelNamePattern.MatchString(channel.Name) || urlErr != nil || parsedURL.Host == "" || (parsedURL.Scheme != "https" && parsedURL.Scheme != "http") || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
			return appError("invalid_config", "channels require a valid name and a clean HTTP(S) repository URL")
		}
		if !validAPTToken(channel.Distribution) || !validAPTToken(channel.Component) || !validDebianArchitecture(channel.Architecture) {
			return appError("invalid_config", "channel distribution, component, or architecture is invalid")
		}
		if seenChannels[channel.Name] {
			continue
		}
		seenChannels[channel.Name] = true
		channels = append(channels, channel)
	}
	settings.Channels = channels
	selected := map[string]string{}
	for _, spec := range settings.Enabled {
		name, version, err := parsePackageVersion(spec)
		if err != nil {
			return appError("invalid_config", "enabled entry must be name==version: "+spec)
		}
		key := normalizePackageName(name)
		previous, exists := selected[key]
		if !exists {
			selected[key] = name + "==" + version
			continue
		}
		oldName, oldVersion, parseErr := parsePackageVersion(previous)
		if parseErr != nil {
			return appError("invalid_config", "invalid existing enabled entry: "+previous)
		}
		if !versionsEqual(version, oldVersion) {
			return appError("invalid_config", fmt.Sprintf("conflicting enabled versions for %s: %s and %s", name, oldName+"=="+oldVersion, name+"=="+version))
		}
	}
	keys := make([]string, 0, len(selected))
	for key := range selected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	settings.Enabled = make([]string, 0, len(keys))
	for _, key := range keys {
		settings.Enabled = append(settings.Enabled, selected[key])
	}
	return nil
}

func saveSettings(path string, settings Settings) error {
	if err := normalizeSettings(&settings); err != nil {
		return err
	}
	return atomicWriteFile(path, []byte(encodeSettingsTOML(settings)), 0o644)
}

func encodeSettingsTOML(settings Settings) string {
	var b strings.Builder
	b.WriteString("channels = [\n")
	for _, channel := range settings.Channels {
		b.WriteString("  { name = ")
		b.WriteString(strconv.Quote(channel.Name))
		b.WriteString(", url = ")
		b.WriteString(strconv.Quote(channel.URL))
		b.WriteString(", distribution = ")
		b.WriteString(strconv.Quote(channel.Distribution))
		b.WriteString(", component = ")
		b.WriteString(strconv.Quote(channel.Component))
		if channel.Architecture != "" {
			b.WriteString(", architecture = ")
			b.WriteString(strconv.Quote(channel.Architecture))
		}
		b.WriteString(" },\n")
	}
	b.WriteString("]\n\nenabled = [\n")
	for _, entry := range settings.Enabled {
		b.WriteString("  ")
		b.WriteString(strconv.Quote(entry))
		b.WriteString(",\n")
	}
	b.WriteString("]\n")
	return b.String()
}

func parseSettingsTOML(input string) (Settings, error) {
	normalized, normalizeErr := normalizeChannelArrayTables(input)
	if normalizeErr != nil {
		return Settings{}, normalizeErr
	}
	input = normalized
	settings := Settings{Channels: []Channel{}, Enabled: []string{}}
	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(input))
	var currentKey, current strings.Builder
	inArray := false
	for scanner.Scan() {
		line := stripTOMLComment(scanner.Text())
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !inArray {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				return settings, fmt.Errorf("expected key = value")
			}
			parsedKey := strings.TrimSpace(key)
			if _, exists := values[parsedKey]; exists || parsedKey == currentKey.String() && inArray {
				return settings, fmt.Errorf("duplicate setting %q", parsedKey)
			}
			currentKey.Reset()
			currentKey.WriteString(parsedKey)
			current.Reset()
			current.WriteString(strings.TrimSpace(value))
			inArray = tomlValueNeedsMore(current.String())
			if !inArray {
				values[currentKey.String()] = current.String()
			}
		} else {
			current.WriteByte('\n')
			current.WriteString(strings.TrimSpace(line))
			if !tomlValueNeedsMore(current.String()) {
				values[currentKey.String()] = current.String()
				inArray = false
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return settings, err
	}
	if inArray {
		return settings, fmt.Errorf("unterminated TOML array for %s", currentKey.String())
	}
	for key, value := range values {
		switch key {
		case "channels":
			items, err := splitTOMLArray(value)
			if err != nil {
				return settings, err
			}
			for _, item := range items {
				fields, err := parseInlineTable(item)
				if err != nil {
					return settings, err
				}
				name, nOK := fields["name"]
				url, uOK := fields["url"]
				if !nOK || !uOK {
					return settings, fmt.Errorf("channel requires name and url")
				}
				settings.Channels = append(settings.Channels, Channel{
					Name: name, URL: url,
					Distribution: fields["distribution"], Component: fields["component"], Architecture: fields["architecture"],
				})
			}
		case "enabled":
			items, err := splitTOMLArray(value)
			if err != nil {
				return settings, err
			}
			for _, item := range items {
				s, err := parseTOMLString(item)
				if err != nil {
					return settings, err
				}
				settings.Enabled = append(settings.Enabled, s)
			}
		default:
			return settings, fmt.Errorf("unknown setting %q", key)
		}
	}
	if _, hasChannels := values["channels"]; !hasChannels {
		settings.Channels = defaultSettings().Channels
	}
	return settings, nil
}

func normalizeChannelArrayTables(input string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	output := make([]string, 0)
	channelTables := make([]map[string]string, 0)
	var current map[string]string
	hasInlineChannels := false
	flush := func() error {
		if current == nil {
			return nil
		}
		if _, ok := current["name"]; !ok {
			return fmt.Errorf("channel table requires name")
		}
		if _, ok := current["url"]; !ok {
			return fmt.Errorf("channel table requires url")
		}
		channelTables = append(channelTables, current)
		current = nil
		return nil
	}
	for scanner.Scan() {
		original := scanner.Text()
		line := strings.TrimSpace(stripTOMLComment(original))
		if line == "" {
			if current == nil {
				output = append(output, original)
			}
			continue
		}
		if line == "[[channels]]" {
			if hasInlineChannels {
				return "", fmt.Errorf("channels cannot mix an array value with [[channels]] tables")
			}
			if err := flush(); err != nil {
				return "", err
			}
			current = map[string]string{}
			continue
		}
		if current != nil {
			key, rawValue, ok := strings.Cut(line, "=")
			if !ok {
				return "", fmt.Errorf("expected key = value in [[channels]] table")
			}
			fieldName := strings.TrimSpace(key)
			if fieldName == "channels" || fieldName == "enabled" {
				if err := flush(); err != nil {
					return "", err
				}
				output = append(output, original)
				if fieldName == "channels" {
					hasInlineChannels = true
				}
				continue
			}
			switch fieldName {
			case "name", "url", "distribution", "component", "architecture":
			default:
				return "", fmt.Errorf("unknown channel field %q", fieldName)
			}
			if _, exists := current[fieldName]; exists {
				return "", fmt.Errorf("duplicate channel field %q", fieldName)
			}
			value, err := parseTOMLString(rawValue)
			if err != nil {
				return "", fmt.Errorf("channel field %s: %w", fieldName, err)
			}
			current[fieldName] = value
			continue
		}
		if strings.HasPrefix(line, "channels ") || strings.HasPrefix(line, "channels=") {
			if len(channelTables) > 0 {
				return "", fmt.Errorf("channels cannot mix an array value with [[channels]] tables")
			}
			hasInlineChannels = true
		}
		output = append(output, original)
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if err := flush(); err != nil {
		return "", err
	}
	if len(channelTables) == 0 {
		return input, nil
	}
	var channels strings.Builder
	channels.WriteString("channels = [\n")
	for _, fields := range channelTables {
		channels.WriteString("  { ")
		keys := []string{"name", "url", "distribution", "component", "architecture"}
		first := true
		for _, key := range keys {
			value, ok := fields[key]
			if !ok {
				continue
			}
			if !first {
				channels.WriteString(", ")
			}
			first = false
			channels.WriteString(key + " = " + strconv.Quote(value))
		}
		channels.WriteString(" },\n")
	}
	channels.WriteString("]\n")
	output = append(output, channels.String())
	return strings.Join(output, "\n"), nil
}

func stripTOMLComment(line string) string {
	quoted, escaped := false, false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if quoted && r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if r == '#' && !quoted {
			return line[:i]
		}
	}
	return line
}

func tomlValueNeedsMore(value string) bool {
	quoted, escaped, brackets := false, false, 0
	for _, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if quoted && r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if !quoted {
			if r == '[' {
				brackets++
			}
			if r == ']' {
				brackets--
			}
		}
	}
	return quoted || brackets > 0
}

func splitTOMLArray(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '[' || value[len(value)-1] != ']' {
		return nil, fmt.Errorf("expected TOML array")
	}
	body := value[1 : len(value)-1]
	var items []string
	quoted, escaped := false, false
	depth := 0
	start := 0
	for i, r := range body {
		if escaped {
			escaped = false
			continue
		}
		if quoted && r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if !quoted {
			if r == '{' {
				depth++
			}
			if r == '}' {
				depth--
			}
			if r == ',' && depth == 0 {
				part := strings.TrimSpace(body[start:i])
				if part != "" {
					items = append(items, part)
				}
				start = i + 1
			}
		}
	}
	last := strings.TrimSpace(body[start:])
	if last != "" {
		items = append(items, last)
	}
	if quoted || depth != 0 {
		return nil, fmt.Errorf("malformed TOML array")
	}
	return items, nil
}

func parseTOMLString(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf("expected basic quoted string")
	}
	return strconv.Unquote(value)
}

func parseInlineTable(value string) (map[string]string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '{' || value[len(value)-1] != '}' {
		return nil, fmt.Errorf("expected inline channel table")
	}
	parts, err := splitTOMLArray("[" + value[1:len(value)-1] + "]")
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, part := range parts {
		key, raw, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("invalid inline table field")
		}
		fieldName := strings.TrimSpace(key)
		switch fieldName {
		case "name", "url", "distribution", "component", "architecture":
		default:
			return nil, fmt.Errorf("unknown channel field %q", fieldName)
		}
		if _, exists := result[fieldName]; exists {
			return nil, fmt.Errorf("duplicate channel field %q", fieldName)
		}
		field, err := parseTOMLString(raw)
		if err != nil {
			return nil, err
		}
		result[fieldName] = field
	}
	return result, nil
}

func deriveChannelName(repoURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(repoURL))
	if err == nil && (strings.EqualFold(parsed.Hostname(), "pypi.org") || strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".pypi.org")) {
		return "pypi"
	}
	if err == nil {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) > 0 && parts[len(parts)-1] != "" {
			last := strings.TrimSuffix(parts[len(parts)-1], ".git")
			if index := strings.IndexAny(last, ".:"); index >= 0 {
				last = last[:index]
			}
			if last != "" {
				return last
			}
		}
		host := parsed.Hostname()
		if host != "" {
			label := strings.Split(host, ".")[0]
			if label != "" {
				return label
			}
		}
	}
	return "channel"
}

func channelInstall(paths AppPaths, repoURL string) error {
	settings, err := loadSettings(paths)
	if err != nil {
		return err
	}
	name := deriveChannelName(repoURL)
	for _, ch := range settings.Channels {
		if ch.Name == name {
			return appError("channel_already_exists", name)
		}
	}
	settings.Channels = append(settings.Channels, defaultAPTChannel(Channel{Name: name, URL: repoURL}))
	if err := normalizeSettings(&settings); err != nil {
		return err
	}
	return writeChannels(paths.Channels, settings.Channels)
}
func channelRemove(paths AppPaths, name string) error {
	settings, err := loadSettings(paths)
	if err != nil {
		return err
	}
	updated := make([]Channel, 0, len(settings.Channels))
	for _, ch := range settings.Channels {
		if ch.Name != name {
			updated = append(updated, ch)
		}
	}
	if len(updated) == len(settings.Channels) {
		return appError("channel_not_found", name)
	}
	settings.Channels = updated
	if err := normalizeSettings(&settings); err != nil {
		return err
	}
	return writeChannels(paths.Channels, settings.Channels)
}

func mergeProfileEnabled(primary, legacy []string) []string {
	merged := append([]string(nil), primary...)
	seen := map[string]bool{}
	for _, spec := range primary {
		name, _, err := parsePackageVersion(spec)
		if err == nil {
			seen[normalizePackageName(name)] = true
		}
	}
	for _, spec := range legacy {
		name, _, err := parsePackageVersion(spec)
		if err != nil || seen[normalizePackageName(name)] {
			continue
		}
		merged = append(merged, spec)
		seen[normalizePackageName(name)] = true
	}
	return merged
}
