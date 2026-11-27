package nixnet

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanMissingUnreferencedArtifacts(t *testing.T) {
	store := t.TempDir()
	present := "present"
	if err := os.Mkdir(filepath.Join(store, present), 0o755); err != nil {
		t.Fatal(err)
	}
	db := emptyDatabase()
	db.Artifacts["present"] = InstalledPackage{Dir: "present"}
	db.Artifacts["missing-orphan"] = InstalledPackage{Dir: "missing-orphan"}
	db.Artifacts["missing-profile"] = InstalledPackage{Dir: "missing-profile"}
	db.ProfileClosures["default.toml"] = map[string]string{"demo": "missing-profile"}

	if !cleanMissingUnreferencedArtifacts(store, &db) {
		t.Fatal("expected stale artifact records to be cleaned")
	}
	if _, exists := db.Artifacts["missing-orphan"]; exists {
		t.Fatal("missing unreferenced artifact record was retained")
	}
	if _, exists := db.Artifacts["missing-profile"]; !exists {
		t.Fatal("profile-referenced artifact record was removed before repair")
	}
	if _, exists := db.Artifacts["present"]; !exists {
		t.Fatal("existing Store artifact was removed despite having no profile reference")
	}
}
