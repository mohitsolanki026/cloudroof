# Contributing to CloudRoof

Thanks for your interest. A couple of ground rules keep the project healthy and
keep its licensing options open.

## How contributions are licensed

CloudRoof is licensed under **AGPL-3.0-only** (see `LICENSE`). By submitting a
contribution you agree that:

1. It is licensed to the project and its users under AGPL-3.0-only; and
2. Because CloudRoof is **dual-licensed**, you also grant the maintainer the
   right to distribute your contribution under CloudRoof's separate commercial
   license. This keeps the commercial tier possible without chasing per-patch
   permission later.

If you cannot grant (2), say so in your PR and we will discuss.

## Developer Certificate of Origin

Every commit must be signed off. The sign-off certifies you wrote the patch, or
otherwise have the right to submit it under the above terms — the standard
[Developer Certificate of Origin 1.1](https://developercertificate.org).

Sign off with `git commit -s`, which appends:

    Signed-off-by: Your Name <you@example.com>

## Before you open a PR

- `make test` (go vet + unit tests) and `make e2e` (full stack) pass.
- `gofmt` is clean; the frontend `tsc --noEmit` is clean.
- Read `.agents/README.md` — it lists the invariants (never a Tier-3 button,
  every param has an allowlist pattern, audit before return, secrets only in
  `resolveTarget`, …) that a change must not regress.
