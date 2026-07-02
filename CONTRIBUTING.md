# Contributing to Pandion

Thanks for your interest in improving Pandion! Bug reports, ideas, and pull
requests are welcome.

## Contributor License Agreement (required)

Pandion is offered under multiple licenses — the [Business Source License 1.1](LICENSE)
and paid [commercial licenses](COMMERCIAL.md). To keep that possible, every
contributor must agree to the [Contributor License Agreement (CLA)](CLA.md)
before their contribution can be merged. The CLA lets the Maintainer include your
contribution in all editions of Pandion, including commercial ones, while you keep
copyright ownership of your work.

**How to agree:** on your first pull request, add a comment with exactly:

```
I have read the CLA and I agree to it.
```

The Maintainer records this against your GitHub username; you won't be asked
again for future PRs. *(This can later be automated with a CLA bot — see the note
at the bottom.)*

> **Why a CLA and not just a DCO?** A Developer Certificate of Origin
> (`Signed-off-by:`) only certifies you had the right to submit under the current
> license — it does **not** grant the right to relicense your code commercially.
> Because Pandion is dual-licensed, a CLA is required; a DCO alone is not enough.

## Development

```bash
export PATH="$HOME/.local/go/bin:$PATH"
make ci                     # gofmt + go vet + go test -race + build (offline)
go run ./cmd/pandion demo   # exercise the full lifecycle on the mock provider
```

Please make sure `make ci` is green before opening a PR. New behavior should come
with tests — the `mock` provider lets you cover orchestration offline, and
`scripts/` has self-cleaning real-cloud e2e checks for cloud-facing changes.

## Pull requests
- Keep PRs focused and reviewable.
- Match the surrounding code style (Go, `gofmt`).
- Describe what changed and how you verified it.
- Reference any related issue.

## Reporting bugs / security issues
- **Bugs / features:** open a GitHub issue with steps to reproduce.
- **Security vulnerabilities:** please do **not** open a public issue — email the
  Maintainer privately (see `COMMERCIAL.md` for contact) so it can be fixed
  before disclosure.

---

### Optional: automate CLA sign-off with a bot
To require CLA agreement automatically on every PR, enable a CLA bot such as
[contributor-assistant/github-action](https://github.com/contributor-assistant/github-action):
add its workflow, a `signatures/` store, and a token secret. Until then, the
manual comment process above is sufficient.
