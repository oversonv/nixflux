# NixFlux

NixFlux is a Go package manager for Linux and Android/Termux. Repository packages are native Debian binary archives (`.deb`). It does not use the former `.nixflux` repository package format and does not call `dpkg-deb`.

## Requirements

- Go 1.20 or newer to build from source.
- `gpgv` at runtime to verify signed APT `Release` metadata.
- Trusted repository public keys. On Termux these are read from `$PREFIX/share/termux-keyring` and `$PREFIX/etc/apt/trusted.gpg.d`.
- `xz` only when a repository index or Debian archive uses XZ compression. Plain, gzip, and bzip2 index/archive streams are handled internally.

Termux packages are built for Bionic and must match the device architecture. NixFlux maps Go `arm64`, `arm`, `386`, and `amd64` to Termux APT architectures `aarch64`, `arm`, `i686`, and `x86_64` respectively.

Build on Termux:

```sh
pkg install golang
GOOS=android GOARCH=arm64 go build -o nixflux ./cmd/nixflux
```

Run the CLI directly from source (the root package is a library now):

```sh
go run ./cmd/nixflux --help
go run ./cmd/nixflux set update
```

Run tests and static checks:

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o nixflux ./cmd/nixflux
```

## Default Termux channel

On Android builds, NixFlux creates the official Termux main channel by default:

```toml
channels = [
  { name = "termux", url = "https://packages-cf.termux.dev/apt/termux-main", distribution = "stable", component = "main" },
]

enabled = []
```

The channel uses the APT distribution `stable`, component `main`, and the runtime Termux architecture. The official Termux repository also publishes root and X11 channels:

```toml
channels = [
  { name = "termux", url = "https://packages-cf.termux.dev/apt/termux-main", distribution = "stable", component = "main" },
  { name = "root", url = "https://packages-cf.termux.dev/apt/termux-root", distribution = "root", component = "stable" },
  { name = "x11", url = "https://packages-cf.termux.dev/apt/termux-x11", distribution = "x11", component = "main" },
]

enabled = []
```

NixFlux loads `dists/<distribution>/Release`, verifies its detached `Release.gpg` signature with `gpgv`, and downloads the matching `Packages` index only when its size and SHA-256 match the signed Release metadata. It supports uncompressed, `.gz`, `.bz2`, and `.xz` Packages indexes. It does not fetch a private `index.json` from an APT repository.

For a custom APT repository, put its trusted public key in the system APT trusted keyring directory or set `NIXFLUX_KEYRING` to a trusted keyring file or directory. Do not disable signature verification for public repositories.

## Configuration and data layout

All mutable NixFlux state lives under `$HOME/nix/`:

```text
$HOME/nix/
├── store/                  # Immutable installed package directories
├── bin/                    # Managed command links on non-Termux; legacy migration source on Termux
├── staging/                # Temporary transaction data
└── set/
    ├── settings.toml       # APT channels and enabled package targets
    ├── store.json          # NixFlux package database
    ├── transaction.json    # Recovery journal, absent when idle
    └── nixflux.lock        # Process lock
```

Example `settings.toml`:

```toml
channels = [
  { name = "termux", url = "https://packages-cf.termux.dev/apt/termux-main", distribution = "stable", component = "main" },
]

