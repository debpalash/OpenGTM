<!-- Thanks for contributing to OpenGTM! -->

## What & why

<!-- What does this change do, and why? Link any related issue: Closes #123 -->

## Type

- [ ] Bug fix
- [ ] New feature
- [ ] New enrichment provider / source
- [ ] Docs
- [ ] Refactor / chore

## Checklist

- [ ] Commits are **signed off** (`git commit -s`) per the [DCO](../DCO)
- [ ] I have signed the [CLA](../CLA.md) (and my employer the [Corporate CLA](../CLA-CORPORATE.md), if this is work for them)
- [ ] Backend tests pass (`uv run pytest`)
- [ ] Monorepo lint and build pass (`bun run lint && bun run build`)
- [ ] I did not add scraping of paywalled / pirated / login-gated content
- [ ] Credentialed providers are **opt-in and inert by default**

## CLA signature

To sign as the PR author, add the explicit declaration described in
[CONTRIBUTING.md](../CONTRIBUTING.md) after reading the [CLA](../CLA.md).
Checking the box above does not sign it. Other contributors can sign in a
new PR comment.

<!-- The PR author may sign by writing a new, unquoted line consisting of
     `CLA-Signature: ` followed by the exact declaration shown in CONTRIBUTING.md.
     Opening this PR or checking the box above does not sign the CLA. Other
     contributors must sign separately in a new PR comment. -->

## Security-sensitive?

<!-- Does this touch auth, tenancy/RLS, the SSRF url guard, secrets, outbound
     fetches, or LLM prompt handling? If so, describe the impact so a reviewer
     looks closely. Do NOT disclose an exploit here — see SECURITY.md. -->

## Screenshots / notes

<!-- Optional -->
