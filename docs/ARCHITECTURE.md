# NixFlux source architecture

## Current source boundaries

The executable entry point is `cmd/nixflux/main.go`. It delegates to the reusable root Go package (`package nixflux`). The root package currently contains the application implementation and is intentionally kept together to avoid a risky package-boundary rewrite during the enabled-state repair.

The root source files are grouped by responsibility, even though they are not yet separate Go import packages:

| Area | Files | Responsibility |
|---|---|---|
| CLI and output | `cli.go`, `output.go`, `errors.go`, `runtime.go` | Argument validation, command dispatch, user-facing output, runtime constants |
| Application operations | `operations.go` | Install/remove/sync/freeze workflows and dependency resolution |
| Configuration | `settings.go`, `profiles.go`, `spec.go` | `channels.json`, profile TOML files, package specifications, validation and normalization |
| Repository client | `repository.go`, `apt.go`, `fetcher.go` | HTTP retrieval, APT Release and Packages metadata, repository selection |
| Debian archive | `deb.go`, `debian_version.go` | AR/TAR parsing, payload conversion and Debian version ordering |
| Store and metadata | `package.go`, `database.go`, `metadata.go`, `jsonutil.go`, `paths.go` | Package extraction, verification, installed database and filesystem paths |
| Transactions and locking | `transaction.go`, `atomic.go`, `lock.go` | Journaling, commit/rollback, atomic writes and process lock |

Tests remain alongside the root package so they can exercise internal transaction and repository behavior. Runtime state under `$HOME/nix/` is not source code and is not included in the source archive.

## State ownership

`channels.json` is the sole repository configuration source. Each profile TOML owns an independent `enabled` package target list; `profiles.lock` selects the persistent default and `.current` selects the loaded working environment. `store.json` indexes immutable-by-convention artifacts and records a resolved artifact closure per profile. The shared Store contains bytes; profile closures define package ownership and availability. These are different concepts:

- the loaded profile's `enabled` list determines which root package entrypoints may be exposed in `PATH`.
- the profile closure pins each root and transitive dependency to an artifact ID derived from its Store path.
- each profile's `.generation` pointer selects an immutable manifest under `profiles/<name>/generations/`; the manifest retains prior roots and artifact IDs for offline rollback.
- the artifact index is shared for deduplication, but resolution only reuses packages in the selected profile closure or read-only system packages.
- dependency commands are not published unless the dependency is also an explicit root target in the loaded profile.
- `profiles update [name]` reconciles only the selected profile's enabled targets with repositories and the Store. Without a name it uses `profiles.lock`; with a name it also loads that profile without changing the persistent default.

Package and freeze mutations update the target profile, artifact index, profile closure, immutable generation manifest, generation pointer, and command links through one transaction journal. `profiles generations` lists retained manifests; `profiles rollback` verifies Store artifacts and restores one generation without network access. Removing a profile atomically removes its spec, generation manifests, database references, default/current markers, and owned command links. Store objects remain available to other profiles. Garbage collection is not implemented. Startup link reconciliation removes stale NixFlux-owned command links created by older releases, without deleting unrelated commands. On Android, current links live in `$PREFIX/bin`; `$HOME/nix/bin` is retained only as a legacy migration source. The CLI refuses to run on Android when `PREFIX` is missing or not absolute, preventing silent publication into a directory outside Termux's standard `PATH`.

## Dependency direction

The intended dependency direction is:

```text
cmd/nixflux -> nixflux CLI -> application operations -> repository / archive / store / transaction
```

Low-level components should not import the CLI or application orchestration. The current single-library-package layout does not enforce all these boundaries at the Go compiler level; this is recorded as future modularization work, not claimed as completed.

## Recommended future package layout

A future extraction should be incremental and preserve existing public behavior:

```text
cmd/nixflux/
internal/
  app/              # use cases: install, remove, synchronize, freeze
  domain/           # package, dependency, version and target models
  config/           # settings parsing, validation, migration
  repository/apt/   # APT metadata, signature verification and index client
  archive/deb/      # ar/tar parsing and DEB conversion
  store/            # package database, payload verification and command links
  transaction/      # journal, commit, rollback and recovery
  platform/         # Termux paths, architecture, keyring and dpkg status
  cli/              # command parsing and output
```

Move one cohesive responsibility at a time, add package-level tests, and avoid moving files solely to make the tree look layered. The handwritten TOML parser currently supports a documented subset; replacing it with a maintained TOML implementation remains an explicit follow-up.
