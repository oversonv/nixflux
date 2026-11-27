# Release binaries

Build ARM64 release binaries from the project root:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o releases/nixnet-linux-arm64 ./cmd/nixnet
GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o releases/nixnet-android-arm64 ./cmd/nixnet
```

These binaries are uploaded as GitHub Release assets and kept out of Git history.
