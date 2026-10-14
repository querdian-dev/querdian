# Security Policy

## Reporting a vulnerability

Please do **not** open a public issue for security problems. Use GitHub's private vulnerability
reporting:
<https://github.com/querdian-dev/querdian/security/advisories/new>

Include affected version or commit, a description, and reproduction steps. We aim to acknowledge
reports within 3 working days and to agree on a disclosure timeline with the reporter.

## Supported versions

The project is pre-alpha; only the latest `main` is supported.

## Scope

Querdian is an access-control component. Any way to read data outside a user's effective policy
(query rewriting bypass, parser divergence, tenant header injection, response merge leaks) is
considered a vulnerability.
