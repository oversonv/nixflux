# Durable Directory Creation Implementation Plan

> **For agentic workers:** Execute this plan inline. Preserve all existing workspace changes.

**Goal:** Ensure newly created parent directories are synced before atomic files and links are published.

**Architecture:** Add one helper that creates missing directory components from the nearest existing parent outward and syncs each parent after creation. Use it in the existing atomic file and symlink writers so all their callers receive the same durability behavior.

**Tech Stack:** Go standard library; existing `syncDirectory` helper.

**Spec:** Bugfix plan derived from the current atomic write path in `atomic.go`.

## Global Constraints

- Preserve the existing atomic rename behavior and file permissions.
- Do not add dependencies or change package-manager behavior beyond durability of newly created directories.
- Preserve unrelated existing workspace modifications.

## Review Focus

- Existing path components may already exist or be symlinks; directory creation must not fail for valid existing paths.
- Concurrent creation can return `EEXIST`; re-check that the path is a directory.
- Every newly created directory must have its parent synced before proceeding.
- Atomic file and symlink publication must remain unchanged after parent preparation.
- Errors creating or syncing directories must stop publication and propagate to the caller.

---

### Task 1: Sync newly created directory components

**Files:**
- Modify: `atomic.go`

**Interfaces:**
- Produces: `ensureDirectory(path string, mode os.FileMode) error`.
- Consumes: existing `syncDirectory(path string)`.

- [ ] Implement `ensureDirectory` to find the nearest existing ancestor, verify it is a directory, create missing components outward in order, tolerate a concurrent `EEXIST` only after verifying the component is a directory, and sync each parent after creation.
- [ ] Replace `os.MkdirAll` in `atomicWriteFile` and `atomicSymlink` with `ensureDirectory` while preserving their current modes and atomic publication logic.
- [ ] Run `gofmt`, `go build ./...`, and `git diff --check`; inspect the final diff for unrelated edits.
