[← INDEX](../INDEX.md)

# ADR 005: v1 lands on `main` via a `GI-<n>-…` branch; there is no `develop`

**Status:** Accepted

**Context:** This repo's conventions are carried over from `deepseek-lens`, whose guards encode a
two-branch flow: PRs into `main` must come from a `develop` branch, and every commit landing on
`main` must trace to a *merged* `develop → main` PR. That flow works when there are stories arriving
continuously and an integration branch has something to integrate.

v1 has neither. Creating `develop` immediately would mean a branch with no purpose yet, plus a
two-hop merge for every change — and the `develop` gate's real function (proving a commit came
through the story-branch flow) has to be recovered some other way.

**Decision:** **No `develop`.** A story branch `GI-<n>-<slug>` is cut from `main` and PRs directly
back into `main`. The guards are **adapted, not copied**:

| `deepseek-lens` predicate | This repo |
|---|---|
| `head_ref == 'develop'` | `^GI-[0-9]+-[a-z0-9-]+$` |
| commit traces to a merged `develop → main` PR | commit traces to a merged `GI-<n>-… → main` PR |
| *(nothing)* | **a fourth step**: the branch's issue number must be one the PR body closes |

That fourth step exists because one hop to `main` no longer proves provenance the way `develop`
did — so the branch is bound to the same set of issues the commits are already bound to.

**Consequences:**

- **Copying the guards verbatim would have rejected this repo's own first PR.** This is recorded
  plainly in [CLAUDE.md](../../../CLAUDE.md) §Enforcement rather than left for someone to
  rediscover from a red CI run.
- `main` is the repo's **default branch**, which is what makes `Closes #N` in a PR body actually
  auto-close the issue — GitHub only honours it against the default branch.
- If a second story ever needs an integration branch, introduce `develop` and restore the two-guard
  flow. Until then there is nothing for it to integrate.
- Everything else is unchanged from `deepseek-lens`: the `GI#<n>` title gate, the closing keyword,
  the per-commit prefix check, and the pre-commit secret scan.

See [../infra-and-deploy.md](../infra-and-deploy.md) for the guard mechanics, including the
`github.event.before`-is-zeros skip on branch creation that makes creating `main` safe.
