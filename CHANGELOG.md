# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.0] - 2026-06-27

First public release.

### Added
- FUSE filesystem that exposes a set of real files as one virtual combined file.
- Transactional split-save: editing the combined view writes changes back into
  the original backing files, or fails cleanly without corrupting them.
- Visible per-file dividers (`<!--stitchfs:/path-->`) that are hidden by Markdown
  renderers but keep section identity inline.
- `-divider-prefix` / `-divider-suffix` flags to suit non-Markdown files.
- Wildcard specs (`*`, `?`, `[abc]`, recursive `**`) with membership refreshed on
  lookup/open.
- `stitchfs -l LINK ...` to create a stable symlink to a combined view.
- `stitchpath` convenience wrapper for `stitchfs path ...`.
- Cross-platform binaries (Linux/macOS, amd64/arm64), a `.deb` package, and a
  Nix flake.

[0.4.0]: https://github.com/eric-frost/stitchfs/releases/tag/v0.4.0
