# TODO — Activation checklist

Everything is built and merged. These are the **manual steps only you can do** to
switch the remaining install channels live. Nothing here needs code changes.

---

## 1. Make the repo public
Settings ▸ General ▸ Danger Zone ▸ **Change visibility → Public**.

Why it's required: release `.deb`/`.rpm` assets and GitHub Pages must be publicly
reachable for `apt`/`dnf`/badges to work.

- [ ] Repo is public
- [ ] Confirm the badges in the README now render (CI, release, license)

---

## 2. Turn on Homebrew  →  `brew install yedidiaSch/tap/envcore`
The `homebrew-tap` repo already exists and is wired; it just needs a push token.

1. Create a **fine-grained PAT**: https://github.com/settings/tokens?type=beta
   - Repository access: **only** `yedidiaSch/homebrew-tap`
   - Permissions: **Contents → Read and write**
2. Add it as a secret on the `envcore` repo:
   ```bash
   gh secret set HOMEBREW_TAP_GITHUB_TOKEN --repo yedidiaSch/envcore   # paste the PAT
   ```
3. Publish the cask for the current release (or wait for the next tag):
   ```bash
   git tag v0.1.1 && git push origin v0.1.1     # release workflow pushes the cask
   ```

- [ ] PAT created (scoped to `homebrew-tap`, contents:write)
- [ ] `HOMEBREW_TAP_GITHUB_TOKEN` secret set
- [ ] Tagged a release; cask appears in `yedidiaSch/homebrew-tap`
- [ ] `brew install yedidiaSch/tap/envcore` works

---

## 3. Turn on APT/YUM  →  `apt install envcore` / `dnf install envcore`
The signing key + publish workflow are already in place (`GPG_PRIVATE_KEY` secret
is set; public key in `packaging/`).

1. Enable GitHub Pages: Settings ▸ Pages ▸ Source **Deploy from a branch** →
   branch **`gh-pages`**, folder `/ (root)`.
   (The `gh-pages` branch is created by the workflow's first run — do step 2 first,
   then set the source, then it serves.)
2. Run the publisher for the existing release:
   Actions ▸ **Publish package repo** ▸ *Run workflow* ▸ tag `v0.1.0`.
3. Verify the repo is live:
   ```bash
   curl -fsSL https://yedidiaSch.github.io/envcore/gpg.key | head -1   # PGP PUBLIC KEY
   ```
4. Smoke-test on a Debian/Ubuntu box (or container):
   ```bash
   curl -fsSL https://yedidiaSch.github.io/envcore/gpg.key | sudo gpg --dearmor -o /usr/share/keyrings/envcore.gpg
   echo "deb [signed-by=/usr/share/keyrings/envcore.gpg] https://yedidiaSch.github.io/envcore/deb stable main" | sudo tee /etc/apt/sources.list.d/envcore.list
   sudo apt update && sudo apt install envcore && envcore version
   ```

- [ ] Publish workflow ran green for `v0.1.0`
- [ ] Pages source set to `gh-pages`
- [ ] `gpg.key` reachable over HTTPS
- [ ] `apt install envcore` works on a clean box
- [ ] `dnf install envcore` works on a clean box

> First real run of the repo-assembly step is this workflow dispatch (reprepro /
> createrepo_c only run on the GitHub runner). If it errors, paste the log — it's
> a quick iterate, same as the e2e scripts.

---

## Notes / caveats
- **Signing key:** fingerprint `8A19045A2466ED368AA23868988E9FA033620E2F`
  (`EnvCore Packages`). Private half is the `GPG_PRIVATE_KEY` secret; rotate per
  `packaging/README.md`.
- **macOS/Windows CLI** is built + released but **unvalidated** — that's roadmap
  **M7** (`../plan/envcore-roadmap.md` in the design set), not part of this checklist.
- After all three are green, every channel is live: `brew` · `apt` · `dnf` ·
  direct download · `go install`. Then delete this file.
