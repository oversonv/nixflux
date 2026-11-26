package nixflux

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

func ensureDirectory(path string, mode os.FileMode) error {
	if path == "" {
		return fmt.Errorf("directory path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	missing := []string{}
	for current := filepath.Clean(absolute); ; current = filepath.Dir(current) {
		info, statErr := os.Stat(current)
		if statErr == nil {
			if !info.IsDir() {
				return fmt.Errorf("directory component is not a directory: %s", current)
			}
			break
		}
		if !os.IsNotExist(statErr) {
			return statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("no existing parent directory for %s", path)
		}
		missing = append(missing, current)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		dir := missing[i]
		if err := os.Mkdir(dir, mode.Perm()); err != nil {
			if !os.IsExist(err) {
				return err
			}
			info, statErr := os.Stat(dir)
			if statErr != nil {
				return statErr
			}
			if !info.IsDir() {
				return fmt.Errorf("directory component is not a directory: %s", dir)
			}
		}
		if err := syncDirectory(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	return nil
}

// syncTree makes every regular file and directory entry under root durable.
// Directories are synced deepest-first so child entries reach stable storage
// before their parents and ancestors.
func syncTree(root string, syncFile func(string) error, syncDir func(string) error) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return syncFile(path)
		}
		if info.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		return fmt.Errorf("unsupported staged file type: %s", path)
	})
	if err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool {
		depthI := len(filepath.Clean(dirs[i]))
		depthJ := len(filepath.Clean(dirs[j]))
		if depthI == depthJ {
			return dirs[i] > dirs[j]
		}
		return depthI > depthJ
	})
	for _, dir := range dirs {
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func syncRegularFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	parent := filepath.Dir(path)
	if err := ensureDirectory(parent, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(parent, ".nixflux-tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDirectory(parent)
}

func atomicWriteJSON(path string, value any) error {
	data, err := marshalPythonJSON(value)
	if err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return atomicWriteFile(path, data, 0o644)
}

func atomicSymlink(target, link string) error {
	if err := ensureDirectory(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(link), ".nixflux-link-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Remove(tmpPath); err != nil {
		return err
	}
	if err := os.Symlink(target, tmpPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, link); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return syncDirectory(filepath.Dir(link))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
