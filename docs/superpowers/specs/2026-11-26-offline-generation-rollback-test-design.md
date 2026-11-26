# Offline Generation Rollback Test Design

## Intent

Prove that a signed local APT install can be upgraded and removed, then restored from a saved profile generation without contacting its repository. This protects the promised offline rollback path against regressions across CLI dispatch, Store retention, generation validation, transaction recovery, and command publication.

## Goals

- Exercise the public CLI command flow against a loopback-only, signed APT fixture.
- Restore the exact earlier package version after a newer version was installed and removed.
- Prove rollback performs no repository request after the local endpoint is disabled and instrumented.
- Verify the restored Store artifact and execute the restored command link.

## Non-goals

- Add runtime package-manager behavior or network dependencies.
- Replace focused unit tests for APT parsing, transaction recovery, or generation digests.
- Test physical power loss; transaction crash boundaries remain simulated by existing tests.

## Constraints

- Use the repository's existing signed APT fixture helpers and standard library only.
- Keep HOME, PREFIX, package Store, profile state, and command links under `t.TempDir()`.
- Use an `Architecture: all` fixture package with a Termux payload path.
- Disable the HTTP repository before rollback, count any later requests, and reject them with HTTP 503.
- Skip only when the existing GPG fixture helper cannot run because required GPG tools are absent.

## Approaches

1. Add a loopback APT CLI lifecycle test using the existing repository fixture. Chosen: it covers the user-visible sequence without adding infrastructure.
2. Add a subprocess test that builds and runs a separate binary. Rejected: it adds build cost while the existing CLI integration test already exercises command dispatch in-process.
3. Add only another unit test for `rollbackProfile`. Rejected: it would miss startup recovery, profile loading, links, and CLI argument routing.

## Design

Add one focused test in `apt_test.go`. Install version 1.0.0 through `runCLI`, save its generation ID, install version 2.0.0, remove version 2.0.0, disable the local APT endpoint, and roll back through `profiles rollback`. The fixture counts and rejects any repository request after disablement. Assert the count stays at zero, the default profile selects version 1.0.0, its Store artifact verifies, and its managed command executes the version 1 payload.

## Implementation Calls

- Build each fixture DEB with the existing `makeDebFixture` helper and serve both through an instrumented `newTestRepositoryWithRequestHook` fixture.
- Use `listProfileGenerations` to identify the generation whose root is `demo==1.0.0`; avoid parsing human-readable output.
- Invoke `runCLI` for user-facing mutations and use internal readers only for precise state assertions.
- Use `exec.LookPath("sh")` for the fixture shebang so it is valid on Termux and Linux.
