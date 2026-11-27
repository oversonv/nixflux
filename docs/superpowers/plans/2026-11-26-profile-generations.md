# Profile Generations Implementation Plan

> **For agentic workers:** Implement inline in this session. Keep existing uncommitted workspace changes. Do not add dependencies.

**Goal:** Add immutable profile generations, offline rollback, and atomic profile removal.

**Architecture:** Store generation manifests beside each profile. Extend the transaction journal with validated metadata-file mutations and backups. Use existing profile closures and Store verification as the rollback source.

**Tech Stack:** Go 1.20, existing JSON encoding, filesystem rename, transaction journal.

**Spec:** `docs/superpowers/specs/2026-11-26-profile-generations-design.md`

## Global Constraints

- Keep Go 1.20 compatibility and add no dependencies.
- Preserve separate persistent-default and loaded-profile semantics.
- Keep Store artifacts immutable and never modify system package state.
- Do not prune generations automatically.
- Existing profiles without generation metadata remain usable; their first successful mutation creates the first generation.

## Review Focus

- Malformed or path-traversing generation IDs must be rejected before filesystem access.
- A rollback with missing or corrupt Store artifacts must leave profile state and links unchanged.
- Recovery of a prepared transaction must restore metadata files and links together.
- A committed generation must not be overwritten by a later mutation.
- Removing the current/default profile must select a valid remaining profile or clear the pointer consistently.

---

### Task 1: Generation model and transaction metadata files

**Files:** `generation.go` (create), `transaction.go` (modify), `paths.go` (modify).

**Interfaces:**
- Produce `ProfileGeneration`, `generationPath`, `readCurrentGeneration`, and `validateGeneration`.
- Produce `transactionFileChange` and validated apply/rollback support for profile metadata files.

- [x] Define versioned generation manifest with ID, creation time, enabled roots, and artifact closure.
- [x] Validate profile names, generation IDs, manifest identities, closure identities, and metadata file paths.
- [x] Apply metadata writes/deletes after transaction prepare; restore original bytes during rollback.
- [x] Run `gofmt` and `go build ./...`.

### Task 2: Create generations with profile mutations

**Files:** `operations.go` (modify), `transaction.go` (modify), `generation.go` (modify).

**Interfaces:** Consume Task 1 metadata mutations. Produce new immutable generation manifest and active pointer in the same package transaction.

- [x] Create the manifest from the committed profile spec and resolved closure.
- [x] Add manifest creation and pointer change to the existing package transaction.
- [x] Reject ID collision instead of replacing an existing generation.
- [x] Run `gofmt` and `go build ./...`.

### Task 3: List generations and rollback offline

**Files:** `profiles.go` (modify), `cli.go` (modify), `generation.go` (modify), `operations.go` (modify).

**Interfaces:** Add `profiles generations <name>` and `profiles rollback <name> <generation-id>`.

- [x] List generations in stable creation-time and ID order; mark the active generation.
- [x] Validate rollback manifest and every referenced Store artifact before building changes.
- [x] Restore roots, closure, active pointer, and links through one transaction without repository calls.
- [x] Run `gofmt` and `go build ./...`.

### Task 4: Atomic profile removal

**Files:** `profiles.go` (modify), `transaction.go` (modify), `generation.go` (modify).

**Interfaces:** Consume transaction metadata-file changes. Profile removal deletes profile metadata and database references in one journal.

- [x] Stage profile, generation manifests, default/current markers, and closure changes in one transaction.
- [x] Switch active links to a valid remaining profile, or remove only NixNet-owned links when none remains.
- [x] Ensure recovery restores all removed files and previous links when the transaction is prepared.
- [x] Run `gofmt`, `go build ./...`, and `git diff --check`.
