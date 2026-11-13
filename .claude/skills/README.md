# Agent Skills

These skills cover lnd's maintenance processes. Each one is a folder with a
`SKILL.md` that gives the trigger (frontmatter `description`) and the workflow,
plus any helper scripts it needs.

Claude Code reads skills from `.claude/skills/`. Codex reads them from
`.agents/skills/`, which is a symlink to this directory, so both tools load the
same files. When you start an agent session from the repo root, the skills load
automatically. You can also invoke one by name, for example `/lnd-backport`.

| Skill | Use it to |
|-------|-----------|
| [`lnd-backport`](lnd-backport/SKILL.md) | Create a backport of a merged PR onto a release branch, or repair a bot `BACKPORT-CONFLICT` draft. It rebuilds the backport commit by commit, resolves conflicts to match the parent, checks dependencies, and writes a commit-by-commit parity table into the PR description. |
| [`cut-release`](cut-release/SKILL.md) | Cut a release or release candidate: fold the `unreleased/` release note fragments, bump the version, create a signed tag with `scripts/tag-release.sh`, walk through the CI, signing and publish steps, and port patch release notes to `master`. |
| [`bump-golang-version`](bump-golang-version/SKILL.md) | Update the Go toolchain version and the minimum Go version across Dockerfiles, workflows, `go.mod` files and `docs/INSTALL.md`. |

Skills that apply beyond lnd (code review, security analysis, Docker
playgrounds, and so on) live in
[lightninglabs/agent-skills](https://github.com/lightninglabs/agent-skills).

## Adding a skill

Create `<name>/SKILL.md` with `name` and `description` frontmatter. Codex
accepts only `name`, `description`, `license`, `allowed-tools` and `metadata`.
Write the description so an agent can tell when to use the skill. Keep the body
to the workflow and the project-specific rules an agent wouldn't otherwise
know. Then add a row to the table above.
