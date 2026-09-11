# Security policy

## Supported versions

Security fixes target the latest stable release of Jelto contracts. Older
versions are not maintained; upgrade before reporting a problem that is already
fixed. Before the first stable release, reports against `main` are welcome.
There is no long-term-support branch or guaranteed response or fix deadline.

## Report privately

Email Taha Bozdemir at [taha@jelto.io](mailto:taha@jelto.io) with the subject
`Security: jelto-contracts`. Do not disclose an unresolved vulnerability in a
public issue, PR, discussion or log attachment. Email is the reporting channel;
this policy does not assume GitHub private vulnerability reporting is enabled.

Include:

- The component, affected version or commit, OS and runtime versions.
- Expected behavior, actual behavior and the potential security or privacy impact.
- Minimal reproduction steps or a proof of concept using synthetic data.
- Relevant configuration and redacted diagnostics, plus known mitigations.

Do not send API keys, session tokens, customer records, IP addresses or other
people's personal data. Reproduce on systems and test accounts you control.
Security and privacy problems in a dependency or the hosted service can also be
reported to this address; identify which component appears affected.

## Handling and disclosure

Taha Bozdemir reviews reports privately, assesses impact and affected versions,
and coordinates a fix or mitigation with the relevant maintainers. Handling is
best effort. We may ask for more information and will share progress when
available; there is no promised response interval or bounty program.

We aim to agree on disclosure timing with the reporter, prepare a tested fix,
and notify affected users through release notes and, when appropriate, a GitHub
security advisory. Please allow coordination before publishing details. We will
ask how you would like to be credited and respect requests for anonymity.

For ordinary bugs and support questions, see [SUPPORT.md](SUPPORT.md).
