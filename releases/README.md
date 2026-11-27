# Release assets

`v1.0.0-beta` publishes these ARM64 binaries:

| Asset | Target |
|---|---|
| `nixnet-linux-arm64` | Linux ARM64 (`linux/arm64`) |
| `nixnet-android-arm64` | Android/Termux ARM64 (`android/arm64`) |
| `SHA256SUMS` | SHA-256 checksums for both binaries |

Build from the project root:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o releases/nixnet-linux-arm64 ./cmd/nixnet
GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o releases/nixnet-android-arm64 ./cmd/nixnet
sha256sum releases/nixnet-linux-arm64 releases/nixnet-android-arm64 > releases/SHA256SUMS
```

Upload these assets to the GitHub release tagged `v1.0.0-beta`. Keep generated binaries and checksum files out of Git history.
