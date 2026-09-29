# Releasing Expanse

A release is a tagged commit on `main`, a GitHub Release carrying the installer
ISO, and a website update. `scripts/release.sh` does all three from a local
checkout, so the ISO that is published is the one that was tested.

## Steps

1. Bump `nix/version.nix`, add a `## <version> - <YYYY-MM-DD>` section to
   `CHANGELOG.md`, and commit as `release: <version> -- <one-line summary>`
   (the summary becomes the release title).
2. Tag it: `git tag -a v<version> -m "Expanse <version>"`, and push `main`.
3. Stage everything without publishing:

   ```sh
   scripts/release.sh --dry-run
   ```

   This builds `.#iso` into `result-iso-<version>`, runs the `iso-version`
   check, and writes `dist/<version>/`:
   `SHA256SUMS`, `notes.md` (the changelog section) and `release.json`.
4. Install, first-boot and reboot `result-iso-<version>` in QEMU. The same
   commit always builds the same store path, so this is the image that ships.
5. Publish: `scripts/release.sh` (asks for confirmation that step 4 passed;
   `--yes` skips the question, `--draft` makes a draft release). It pushes the
   tag if GitHub lacks it, creates the release with the ISO, `SHA256SUMS` and
   `release.json` attached (re-running refreshes the assets and notes), and
   runs the website hook.

`HEAD` must be the tag and the tree clean. GitHub caps release assets at 2 GiB
each; the 1.1.6 ISO is 1.37 GiB.

## Website hook

After publishing, the script runs `$EXPANSE_WEBSITE/tools/sync-release DIR`
(default `../expanse-website`) if it exists and is executable. `DIR` is the
absolute path of `dist/<version>/`, holding `notes.md` (Markdown) and
`release.json`:

```json
{
  "version": "1.1.6",
  "tag": "v1.1.6",
  "date": "2026-09-28",
  "title": "Expanse 1.1.6: installer facelift, tty1 host console, quiet boots",
  "release_url": "https://github.com/team-expanse/expanse/releases/tag/v1.1.6",
  "notes": "notes.md",
  "iso": {
    "name": "expanse-1.1.6-x86_64-linux.iso",
    "url": "https://github.com/team-expanse/expanse/releases/download/v1.1.6/expanse-1.1.6-x86_64-linux.iso",
    "sha256": "…",
    "size": 1472069632
  }
}
```

The hook belongs to the website repository: it updates the site's version,
download link and checksum, adds the changelog entry, and commits there.
`release.json` is also attached to every GitHub release, so the site can be
brought up to date from the release alone.
