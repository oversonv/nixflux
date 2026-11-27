<p align="center">
  <img src="https://img.shields.io/badge/NixNet-1.0.0--beta-7c3aed?style=for-the-badge" alt="NixNet 1.0.0 beta">
  <img src="https://img.shields.io/badge/Go-1.20%2B-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go 1.20 or newer">
</p>

<h1 align="center">NixNet</h1>

<p align="center">
  A profile-based package manager for Linux and Android/Termux.<br>
  Install signed Debian packages into isolated environments, with exact versions and recoverable transactions.
</p>

<p align="center">
  <a href="#install">Install</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#profiles">Profiles</a> ·
  <a href="#commands">Commands</a> ·
  <a href="docs/ARCHITECTURE.md">Architecture</a>
</p>

<p align="center">
  <a href="https://github.com/oversonv/nixnet/releases/tag/v1.0.0-beta">Release</a> ·
  <a href="https://github.com/oversonv/nixnet/blob/main/LICENSE">License</a> ·
  <a href="https://github.com/oversonv/nixnet/issues">Issues</a>
</p>

> [!WARNING]
> **1.0.0-beta** is an early release. It supports ARM64 Linux and Android/Termux. Read the [package compatibility notes](#package-compatibility) before using packages that rely on maintainer scripts.

## Why NixNet?

System package managers own the system. NixNet keeps its package payloads under `$HOME/nix/store` and publishes only the selected profile's commands. It does not install packages into or modify Termux's `dpkg` database.

| Capability | What it means |
|---|---|
| **Profiles** | Keep separate package sets for different tasks; switch the commands exposed on `PATH`. |
| **Exact closures** | Resolve each profile to pinned package versions and verified Store artifacts. |
| **Signed APT metadata** | Verify repository `Release` files with `gpgv`, then check package hashes before extraction. |
| **Recoverable changes** | Stage installs and removals in a journaled transaction; recover unfinished work on the next command. |
| **Termux-aware** | Publish command links in `$PREFIX/bin` and recognize dependencies already provided by Termux. |

## Install

The release includes prebuilt ARM64 binaries for Linux and Android/Termux. Install the beta with:

```sh
curl -fsSL https://raw.githubusercontent.com/oversonv/nixnet/main/install.sh \
  | NIXNET_VERSION=v1.0.0-beta sh
```

The installer places the binary in `$PREFIX/bin` on Termux or `$HOME/.local/bin` on Linux. On Linux, add `$HOME/.local/bin` to `PATH` if needed. The installer can also use `wget` when `curl` is unavailable.

NixNet requires the `gpgv` and `xz` executables for signed APT metadata and XZ-compressed package indexes or archives. On Termux, install them with `pkg install gpgv xz` if they are not already present.

## Quick start

```sh
nixnet --version
nixnet channel list
nixnet env install termux:neofetch==latest
nixnet env list
```

On Android, NixNet can configure the official Termux repository automatically. On Linux, add a signed APT repository first:

```sh
nixnet channel install https://packages.example.org/apt/stable
nixnet channel fetch stable
nixnet env install stable:tool==1.2.3
```

Use the repository's channel name in package specs. `latest` selects its newest available version; pin an exact version when you need a repeatable environment.

## Profiles

A profile is an independent set of requested package roots. NixNet resolves those roots and their dependencies into a concrete closure. `profiles.lock` selects the persistent default, while `.current` identifies the profile whose commands are exposed on `PATH`.

```sh
nixnet profiles install work
nixnet --profile work env install termux:eza==latest
nixnet profiles update work
nixnet profiles list
nixnet profiles generations work
nixnet profiles rollback work <generation-id>
```

The profile selector targets operations without switching the loaded environment. `profiles update work` applies and loads that profile; it does not change the persistent default.

Successful profile changes record generations so earlier states can be restored offline. When `env remove` physically removes a package, generations that require that package are pruned. A package stays in the Store when another active profile or another package in the same profile still needs it.

## Install, update, and remove

```sh
# Install one or more packages into the loaded profile
nixnet env install termux:figlet==2.2.5-3 termux:eza==latest

# Update selected roots from one channel
nixnet env update termux:all==latest
nixnet env update termux:eza==latest

# Freeze an exact root version
nixnet env freeze eza==0.23.5
nixnet env unfreeze eza==0.23.5

# Remove package roots from the profile
nixnet env remove figlet==latest eza==latest
```

Removing a root recomputes the profile closure and unlinks commands no longer provided by it. NixNet physically removes Store artifacts that no remaining active profile references. Dependencies still required by another root, and packages shared with another profile, stay in the Store. The removal and profile update happen in one recoverable transaction.

Short aliases are available: `i` install, `r` remove, `u` update, `l` list, `f` freeze, `uf` unfreeze, and `lf` list frozen packages.

## Repositories and trust

Repository settings live in `$HOME/nix/set/channels.json`. NixNet supports Debian APT repositories: it verifies each signed `Release` file using the repository's configured trusted key, then validates the selected `Packages` index against its signed size and SHA-256. Downloaded package archives are checked against the index hash before extraction.

NixNet reads the system package database only to detect dependencies already installed by Termux or Linux. It never changes that database or replaces `pkg`/APT.

## Package compatibility

NixNet parses Debian `.deb` archives and supports `Depends`, `Pre-Depends`, dependency alternatives and version constraints, architecture restrictions, regular files, directories, supported symlinks, regular-file hardlinks, and package checksums.

Maintainer scripts, triggers, conffile semantics, `Provides`, conflict/replacement rules, `Recommends`, `Suggests`, and complete Multi-Arch behavior are not implemented. Packages requiring unsupported features are rejected rather than partially installed.

## State layout

```text
$HOME/nix/
├── store/          verified package payloads
├── staging/        transaction staging and recovery data
├── bin/            managed command links on Linux
├── set/
│   ├── channels.json
│   ├── store.json
│   └── transaction.json
└── profiles/
    ├── profiles.lock
    ├── .current
    ├── <name>.toml
    └── <name>/generations/
```

On Termux, managed command links are published in `$PREFIX/bin`. NixNet reconciles only links it owns; unrelated commands and links are left alone.

## Commands

Run `nixnet --help` for the complete syntax. Main command groups:

| Group | Actions |
|---|---|
| `env` | `install`, `remove`, `list`, `update`, `freeze`, `unfreeze`, `frozen` |
| `channel` | `install`, `remove`, `list`, `fetch` |
| `profiles` | `list`, `install`, `remove`, `update`, `generations`, `rollback` |

`--profile NAME` selects a target profile for `env` commands without switching the loaded profile.

## Build and test

Requirements: Go 1.20+, `gpgv`, and `xz` for XZ-compressed data.

```sh
go test ./...
go vet ./...
go build -o nixnet ./cmd/nixnet
```

Build the release targets from the project root:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o releases/nixnet-linux-arm64 ./cmd/nixnet
GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o releases/nixnet-android-arm64 ./cmd/nixnet
```

See [`releases/README.md`](releases/README.md) for asset names and [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for implementation details.
