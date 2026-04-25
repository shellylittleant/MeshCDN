# Contributing to MeshCDN

Thanks for your interest. MeshCDN is alpha software and bug reports + small fixes are especially valuable right now.

## Reporting issues

Before opening an issue, please include:

1. **Version**: output of `cdn-agent --version` or check `journalctl -u meshcdn | grep "启动中" | tail -1`
2. **Cluster size**: how many nodes? Single-node or multi-node?
3. **What you tried**: exact Telegram command or `cdn-agent exec` invocation
4. **What you expected vs what happened**
5. **Logs**: `sudo journalctl -u meshcdn -n 200 --no-pager` (redact bot tokens, IPs you don't want public)

For security-sensitive issues (e.g. anything that could expose other users' data), please email the maintainers privately rather than opening a public issue. Contact info will be added here once the project has a stable maintainer team.

## Submitting changes

### Workflow

1. Fork the repo
2. Create a topic branch (`git checkout -b fix/short-description`)
3. Make changes, run `gofmt -w .` and `go vet ./...`
4. Commit with a descriptive message
5. Sign off your commits (see DCO below)
6. Open a Pull Request against `main`

### Commit signoff (DCO)

We use [Developer Certificate of Origin](https://developercertificate.org/). Add `-s` to your commit:

```bash
git commit -s -m "fix: handle empty result in viewSSLDetail"
```

This adds a `Signed-off-by:` line, certifying that you have the right to contribute the change.

### Code style

- Run `gofmt -w .` on all changes
- Run `go vet ./...` and fix any warnings
- Follow Go idioms; if a file is using existing patterns (e.g. error returns, naming), match them rather than introducing new conventions
- Comments may be in English or Chinese — both are common in the codebase. New code should generally have English doc comments at package and exported function level; inline comments can be in whichever language explains the intent best
- Keep PRs focused. One logical change per PR

### Tests

The project doesn't currently have a comprehensive test suite (one of the things planned for v3.2). For now:
- Manual verification on a single-node deployment is the minimum bar
- If you're touching mesh code, please verify on a 2+ node setup
- Include reproduction steps in the PR description

### What we're looking for

Good first contributions:
- Documentation improvements (especially English clarifications of existing Chinese comments)
- Bug fixes with clear reproduction
- Small UX improvements (button copy, error messages)
- Adding tests for existing functionality

Please discuss before working on:
- Schema changes
- New commands or new top-level features (`/w newthing`)
- Changes to mesh protocol wire format
- Anything that requires migration of existing deployments

Open an issue first, get a thumbs up, then implement. This saves you wasted work if the feature isn't a fit for the project's direction.

---

## Code of conduct

Be respectful. Disagreements happen — argue about the code, not the person. We'll add a longer CoC if the project grows large enough to need one.

## Questions?

Open a GitHub Discussion (when enabled) or an issue tagged `question`.
