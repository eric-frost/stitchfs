# Publishing `stitchfs-bin` to the AUR

This needs a one-time AUR account and SSH key — it can't be automated from the
release pipeline. Steps:

1. Create an account at https://aur.archlinux.org and add your SSH public key
   under *My Account*.
2. Clone the (empty) AUR repo and drop these two files in:

   ```bash
   git clone ssh://aur@aur.archlinux.org/stitchfs-bin.git
   cd stitchfs-bin
   cp /path/to/stitchfs/packaging/aur/PKGBUILD .
   cp /path/to/stitchfs/packaging/aur/.SRCINFO .
   git add PKGBUILD .SRCINFO
   git commit -m "stitchfs-bin 0.4.0"
   git push
   ```

3. On each release, bump `pkgver`/`sha256sums_*` in `PKGBUILD`, regenerate
   `.SRCINFO` with `makepkg --printsrcinfo > .SRCINFO`, then commit and push.

The `sha256sums_*` come straight from the release `checksums.txt`.