enabled = [
  "neofetch==7.1.0-1",
]
```

`enabled` entries are exact `name==version` targets for environment synchronization. `nixflux env install <channel>:<name>==latest` resolves `latest` once and stores the concrete version returned by the repository, not the word `latest`. The `latest` selector is therefore an installation-time convenience, not a rolling update policy. `nixflux set update` and its alias `nixflux env update` enforce the exact versions currently listed in `enabled`; they install missing targets and repair missing or invalid transitive dependencies. Debian version comparison includes epochs, upstream versions, revisions, and `~` pre-releases.

`enabled` is the authoritative desired set for user-requested packages. `env install` adds or replaces that package's target in `enabled`; `env remove` removes the target by package name. An installed package that is not listed in `enabled` is not exposed as a command. Packages installed only as dependencies remain available in the private store but their entrypoints are not published unless the package is explicitly installed as a target itself. A package listed in `enabled` but missing from the database is restored by `set update` / `env update`. Each normalized package name can have only one enabled version; conflicting version rows are rejected instead of being silently resolved by choosing one.

## Debian package handling

NixFlux reads the Debian `ar` container and its `control.tar*` and `data.tar*` members in Go. It validates the archive structure, package name/version/architecture, paths, symlink targets, dependencies, and file checksums. The complete downloaded `.deb` is verified against its signed APT Packages index SHA-256 before extraction.

Package files are unpacked into NixFlux's isolated content-addressed store. Only entrypoints belonging to packages listed in `settings.toml` `enabled` are exposed through symlinks. On Termux, managed links are created directly in `$PREFIX/bin`, which is already in Termux's default `PATH`; on other platforms, links are stored in `$HOME/nix/bin` and that directory must be added to `PATH`. Installed dependency packages are not exposed automatically.

At startup, NixFlux reconciles its managed links against `enabled`. Links created by older builds for packages that are not enabled are removed, but only when the link target is inside the NixFlux store. The old `$HOME/nix/bin` directory is retained only as a migration source for pre-Termux layouts; current Android links are placed in `$PREFIX/bin`. An Android build requires `$PREFIX` to be an absolute path and exits with an error rather than silently publishing commands into a directory outside the normal Termux `PATH`. A missing or non-executable enabled entrypoint is not published; `env update` can repair the package. NixFlux refuses to overwrite an existing Termux command with the same name. When upgrading an older build, run one NixFlux command once so stale links from the previous implementation are reconciled.

NixFlux checks `$PREFIX/var/lib/dpkg/status` on Termux when deciding whether a dependency is already provided by the system. This is read-only: NixFlux does not update or remove packages from the system `dpkg` database and does not replace `pkg` or APT.

NixFlux keeps package payloads in its private store. On Termux it creates only managed command symlinks in `$PREFIX/bin`; it does not unpack package payload files over the Termux prefix or alter the system dynamic-loader configuration. Dependencies already installed in Termux can satisfy package requirements. A dependency downloaded into the private NixFlux store is tracked and stored there, but arbitrary native binaries that require that dependency through the system ELF loader are not guaranteed to run without an explicit runtime wrapper/library-path integration. This is an intentional boundary of the current isolated-store implementation, not full `dpkg` runtime compatibility.

### Current Debian compatibility boundaries

Implemented: `Depends` and `Pre-Depends`, version constraints, alternative dependencies, architecture restrictions, regular files, directories, relative symlinks, supported absolute symlinks within known package prefixes, and regular-file hardlinks.

Not implemented: package maintainer scripts (`preinst`, `postinst`, `prerm`, `postrm`, `config`), triggers, conffile upgrade semantics, virtual package resolution (`Provides`), package conflict/replacement semantics, `Recommends`/`Suggests`, and complete Multi-Arch rules. Packages requiring maintainer scripts or unsupported special filesystem nodes are rejected rather than installed partially. XZ-compressed members require the `xz` executable; `dpkg-deb` is not required.

The DEB parser creates an internal staging archive for NixFlux's private store. That staging representation is not a repository package format and is never downloaded from a channel.

## Commands

```sh
./nixflux --help
./nixflux version

./nixflux channel install https://packages-cf.termux.dev/apt/termux-main
./nixflux channel list
./nixflux channel fetch termux
./nixflux channel remove termux

./nixflux env install termux:neofetch==latest
./nixflux env install termux:neofetch==7.1.0-1
./nixflux env list
./nixflux env freeze neofetch==7.1.0-1
./nixflux env frozen
./nixflux env unfreeze neofetch==7.1.0-1
./nixflux env remove neofetch==7.1.0-1

./nixflux set update
./nixflux env update
```

An exact version is required for remove, freeze, and unfreeze. A normal install does not downgrade an installed package. Downgrades selected by exact `settings.toml` targets are handled by `set update` / `env update`. Frozen packages cannot change versions. Removing a package required by another installed NixFlux package is rejected. `env remove` also removes that package from `enabled`, so a later environment update does not reinstall it.

## Atomicity and recovery

Install and remove operations stage package contents and write a transaction journal before changing store directories, entrypoint links, the package database, or `settings.toml`. The journal stores the previous settings bytes so an interrupted operation can restore configuration and package state together. Interrupted transactions are recovered by the next command. Mutating commands are serialized with a process lock.
