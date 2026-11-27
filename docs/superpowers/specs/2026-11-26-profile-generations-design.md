# Profile generations design

## Intent

Add immutable snapshots of successfully applied profile states and offline rollback. This gives NixNet a useful Nix-like recovery model while reusing the existing verified Store and transaction journal.

## Goals

- Record profile roots and exact Store artifact IDs after each successful state change.
- List retained generations and restore one without network access or version resolution.
- Switch profile data, active command links, and generation pointer through one recoverable transaction.
- Remove a profile and its references atomically.

## Non-goals

- Repository snapshot locks and cross-device realization.
- Garbage collection, automatic generation pruning, or pins.
- Runtime isolation or changes to the system package manager.

## Approaches

- **Generation snapshots over the current profile closure (selected):** small schema and command surface; rollback can use existing Store objects. Requires transaction support for a small set of profile metadata files.
- **A complete lock and realization redesign:** stronger portability, but needs repository provenance, fetch-plan separation, and a broader migration. Defer until generation behavior is stable.
- **Copy each Store tree per generation:** easy to reason about, but duplicates data and weakens the shared Store model. Reject.

## Design

Each profile keeps immutable JSON manifests under `profiles/<name>/generations/` and a `.generation` pointer to the active manifest. A manifest records schema version, ID, creation time, enabled roots, and the closure's package-to-artifact mapping. Store metadata remains the source for artifact identity and verification.

Profile mutations create a manifest and switch its pointer inside the same transaction as database and command-link changes. Rollback validates the manifest, profile roots, closure, and Store objects before applying it. It performs no repository access. Transaction file changes are restricted to validated NixNet profile metadata paths, and recovery restores their previous bytes.

Removing a profile uses one journal to update the default/current markers, command links, profile database references, and profile file. Retained generation manifests are removed with the profile only after the transaction is committed.

## Constraints

- Keep Go 1.20 compatibility and add no dependencies.
- Preserve separate persistent-default and loaded-profile semantics.
- Keep Store artifacts immutable and never modify system package state.
- Do not prune generations automatically.
- Existing profiles without generation metadata remain usable; their first successful mutation creates the first generation.

## Acceptance

- Successful profile changes produce a valid immutable generation and current pointer.
- Generation listing is deterministic.
- Rollback restores prior roots, closure, and active command links without network access.
- Invalid manifests and missing/corrupt Store objects fail before state changes.
- Profile removal is recoverable across interruption.
