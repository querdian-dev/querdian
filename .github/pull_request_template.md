## What and why

## Checklist

- [ ] Tests added or updated (unit; `test/e2e` if backend behaviour is involved)
- [ ] `make lint test-race` green; `make generate manifests` has no diff
- [ ] No new dependency outside the licence allowlist (`make licenses`)
- [ ] Docs updated (`docs/`, `CHANGELOG.md` if user-visible)
- [ ] Commits are signed off (`git commit -s`)
- [ ] Clean-room rule respected (no Loki/Tempo/janus source consulted; see CONTRIBUTING.md)
