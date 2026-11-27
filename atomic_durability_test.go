package nixnet

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSyncTreePersistsFilesAndDirectoriesBottomUp(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "payload", "bin")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(nested, "demo")
	if err := os.WriteFile(file, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("demo", filepath.Join(nested, "demo-link")); err != nil {
		t.Fatal(err)
	}
	var files, dirs []string
	err := syncTree(root, func(path string) error {
		files = append(files, path)
		return nil
	}, func(path string) error {
		dirs = append(dirs, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, []string{file}) {
		t.Fatalf("synced files = %#v; want only regular files %#v", files, []string{file})
	}
	if !reflect.DeepEqual(dirs, []string{nested, filepath.Join(root, "payload"), root}) {
		t.Fatalf("synced directories = %#v; want deepest first", dirs)
	}
}

func TestSyncTreeReturnsFileSyncFailure(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "payload")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected file sync failure")
	err := syncTree(root, func(string) error { return injected }, func(string) error { return nil })
	if !errors.Is(err, injected) {
		t.Fatalf("syncTree error = %v; want injected sync error", err)
	}
}
