---
name: release
description: Create and publish an agentsctl release for a user-provided semantic version tag.
---

# Release

## Purpose

Prepare reviewed release notes, create the release commit and tag in the required order, and verify the GitHub Release.

For the pipeline and commit-message format, see [`CONTRIBUTING.md#Releasing`](../../../CONTRIBUTING.md#releasing).

## Input

- `version`: the semantic version tag requested by the user, such as `v1.2.0`

## Workflow

### 1. Gather changes

Find the previous tag:

```bash
git describe --tags --abbrev=0
```

When it succeeds, inspect commits after that tag:

```bash
git log --oneline <previous-tag>..HEAD
```

When no previous tag exists, treat this as the initial release and inspect history from the repository's first commit through `HEAD`:

```bash
git log --oneline --reverse HEAD
```

Summarize the user-important changes rather than listing every commit mechanically. Include the short hash for each summarized change.

### 2. Draft and review release notes

Use this format:

```text
release: <version>

\## <SECTION>

- <short hash> <description>
```

Present the complete draft to the user and stop. Do not create the release commit or tag until the user explicitly approves the notes.

### 3. Create the approved release commit

After approval, use the reviewed notes as the complete commit message. If the release commit has no tree changes, use `--allow-empty`.

Use `\##` in the commit message because Git may strip lines beginning with `#`. The release workflow converts it to `##` in the published body.

### 4. Push the commit first

Push the release commit and verify that the push succeeds before creating a tag:

```bash
git push origin main
```

### 5. Create and push the tag separately

Only after the commit push succeeds, create the tag on that release commit and push that specific tag in a separate operation:

```bash
git tag <version>
git push origin <version>
```

Never overwrite an existing tag or release, and never force-push.

### 6. Verify the release

Confirm that the tag-triggered Release workflow succeeds, the GitHub Release body matches the approved notes with `\##` converted to `##`, and these assets are published:

- `agentsctl-linux-amd64`
- `agentsctl-linux-arm64`
- `agentsctl-darwin-amd64`
- `agentsctl-darwin-arm64`
