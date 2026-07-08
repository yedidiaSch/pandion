# Commercial licensing

Pandion is **free and open source** under the [GNU Affero General Public License
v3.0](LICENSE) (AGPL-3.0). Anyone — individuals and companies of any size — may
run, study, modify, and share it at no cost under those terms.

A **commercial license** is an alternative to the AGPL for cases where the AGPL's
obligations don't fit. It funds ongoing development.

> ⚠️ **Before relying on this:** have a lawyer review this file and the
> [`LICENSE`](LICENSE) (AGPL-3.0). Nothing here is legal advice.

---

## Do I need a commercial license?

Using Pandion **as-is** — the CLI, run against your own infrastructure — is fully
covered by the AGPL and needs **no commercial license**, regardless of your
company's size or whether the use is commercial. Distributing it unmodified is
also fine under the AGPL.

You should consider a commercial license if you want to do something the AGPL
would otherwise require you to open-source, for example:

| You want to… | AGPL requires | Commercial license |
|---|---|---|
| Run Pandion as-is, internally or in production | Nothing extra | Not needed — AGPL covers it |
| Modify Pandion for your own internal use | Nothing extra (no distribution) | Not needed |
| **Embed Pandion (or its code) in a proprietary/closed-source product** | You must release your product's corresponding source under AGPL | 💼 **Commercial license** |
| **Offer a modified Pandion to others over a network** (SaaS) | You must offer users the modified source (AGPL §13) | 💼 **Commercial license** (to keep modifications closed) |
| Ship Pandion inside a product whose license is incompatible with AGPL | Not permitted | 💼 **Commercial license** |

If you're unsure whether your use triggers the AGPL, email us — we're happy to
clarify.

---

## What a commercial license gives you

- The right to use, modify, and distribute Pandion **without** the AGPL's
  copyleft and network-source-disclosure obligations — so you can embed it in a
  closed-source product or hosted service and keep your changes private.
- *(Optional, defined per engagement)* priority support, security-response SLAs,
  and early access to releases.

Every contribution is made under our [CLA](CLA.md), so the maintainer holds the
rights needed to offer this alternative license alongside the AGPL.

## Pricing

Pricing is set per engagement, based on your organization's size and how you use
Pandion. Commercial licenses are typically **annual, per company**.

| Tier | Who | Price |
|---|---|---|
| **Startup** | Small teams / early-stage | Contact us |
| **Business** | Mid-size organizations | Contact us |
| **Enterprise** | Large organizations, custom terms, support SLA | Contact us |

## How to buy

Email **`didisc123@gmail.com`** with:
1. Your company name and approximate size,
2. How you intend to use Pandion (and what triggers the need — embedding, SaaS, etc.),
3. Roughly how many people / teams will use it.

We'll reply with a quote and a commercial license agreement.

---

## FAQ

**Is Pandion open source?**
Yes — genuinely. AGPL-3.0 is an [OSI-approved](https://opensource.org/licenses/AGPL-3.0)
open-source license and a [GNU](https://www.gnu.org/licenses/agpl-3.0.html) copyleft
license. You can read, build, run, fork, and modify it freely.

**Can I use it commercially without paying?**
Yes. Commercial *use* of the tool as-is is fine under the AGPL. A commercial
license is only about escaping the AGPL's **source-sharing** obligations — mainly
embedding Pandion in a closed-source product or a modified hosted service.

**We're a big company running it internally against our own cloud — do we pay?**
No. Running the CLI as-is, however large you are, is covered by the AGPL.

**Does the AGPL "infect" my code just because I use the CLI?**
Running a program does not subject your other software to the AGPL. The AGPL's
obligations attach when you **distribute a modified Pandion** or **offer a
modified Pandion to others over a network** — not to code that merely invokes the
CLI as a separate program.

**Do older releases (published under BSL 1.1) change?**
No. Versions already published under the Business Source License remain under
their original terms (and keep their four-year Apache-2.0 conversion). Everything
from the relicensing forward is AGPL-3.0.
