# Contributing

## Releasing

Releases follow [Semantic Versioning](https://semver.org/) and are triggered by pushing a `v*` tag. GitHub Actions cross-compiles `agentsctl` for Linux and macOS, then publishes the binaries to a GitHub Release automatically.

### 1. Choose a version

| Change | Example |
| --- | --- |
| Breaking change | `v2.0.0` |
| Backwards-compatible feature | `v1.1.0` |
| Backwards-compatible bug fix | `v1.0.1` |

### 2. Prepare the release commit message

The release commit message becomes the source for the GitHub Release notes. Line 1 is the release commit subject, line 2 is blank, and lines 3 onward become the GitHub Release body.

```text
release: v1.2.0

\## NEW FEATURE

- abc1234 Add support for a new workflow

\## BUG FIX

- def5678 Fix an existing workflow
```

Use `\##` instead of `##` because Git treats lines starting with `#` as comments when editing a commit message and may strip them. The release workflow converts `\##` back to `##` before publishing the body.

### 3. Create and publish the release

Commit and push the release commit before creating its tag. Push the tag only after the commit push succeeds so the tag-triggered workflow always sees the release commit on the remote branch.

```bash
git commit -m 'release: v1.2.0

\## NEW FEATURE

- abc1234 Add support for a new workflow'
git push origin main
git tag v1.2.0
git push origin v1.2.0
```

The tag push starts GitHub Actions, which builds the Linux amd64, Linux arm64, macOS Intel, and macOS Apple Silicon binaries and creates the GitHub Release with those binaries attached.
