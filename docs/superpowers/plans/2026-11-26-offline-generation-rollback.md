# Offline Generation Rollback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove a prior package generation can be restored through the CLI without contacting its APT repository.

**Architecture:** Extend the existing signed loopback APT integration coverage with one complete install, upgrade, remove, and offline rollback sequence. Reuse the current DEB and repository fixtures, with a request hook that rejects and counts requests during rollback, so the test checks the production resolver and transaction flow without introducing new libraries or external network access.

**Tech Stack:** Go standard library, existing GPG fixture helper, existing APT and DEB test helpers.

**Spec:** `docs/superpowers/specs/2026-11-26-offline-generation-rollback-test-design.md`

## Global Constraints

- Use only a loopback APT server and temporary HOME/PREFIX state.
- Keep runtime behavior and dependencies unchanged.
- Disable the APT fixture during rollback and assert it receives no repository requests.
- Verify restored package metadata, Store integrity, and execution of the managed command.

## Review Focus

- Generation lookup must select the version 1.0.0 root generation, not the later generation.
- Removing version 2.0.0 must not destroy the Store artifact referenced by version 1.0.0.
- Rollback must succeed without making a request to the disabled repository.
- The profile closure and published command must both point to the restored version 1.0.0 artifact.
- Test isolation must keep environment paths and command links under temporary directories.

---

### Task 1: Add the offline CLI rollback integration test

**Files:**
- Modify: `apt_test.go`
- Modify: `nixnet_test.go`

**Interfaces:**
- Consumes: `makeDebFixture`, `newTestRepositoryWithRequestHook`, `runCLI`, `listProfileGenerations`, `verifyStoredPackage`.
- Produces: `TestCLIProfileRollbackRestoresPreviousPackageWithoutNetwork`.

- [x] Create two signed-repository DEB fixtures for `demo` versions 1.0.0 and 2.0.0, with distinguishable executable output.
- [x] Isolate HOME and PREFIX, configure the loopback channel, and install version 1.0.0 through `runCLI`.
- [x] Capture the generation ID whose enabled root is `demo==1.0.0`.
- [x] Install version 2.0.0, remove it, disable the APT endpoint, and run `profiles rollback` to the saved generation.
- [x] Assert the restored profile closure selects version 1.0.0, its artifact passes Store verification, and the managed command prints the version 1 marker.
- [x] Assert rollback makes no request to the disabled repository endpoint.
- [x] Run the focused test and the full Go test suite; run `go build ./...` and `git diff --check`.

## Execution Results

- `TestCLIProfileRollbackRestoresPreviousPackageWithoutNetwork`: pass.
- Full `go test ./...`: pass.
- `go build ./...`: pass.
- `git diff --check`: pass.
