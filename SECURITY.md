# Security

## Reporting a vulnerability

**Preferred:** use GitHub's [Private Vulnerability Reporting](https://github.com/airlockrun/agentsdk/security/advisories/new).

**Fallback:** email `security@airlock.run`.

**Don't** open a public issue for vulnerabilities.

## What's a vulnerability in a library

- A flaw in agentsdk that, when used as documented, makes the dependent application vulnerable (memory corruption, panic on attacker-controlled input, cryptographic mistake, parser confusion, auth bypass, etc.).
- A defect in security-relevant code (signing, token handling, request authentication).

## What's not

- Bugs without a security impact — open a regular issue instead.
- Vulnerabilities in libraries that agentsdk depends on — report to the upstream first; we'll bump once they patch.
- Misuse: the dependent application using agentsdk in a way the docs warn against. (We're happy to make those warnings louder if you flag where they weren't loud enough.)

## What you can expect

- **Acknowledgment within 72 hours.**
- **Triage within 7 days.**
- **Fix targeted within 30 days for High/Critical, 90 days for Low/Medium.**
- Credit in the security advisory unless you ask to remain anonymous.

## Safe harbor

Good-faith research won't trigger legal action. Don't disclose publicly before we've patched (or 90 days, whichever first). Don't demand payment as a condition of disclosure.
