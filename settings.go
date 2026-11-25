package nixflux

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
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
	Channels []Channel `json:"channels"`
	Enabled  []string  `json:"enabled"`
}

func defaultSettings() Settings {
	channels := []Channel{}
	if runtimeGOOS == "android" {
		channels = append(channels, Channel{Name: "termux", URL: "https://packages-cf.termux.dev/apt/termux-main", Distribution: "stable", Component: "main"})
	}
	return Settings{Channels: channels, Enabled: []string{}}
}

func loadSettings(paths AppPaths) (Settings, error) {
	if _, err := os.Stat(paths.Settings); err == nil {
		data, err := os.ReadFile(paths.Settings)
		if err != nil {
			return Settings{}, appError("io_error", err.Error())
		}
		// Early NixFlux archives shipped this placeholder instead of a valid
		// settings document. Migrate that exact legacy value to empty defaults.
		if strings.TrimSpace(string(data)) == "enabled = true" {
			settings := defaultSettings()
			if err := saveSettings(paths.Settings, settings); err != nil {
				return Settings{}, err
			}
			return settings, nil
		}
		settings, err := parseSettingsTOML(string(data))
		if err != nil {
			return Settings{}, appError("invalid_config", err.Error())
		}
		if err := normalizeSettings(&settings); err != nil {
			return Settings{}, err
		}
		return settings, nil
	} else if !os.IsNotExist(err) {
		return Settings{}, appError("io_error", err.Error())
	}

	settings := defaultSettings()
	if raw, err := os.ReadFile(paths.LegacyChannels); err == nil {
		var old struct {
			Channels []Channel `json:"channels"`
		}
		if err := json.Unmarshal(raw, &old); err != nil {
			return Settings{}, appError("invalid_config", "legacy channels.json: "+err.Error())
		}
		settings.Channels = old.Channels
	} else if !os.IsNotExist(err) {
		return Settings{}, appError("io_error", err.Error())
	}
	if err := normalizeSettings(&settings); err != nil {
		return Settings{}, err
	}
	if err := saveSettings(paths.Settings, settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
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
	return saveSettings(paths.Settings, settings)
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
	return saveSettings(paths.Settings, settings)
}
