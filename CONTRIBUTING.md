# Contributing

Thanks for your interest in stitchfs.

## Building

```bash
go build ./...
go test ./...
./regression.sh   # focused regressions for dividers, wildcards, save paths
```

stitchfs is a single `main.go` plus tests. It builds with `CGO_ENABLED=0` and
cross-compiles to Linux and macOS on amd64/arm64.

## Before opening a PR

- `gofmt -w .` and `go vet ./...` must be clean.
- Add or update a test for behavior changes — the save/divider/wildcard paths
  are easy to regress.
- Keep it dependency-light. The only runtime dependency is
  [`hanwen/go-fuse`](https://github.com/hanwen/go-fuse).

## Reporting bugs

Open an issue with your OS, the exact command, and what you expected vs. saw.
For save problems, include which editor you used — save strategy (in-place vs.
temp-file rename vs. symlink replace) matters.
