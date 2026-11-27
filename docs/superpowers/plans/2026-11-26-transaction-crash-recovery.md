# Transaction Crash Recovery Implementation Plan

> **For agentic workers:** Execute this plan inline. Preserve all existing workspace changes.

**Goal:** Prove transaction recovery returns the complete old state after interruption before commit and preserves the complete new state after commit.

**Architecture:** Add an optional internal observer around transaction mutation boundaries. A test observer can interrupt execution without invoking rollback, leaving the journal and filesystem as a process crash would. Exercise every mutation checkpoint using a real temporary filesystem and run recovery twice.

**Tech Stack:** Go standard library and existing package tests.

**Spec:** Existing transaction journal and recovery behavior in `transaction.go`.

## Global Constraints

- Do not use global mutable hooks; keep fault injection scoped to one transaction call.
- Simulated interruption must leave the prepared/committed journal intact and skip in-process rollback.
- Use real filesystem operations in tests; do not mock the recovery behavior.
- Preserve all existing workspace modifications and avoid commits.

## Review Focus

- Interruption before the commit marker must restore database, profile, current profile, links, moved artifacts, and transaction files.
- Interruption after the commit marker must retain all new state and only finish cleanup.
- Re-running recovery after a successful recovery must be harmless.
- Package replacement must restore the prior artifact if only some moves completed.
- Link removal and replacement must restore the correct symlink targets.

---

### Task 1: Exercise transaction interruption boundaries

**Files:**
- Modify: `transaction.go`
- Create: `transaction_crash_test.go`
- Create: `transaction_security_test.go`
- Create: `generation_integrity_test.go`
- Modify: `nixnet_test.go`, `paths_test.go`

**Interfaces:**
- Produces: internal `applyTransactionObserved(paths AppPaths, journal transactionJournal, db Database, afterMutation func(string) error) error`; `applyTransaction` remains the production entry point and passes no observer.
- Checkpoints: prepared journal, each move, database, profile/current-profile writes, each journal file, each link, committed journal, stage cleanup, and journal cleanup.

- [x] Add an interruption-matrix test using a real temporary transaction fixture; assert rollback for all checkpoints before commit, committed state for checkpoints at/after commit, and safe second recovery.
- [x] Run the focused test and confirm it fails because the observer entry point is absent.
- [x] Implement the scoped observer and checkpoints; observer errors return directly to simulate process loss.
- [x] Run the focused test and full `go test ./...`; record any failures outside this change.
- [x] Run `gofmt`, `go build ./...`, and `git diff --check`; inspect changed files.

## Execution Results

- Focused crash-recovery matrix: pass across all thirteen injected interruption points, including repeated recovery.
- Existing transaction and atomic-write tests: pass.
- The first full-suite run exposed six stale migration/profile-link test expectations; the fixtures now reflect profile-scoped closures and the current migration contract.
- Additional security tests cover symlink traversal in transaction and launcher paths, stale command-link preconditions, duplicate link operations, and drift between profile spec/closure and the active generation.
- Current full `go test ./...`, `go build ./...`, and `git diff --check`: pass.
- Rollback now keeps its journal when any rollback mutation or required directory sync fails, allowing a later recovery attempt to retry safely.
- Staged package files and directories are synced bottom-up before the prepared journal can reference them; newly created managed directories sync their parent entries.
- Added injected-fsync failure coverage proving recovery retains the journal and succeeds on retry. Command-link migration/reconciliation now propagates directory sync failures rather than ignoring them.
- Final focused recovery/durability tests, full `go test ./...`, `go build ./...`, and `git diff --check`: pass.
