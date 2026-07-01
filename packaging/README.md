# EnvCore package repository

A signed **APT** + **YUM** repository is published to GitHub Pages by
`.github/workflows/packages-repo.yml` from each release's `.deb`/`.rpm` artifacts.

- Served at: `https://yedidiaSch.github.io/envcore/`
- `deb/` — apt repo (`stable main`, amd64+arm64), `Release` signed by the project key
- `rpm/` — yum repo, `repomd.xml` signed by the project key
- `gpg.key` — the public signing key (also committed here as
  `envcore-packages.gpg.key`)
- `envcore.repo` — a ready-to-drop `/etc/yum.repos.d/` file

## Signing key

- **Fingerprint:** `8A19045A2466ED368AA23868988E9FA033620E2F`
- Identity: `EnvCore Packages <127786072+yedidiaSch@users.noreply.github.com>`
- The **private** key is stored as the repo Actions secret `GPG_PRIVATE_KEY`
  (no passphrase, CI-scoped). The workflow imports it to sign the repos.
- To **rotate**: generate a new key, replace the secret and
  `envcore-packages.gpg.key`, and re-run the workflow.

## One-time activation (after the repo is public)

1. **Make the repo public** (release assets + Pages must be publicly reachable).
2. **Enable GitHub Pages** → Settings ▸ Pages ▸ Source: **Deploy from a branch**,
   Branch: **`gh-pages`** / `/ (root)`. (The workflow creates `gh-pages` on first run.)
3. **Populate the repo** for the existing release:
   Actions ▸ *Publish package repo* ▸ **Run workflow** (tag `v0.1.0`).
   Future releases publish automatically on `release: published`.

After that, `sudo apt install envcore` / `sudo dnf install envcore` work per the
top-level README.
