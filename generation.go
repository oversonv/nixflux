package nixflux

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const generationSchemaVersion = 1

var generationIDPattern = regexp.MustCompile(`^[a-z0-9]+$`)

type ProfileGeneration struct {
	SchemaVersion int               `json:"schema_version"`
	ID            string            `json:"id"`
	Profile       string            `json:"profile"`
	CreatedAt     time.Time         `json:"created_at"`
	Enabled       []string          `json:"enabled"`
	Closure       map[string]string `json:"closure"`
	Frozen        map[string]string `json:"frozen"`
	Digest        string            `json:"digest"`
}

func generationDirectory(paths AppPaths, profile string) (string, error) {
	profilePath, err := profilePath(paths, profile)
	if err != nil {
		return "", err
	}
	profileInfo, profileErr := os.Lstat(profilePath)
	if profileErr != nil && !os.IsNotExist(profileErr) {
		return "", wrapAppError("io_error", "inspect profile generation owner", profileErr)
	}
	if profileErr == nil && !profileInfo.Mode().IsRegular() {
		return "", appError("invalid_config", "profile generation owner must be a regular file")
	}
	name := strings.TrimSuffix(filepath.Base(profilePath), ".toml")
	profileDir := filepath.Join(paths.Profiles, name)
	generationDir := filepath.Join(profileDir, "generations")
	for _, dir := range []string{profileDir, generationDir} {
		info, statErr := os.Lstat(dir)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return "", wrapAppError("io_error", "inspect profile generation directory", statErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", appError("invalid_config", "profile generation path must contain only real directories")
		}
	}
	return generationDir, nil
}

func generationPath(paths AppPaths, profile, id string) (string, error) {
	if !generationIDPattern.MatchString(id) {
		return "", appError("invalid_config", "invalid profile generation ID")
	}
	dir, err := generationDirectory(paths, profile)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

func generationPointerPath(paths AppPaths, profile string) (string, error) {
	dir, err := generationDirectory(paths, profile)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), ".generation"), nil
}

func readCurrentGeneration(paths AppPaths, profile string) (string, error) {
	path, err := generationPointerPath(paths, profile)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		dir, dirErr := generationDirectory(paths, profile)
		if dirErr != nil {
			return "", dirErr
		}
		entries, readErr := os.ReadDir(dir)
		if os.IsNotExist(readErr) {
			return "", nil
		}
		if readErr != nil {
			return "", wrapAppError("io_error", "inspect profile generations", readErr)
		}
		if len(entries) != 0 {
			return "", appError("invalid_database", "profile has generations but no current generation pointer")
		}
		return "", nil
	}
	if err != nil {
		return "", wrapAppError("io_error", "read current profile generation", err)
	}
	if !info.Mode().IsRegular() {
		return "", appError("invalid_config", "profile generation pointer must be a regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", wrapAppError("io_error", "read current profile generation", err)
	}
	id := strings.TrimSpace(string(raw))
	if !generationIDPattern.MatchString(id) {
		return "", appError("invalid_config", "profile generation pointer contains an invalid ID")
	}
	if _, err := readProfileGeneration(paths, profile, id); err != nil {
		return "", wrapAppError("invalid_database", "validate current profile generation", err)
	}
	return id, nil
}

func newProfileGeneration(profile string, settings Settings, db Database) (ProfileGeneration, error) {
	name := strings.TrimSuffix(profile, ".toml")
	filename, err := profileFilename(name)
	if err != nil {
		return ProfileGeneration{}, err
	}
	if filename != profile || settings.ProfileName != profile {
		return ProfileGeneration{}, appError("invalid_config", "profile generation owner does not match its settings")
	}
	now := time.Now().UTC()
	closure := map[string]string{}
	for name, artifactID := range db.ProfileClosures[profile] {
		closure[name] = artifactID
	}
	frozen := map[string]string{}
	for name, version := range db.ProfileFrozen[profile] {
		frozen[name] = version
	}
	generation := ProfileGeneration{
		SchemaVersion: generationSchemaVersion,
		ID:            fmt.Sprintf("%x", now.UnixNano()),
		Profile:       profile,
		CreatedAt:     now,
		Enabled:       append([]string{}, settings.Enabled...),
		Closure:       closure,
		Frozen:        frozen,
	}
	digest, err := generationDigest(generation)
	if err != nil {
		return ProfileGeneration{}, err
	}
	generation.Digest = digest
	return generation, nil
}

