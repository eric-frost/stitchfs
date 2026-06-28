# stitchfs

[![CI](https://github.com/eric-frost/stitchfs/actions/workflows/ci.yml/badge.svg)](https://github.com/eric-frost/stitchfs/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/eric-frost/stitchfs.svg)](https://pkg.go.dev/github.com/eric-frost/stitchfs)
[![Latest release](https://img.shields.io/github/v/release/eric-frost/stitchfs?sort=semver)](https://github.com/eric-frost/stitchfs/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Edit several files as one. Save, and the changes split back into the originals.**

`stitchfs` is a small [FUSE](https://en.wikipedia.org/wiki/Filesystem_in_Userspace)
filesystem and CLI. Point it at two or more real files and it exposes a single
virtual file that stitches them together with a visible divider between each one.
Open that virtual file in any editor, make your edits anywhere in it, save — and
`stitchfs` writes each section back into the file it came from. If your edit would
break the stitching, the save is rejected and the originals are left untouched.

![demo](docs/demo.gif)

It was built for a Markdown workflow — reviewing and editing a handful of notes,
spec sections, or agent context files side by side as one document — but the
backing files can be anything, and the divider style is configurable.

## Why

You can already `cat a.md b.md`, but you can't edit the result and have it land
back in `a.md` and `b.md`. Copy-pasting between files loses your place and invites
mistakes. `stitchfs` gives you one editable buffer with a clear, machine-checkable
boundary between sections, and makes the write-back transactional so a fat-fingered
save can't quietly corrupt your files.

## Install

`stitchfs` runs on **Linux and macOS** (FUSE required). No Windows.

### go install

```bash
go install github.com/eric-frost/stitchfs@latest
```

Installs the `stitchfs` binary into `$(go env GOPATH)/bin`.

### Debian / Ubuntu (.deb)

Download the `.deb` from the [latest release](https://github.com/eric-frost/stitchfs/releases/latest):

```bash
sudo apt install ./stitchfs_*_amd64.deb   # pulls in fuse3
```

This also installs the `stitchpath` wrapper and a `stitchfs.service` systemd unit
(not enabled by default).

### Nix

```bash
nix run github:eric-frost/stitchfs -- --version   # try it
nix profile install github:eric-frost/stitchfs    # install it
```

### Homebrew

```bash
brew install eric-frost/tap/stitchfs
```

### Arch (AUR)

```bash
yay -S stitchfs-bin     # or: paru -S stitchfs-bin
```

### Build from source

```bash
git clone https://github.com/eric-frost/stitchfs
cd stitchfs
go build -o stitchfs .
```

You also need `fuse3` (Linux) or [macFUSE](https://macfuse.github.io/) (macOS)
present at runtime for the mount to work.

## Quick start

```bash
# 1. Run the mount (or install the systemd service — see below).
sudo mkdir -p /mnt/stitchfs
stitchfs mount -allow-other /mnt/stitchfs &

# 2. Get a combined view of two files.
stitchfs notes/part1.md notes/part2.md
# -> /mnt/stitchfs/%2F...%2Fpart1.md,%2F...%2Fpart2.md

# 3. Open that path in your editor, edit, save. Changes land back in part1.md
#    and part2.md. Or do it in one line:
kate "$(stitchfs notes/part1.md notes/part2.md)"
```

Reading the combined file looks like this:

```text
<!--stitchfs:/home/you/notes/part1.md-->

...contents of part1.md...

<!--stitchfs:/home/you/notes/part2.md-->

...contents of part2.md...
```

The `<!--stitchfs:...-->` dividers are HTML comments, so Markdown renderers hide
them while they stay visible and editable in a plain-text editor.

## Commands

| Command | What it does |
| --- | --- |
| `stitchfs FILE...` | Print the virtual combined path for these files (default mode). |
| `stitchfs -l LINK FILE...` | Create/update a symlink `LINK` pointing at the combined path. |
| `stitchfs path FILE...` | Same as the default; exists so `stitchpath` can delegate. |
| `stitchpath FILE...` | Wrapper for `stitchfs path ...`. |
| `stitchfs mount [-allow-other] MOUNTPOINT` | Run the FUSE filesystem (usually via systemd). |

Useful flags on `mount`: `-divider-prefix` / `-divider-suffix` to change the
marker (see [non-Markdown files](#non-markdown-files)), `-debug`, `-trace`,
`-version`.

## Wildcards

A quoted glob stays *dynamic* in the combined view — it re-expands every time the
file is opened, so new matches show up on reopen:

```bash
stitchfs -l ~/tmp/all-notes.md '/home/you/notes/**/*.md'
```

Supports `*`, `?`, character classes (`[abc]`), and recursive `**`. Keep the
pattern quoted so your shell doesn't expand it first. Entries with no matches are
skipped; a spec that matches nothing behaves as a missing file (`ENOENT`). An
open file handle keeps the set it saw at open time — reopen to refresh.

## Saving — the transactional part

`stitchfs` accepts a save only if the edited content still contains the exact
divider for every backing file, in order, with nothing but whitespace before the
first one. On a valid save it splits the content at the dividers, writes each
piece to a temp file beside the original, and renames them into place. So:

- invalid edits (a deleted/renamed/reordered divider) are rejected as a normal
  save error (`EPERM`), and the originals are untouched;
- the virtual blank lines around dividers are not written back into your files;
- both common editor save styles work: temp-file-rename (Kate, gedit, …) and
  direct in-place writes (scripts, `Path.write_text()`, …).

### Non-Markdown files

The default divider is an HTML comment so Markdown hides it. For other file types,
pick a comment style that fits when you start the mount:

```bash
# Shell / Python / YAML style
stitchfs mount -divider-prefix '# ---stitchfs:' -divider-suffix ' ---' /mnt/stitchfs
```

## Running the mount

### systemd (system-wide)

The `.deb` installs `/lib/systemd/system/stitchfs.service`. Enable it:

```bash
sudo systemctl enable --now stitchfs.service   # mounts /mnt/stitchfs
```

For a `go install` build, copy
[`packaging/stitchfs.service`](packaging/stitchfs.service), fix `ExecStart` to
your binary path, and enable it.

### Manual

```bash
sudo mkdir -p /mnt/stitchfs
stitchfs mount -allow-other /mnt/stitchfs
```

When you run `stitchfs FILE...` and the mount is down, the CLI tries to start
`stitchfs.service` for you before printing a path.

## How it differs from other "concat" filesystems

There are existing FUSE tools that present multiple files as one — they're worth
knowing about, and they solve a different problem:

- [`schlaile/concatfs`](https://github.com/schlaile/concatfs) and
  [`concat-fuse`](https://github.com/concat-fuse/concat-fuse) concatenate files
  (e.g. split movie files) into one **read-only** stream.
- [`mergerfs`](https://github.com/trapexit/mergerfs) is a union filesystem that
  merges whole *directories*, not the contents of individual files.

`stitchfs` is aimed at **editing**: visible per-file dividers, a transactional
write-back that splits your edits to the right files, validation that refuses to
corrupt originals, and dynamic wildcard membership. If you only need a read-only
concatenation of large binaries, the tools above are a better fit.

## Security notes

- FUSE only — needs `/dev/fuse` and `fusermount3` (Linux) or macFUSE (macOS).
- `-allow-other` lets other local users read/write the mount; it implies
  `default_permissions` so the kernel still enforces the backing files' modes.
  Don't expose a `-allow-other` mount on a multi-user box you don't trust.
- The virtual file is rendered on demand from the backing files; nothing is
  cached to disk.

## Limitations

- Combined paths are flat virtual files under one mount, not a browsable tree.
- Wildcard membership refreshes on open, not pushed live into already-open editors.
- Some editors replace a symlink on save instead of writing through it; for
  editing, open the real combined path (or confirm your editor preserves
  symlinks).
- Last successful save wins; there's no cross-editor conflict resolution.

## Development

```bash
go test -race ./...
./regression.sh      # focused divider/wildcard/save regressions
gofmt -l . ; go vet ./...
```

Single `main.go`, one runtime dependency
([`hanwen/go-fuse`](https://github.com/hanwen/go-fuse)), builds with
`CGO_ENABLED=0`. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE) © Eric Frost