func generationDigest(generation ProfileGeneration) (string, error) {
	generation.Digest = ""
	raw, err := json.Marshal(generation)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func encodeGeneration(generation ProfileGeneration) ([]byte, error) {
	if generation.SchemaVersion != generationSchemaVersion || !generationIDPattern.MatchString(generation.ID) || generation.Profile == "" || generation.CreatedAt.IsZero() {
		return nil, appError("invalid_database", "profile generation metadata is incomplete")
	}
	digest, err := generationDigest(generation)
	if err != nil {
		return nil, err
	}
	if generation.Digest != digest {
		return nil, appError("invalid_database", "profile generation digest does not match its contents")
	}
	return marshalPythonJSON(generation)
}

func validateGenerationPayload(raw []byte, profile, id string) error {
	var generation ProfileGeneration
	if err := json.Unmarshal(raw, &generation); err != nil {
		return wrapAppError("invalid_database", "parse profile generation", err)
	}
	if generation.ID != id || generation.Profile != profile || generation.SchemaVersion != generationSchemaVersion || generation.CreatedAt.IsZero() || generation.Closure == nil {
		return appError("invalid_database", "profile generation identity or schema is invalid")
	}
	digest, err := generationDigest(generation)
	if err != nil {
		return err
	}
	if generation.Digest != digest {
		return appError("invalid_database", "profile generation digest does not match its contents")
	}
	return nil
}

func readProfileGeneration(paths AppPaths, profile, id string) (ProfileGeneration, error) {
	path, err := generationPath(paths, profile, id)
	if err != nil {
		return ProfileGeneration{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ProfileGeneration{}, appError("profile_not_found", "generation "+id)
		}
		return ProfileGeneration{}, wrapAppError("io_error", "read profile generation", err)
	}
	if !info.Mode().IsRegular() {
		return ProfileGeneration{}, appError("invalid_database", "profile generation manifest must be a regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ProfileGeneration{}, wrapAppError("io_error", "read profile generation", err)
	}
	if err := validateGenerationPayload(raw, profile, id); err != nil {
		return ProfileGeneration{}, err
	}
	var generation ProfileGeneration
	if err := json.Unmarshal(raw, &generation); err != nil {
		return ProfileGeneration{}, wrapAppError("invalid_database", "parse profile generation", err)
	}
	return generation, nil
}

func validateProfileGeneration(paths AppPaths, generation ProfileGeneration, db Database) error {
	if generation.SchemaVersion != generationSchemaVersion || !generationIDPattern.MatchString(generation.ID) {
		return appError("invalid_database", "profile generation schema or ID is invalid")
	}
	if _, err := profilePath(paths, generation.Profile); err != nil {
		return wrapAppError("invalid_database", "invalid profile generation owner", err)
	}
	digest, err := generationDigest(generation)
	if err != nil {
		return err
	}
	if generation.Digest != digest {
		return appError("invalid_database", "profile generation digest does not match its contents")
	}
	for name, version := range generation.Frozen {
		artifactID, exists := generation.Closure[normalizePackageName(name)]
		pkg, artifactExists := db.Artifacts[artifactID]
		if !exists || !artifactExists || !versionsEqual(pkg.Version, version) {
			return appError("invalid_database", "profile generation contains an invalid frozen package")
		}
	}
	settings := Settings{ProfileName: generation.Profile, Enabled: generation.Enabled}
	checkDB := cloneDatabase(db)
	checkDB.ProfileClosures[generation.Profile] = generation.Closure
	checkDB.ProfileFrozen[generation.Profile] = generation.Frozen
	return ensureProfileClosure(paths, &checkDB, settings)
}

func generationPointerChange(paths AppPaths, profile, id string) (transactionFileChange, error) {
	path, err := generationPointerPath(paths, profile)
	if err != nil {
		return transactionFileChange{}, err
	}
	if !generationIDPattern.MatchString(id) {
		return transactionFileChange{}, appError("invalid_config", "invalid profile generation ID")
	}
	return captureTransactionFileChange(path, []byte(id+"\n"), 0o600, false)
}

func generationManifestChange(paths AppPaths, generation ProfileGeneration) (transactionFileChange, error) {
	path, err := generationPath(paths, generation.Profile, generation.ID)
	if err != nil {
		return transactionFileChange{}, err
	}
	if _, err := os.Lstat(path); err == nil {
		return transactionFileChange{}, appError("invalid_database", "profile generation ID already exists")
	} else if !os.IsNotExist(err) {
		return transactionFileChange{}, wrapAppError("io_error", "check profile generation path", err)
	}
	content, err := encodeGeneration(generation)
	if err != nil {
		return transactionFileChange{}, err
	}
	return captureTransactionFileChange(path, content, 0o644, false)
}
