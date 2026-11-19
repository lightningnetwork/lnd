---
name: cut-release
description: Cut an lnd release or release candidate. Use when the user wants to release a version (e.g. "release 0.20.3", "cut rc2 for 0.21.2"), bump build/version.go for a release, fold the docs/release-notes/unreleased fragments into the release notes, port patch release notes to master, create/push a signed release tag, or walk the post-tag publish and signing steps. Always asks which version and whether it is an rc or the final, and checks the series is still supported. Covers both major (0.X.0) and patch (0.X.Y) releases.
allowed-tools: Read, Edit, Bash, Grep, Glob, AskUserQuestion
---

# Cut an lnd Release

Releases happen on the release branch for the version series
(`v0.20.x-branch`, `v0.21.x-branch`, …), never on `master`. Master only ever
carries the `.99` development version.

The release itself is driven by **one signed tag pushed to
`lightningnetwork/lnd`**. Everything after that — building artifacts, creating
the draft GitHub release, building the Docker image — is CI. So the job splits
into: get the release branch into the exact shape you want tagged, tag it, push
the tag, then shepherd the draft release through signing and publishing.

## Step 1: Ask which release is being cut — always

**Never infer the version or the release kind.** The two answers drive every
later step: an rc carries a `beta.rcN` suffix in *both* `build/version.go` and
the tag, a final carries a bare `beta` in both, and getting it wrong means a
published tag that has to be retracted. Ask even when the user's request looks
unambiguous ("release 0.21.2" does not say rc or final), and ask before touching
any file.

Gather the state first so the questions are grounded and the options concrete:

```bash
git fetch origin
# Which series are alive, and what has already been released on each?
git branch -r --list 'origin/v0.2*-branch'
git tag --list 'v0.2*' --sort=-v:refname | head -20
# What does master think the development version is (tells you the next minor)?
git show origin/master:build/version.go | sed -n '40,56p'
```

Then ask the user, as a single question with concrete choices derived from the
state above:

1. **Which version?** e.g. `0.21.2`, `0.22.0`. Offer the plausible next
   versions per live series, not a free-form prompt.
2. **Release candidate or final?** If rc, which rc number — derive the default
   from the highest existing `rcN` tag for that version and offer the next one.

With both answers, derive and state back what you are about to do, so a wrong
answer is caught before the bump PR:

| Answer | `AppPreRelease` | Tag | Release branch |
|---|---|---|---|
| `0.21.2` rc2 | `beta.rc2` | `v0.21.2-beta.rc2` | `v0.21.x-branch` |
| `0.21.2` final | `beta` | `v0.21.2-beta` | `v0.21.x-branch` |

The release branch is always derived from the minor version, never asked:
`0.21.2` → `v0.21.x-branch`.

### Check the series is still supported

lnd maintains patch releases for the **two most recent minor series only**. Once
`0.22.0` ships, `0.22.x` and `0.21.x` receive patch releases and `0.20.x` is end
of life. Determine the current pair from the tag list above (or from master's
`.99` version: master on `0.22.99-beta` means `0.22` is the newest series).

If the requested version is on an out-of-support series, **stop and tell the
user before doing any work** — say which series are currently supported and ask
them to confirm they really want a release on an EOL branch. Only proceed on an
explicit confirmation; a security backport to an old series is a real case, but
it is the user's call, not an assumption.

### Then check the branch state for that version

```bash
git show origin/<release-branch>:build/version.go | sed -n '40,56p'
git tag --list 'v0.21.2*'
git log --oneline <last-rc-tag>..origin/<release-branch>
```

That last command matters: it shows what landed since the previous rc. If real
code changed since the last rc, a final is probably wrong and another rc is
needed. Raise it with the user — do not decide it silently, and do not override
their answer either; just make sure they are choosing with the diff in view.

## Step 2: Fold the release note fragments

Release notes are no longer written in a shared document while development is
going on. Each PR adds a fragment at
`docs/release-notes/unreleased/<PR>.md`. The fragment has no version, and the
backport bot carries it to the release branch along with the change.
Preparing a release means **folding** those fragments into
`docs/release-notes/release-notes-<version>.md`. `docs/release-notes/README.md`
on `master` describes the process in full.

The fold is part of every release, **rc and final alike**. CI
(`scripts/check-release-notes-consistency.sh`, which ignores `no-changelog`)
rejects a PR that bumps `build/version.go` to a release version unless two
things hold: `release-notes-<version>.md` exists, and `unreleased/` contains
nothing except `.gitkeep`. A final release also needs a non-empty contributors
section.

The fold tool is a Go program in `scripts/release-notes`, run with
`make release-notes`. It exists **only on `master`**; the release branches have
the CI checks but not the tool. The tool uses only the standard library and works
on relative paths from the repo root, so build it from a `master` checkout and
run the binary at the root of the release-branch checkout:

```bash
# In a master checkout (major release: just use make there instead).
go build -o /tmp/release-notes ./scripts/release-notes

# At the root of the bump branch (created in Step 3), with tags fetched,
# because contributors are generated from <from>..HEAD.
git fetch origin --tags
/tmp/release-notes <version> [<from-tag>]     # e.g. 0.21.9
# On master for a major release:
make release-notes version=0.22.0 [from=v0.21.9-beta]
```

The tool does the following:
- **Notes file:** it creates `release-notes-<version>.md` from the template if
  it's missing, or extends the existing one.
- **Already-announced fragments:** it deletes fragments whose PR another
  release note document already announces.
- **Folding:** it appends each remaining fragment under its matching heading,
  in PR order, and deletes the fragment. It keeps `unreleased/.gitkeep`.
- **Contributors:** it merges in the git authors (`%aN`) of `<from>..HEAD`.
  `from` defaults to the newest final tag of a lower version, such as
  `v0.21.8-beta` for `0.21.9`.
- **Re-runs:** it can run again, e.g. for fragments that land after an rc,
  without adding duplicate entries or contributors.
- **Failure:** if a fragment uses a heading that the document doesn't have, it
  fails without changing anything.

Review the result before committing. Keep each entry's wording and PR link, and
only adjust ordering and transitions. Check co-authored commits and the
contributor list. If one person appears under two names, fix it with a `.mailmap`
entry rather than editing the generated list.

## Step 3: The version bump PR

The commit that carries the version bump is the commit that gets tagged, so it
must be merged upstream first. Never bump directly on the release branch.

Branch naming convention (follow the existing ones):

| Release step | Branch | Example PR |
|---|---|---|
| rc | `bump-v0.21.2` | #11043 |
| final | `bump-v0.21.2-final` | #10953, #11062, #11063 |

```bash
git checkout -b bump-v0.21.2-final origin/v0.21.x-branch
```

The bump PR carries two things: the version change in `build/version.go` and
the fold from Step 2 (the new or extended `release-notes-<version>.md`, plus
the deleted fragments). Put the fold in its own `docs:` commit before the
`build:` commit. The later port to `master` is then easy to check against it.

The version change is one line in the common case:

- **rc → next rc:** `AppPreRelease = "beta.rc1"` → `"beta.rc2"`
- **rc → final:** `AppPreRelease = "beta.rc1"` → `"beta"`
- **new patch series:** bump `AppPatch` and set `AppPreRelease = "beta.rc1"`

`AppPatch` is written with a leading zero up to 7 (`uint = 02`), but a leading
zero makes it a Go **octal** literal: `08` and `09` do not compile
(`invalid digit '8' in octal literal`, caught by the Static Checks job). From 8
on, write the patch in plain decimal (`uint = 8`, `uint = 10`), as releases
before the zero-padding did. `tag-release.sh` parses the value as base 10
either way.

Commit, push to the **fork** (`origin` is upstream and rejects new branches),
and open the PR against the release branch:

```bash
git add -A docs/release-notes
git commit -m "docs: fold release notes for v0.21.2"
git commit -am "build: bump version to v0.21.2"
git push -u fork bump-v0.21.2-final
gh pr create --repo lightningnetwork/lnd \
  --head <your-gh-user>:bump-v0.21.2-final \
  --base v0.21.x-branch \
  --label no-changelog --label no-itest \
  --title "build: bump version to v0.21.2" \
  --body "Bumps the version from \`0.21.2-beta.rc1\` to \`0.21.2-beta\` and folds the release note fragments in \`docs/release-notes/unreleased\` into \`release-notes-0.21.2.md\`."
```

The commit subject is `build: bump version to <tag-without-beta-suffix-noise>`,
matching history (`build: bump version to v0.20.2`,
`build: bump version to v0.21.2-beta.rc1`).

Both labels matter. `no-changelog` skips the per-PR release-notes check: the
bump has no fragment of its own. The consistency check still runs either way.
`no-itest` skips the integration-test suite. Apply both labels when you create
the PR, because the changelog check only honors the label if it's present when
the PR event fires.

**Re-fold right before merging.** Backports keep landing on the release branch
while the bump PR waits, and each one brings a new fragment. Just before the
merge, rebase onto the latest release branch and check
`git ls-tree --name-only origin/<release-branch> docs/release-notes/unreleased/`.
If any fragment besides `.gitkeep` is there, run the fold again and push.
v0.21.7 went wrong this way: the bump PR wasn't rebased after #11540's backport
merged, so its fragment was never folded, and the notes had to move to v0.21.8
(#11565).

## Step 4: Create and push the signed tag

Wait for the bump PR to merge, then check out the release branch at the merge
commit and use the helper — it exists so you cannot tag the wrong thing:

```bash
git fetch origin
git checkout v0.21.x-branch
git merge --ff-only origin/v0.21.x-branch
scripts/check-release-notes-consistency.sh release
./scripts/tag-release.sh v0.21.2-beta --branch v0.21.x-branch
```

The consistency check confirms that the commit you're about to tag has
`release-notes-<version>.md` and an empty `unreleased/`. On `master`,
`tag-release.sh` runs this check itself (#11562). The release branches' copies
of `tag-release.sh` predate that change, so run it by hand there. If it fails,
don't tag: fold the leftover fragments in a new PR first. A tag pushed on an
unready commit is burned, since `release.yaml` runs the same check against the
tagged commit.

Use `merge --ff-only` rather than a plain `pull`: if the local branch has drifted
it fails loudly instead of building a merge commit that would then get tagged.

Confirm the tag landed on the merge commit and carries a good signature before
going any further:

```bash
git verify-tag v0.21.2-beta
git show --no-patch --format='%H %s' v0.21.2-beta^{commit}
```

`scripts/tag-release.sh` refuses to tag unless the tag does not already exist
locally or upstream, `HEAD` is exactly equal to upstream
`lightningnetwork/lnd/<branch>` (it fetches first, and identifies the upstream
remote by URL rather than by the name `origin`), and the four constants in
`build/version.go` at `HEAD` reconstruct precisely the requested tag. It then
creates a **signed annotated** tag (`git tag -s`) and prints the push command
rather than pushing — the push is a deliberate maintainer action.

Tag naming: always the full `v<major>.<minor>.<patch>-beta[.rcN]` form, e.g.
`v0.21.2-beta.rc1`, `v0.21.2-beta`.

Then push it. **This is the point of no return** — pushing a `v*` tag triggers
the release. Confirm with the user before running it:

```bash
git push origin v0.21.2-beta
```

**The push output is misleading.** Upstream rulesets restrict ref creation, so
the remote prints `Cannot create ref due to creations being restricted` even
when the tag was created successfully — the same push also reports
`* [new tag]`. Do not retry on the strength of that message. Verify against the
remote instead:

```bash
git ls-remote --tags origin 'refs/tags/v0.21.2*'
```

The `refs/tags/<TAG>^{}` line must show the commit you tagged.

## Step 5: What CI does with the tag

Pushing a tag matching `v*` fires two workflows:

- `.github/workflows/release.yaml` — builds every architecture with
  `make release tag=<TAG>` and creates a **draft** GitHub release containing
  the binaries and `manifest-<TAG>.txt`. The draft body has a literal
  `# Database Migrations` / `TODO` placeholder that a maintainer must fill in
  (or delete) before publishing.
- `.github/workflows/docker.yml` — builds and pushes
  `lightninglabs/lnd:<TAG>` to Docker Hub, after checking PGP key expirations.
  Newly added `scripts/keys/*.asc` files are worth watching here, since an
  expired or malformed key fails this job.

Confirm both actually started — a tag push that silently matched nothing is
worse than a failed build:

```bash
gh run list --repo lightningnetwork/lnd --limit 8 \
  --json workflowName,headBranch,status,conclusion,event \
  --jq '.[] | select(.event=="push") | "\(.workflowName)\t\(.headBranch)\t\(.status)/\(.conclusion // "-")"'
```

## Step 6: Sign the manifest and publish

The release is only trustworthy once a quorum of maintainer signatures is
attached. Each signer independently reproduces the build and signs the
manifest — see `docs/signing_release_candidate.md` and
`docs/release.md`. In short, per signer:

```bash
git checkout <TAG>
make docker-release tag=<TAG>        # or `make release tag=<TAG>` on Linux/WSL
diff lnd-<TAG>/manifest-<TAG>.txt ~/Downloads/manifest-<TAG>.txt
gpg --detach-sig --output manifest-<USERNAME>-<TAG>.sig manifest-<TAG>.txt
```

Only sign if the diff is empty. `<USERNAME>` must match the signer's name in
`scripts/verify-install.sh` and their key file in `scripts/keys/`. Signature
files are uploaded to the GitHub release page; signers coordinate over Keybase
so two people do not edit the release page at once.

Publishing the release (undrafting it) triggers
`.github/workflows/verify-release.yaml`, which runs `/verify-install.sh <TAG>`
inside the published Docker image and **flips the release back to draft if
verification fails**. It can also be re-run manually via `workflow_dispatch`
with the version as input.

Because that check pulls `lightninglabs/lnd:<TAG>`, the Docker job from Step 5
must have finished before you publish — otherwise verification fails on a
missing image and re-drafts an otherwise good release.

## Step 7: Follow-ups

- **Port a patch release's notes to `master`** in a `docs:` PR, for example
  #11567 "docs: port latest release notes to master". Copy
  `release-notes-<version>.md` from the release branch
  (`git checkout origin/<release-branch> -- docs/release-notes/release-notes-<version>.md`),
  and delete every fragment on `master` whose PR it announces. Those changes
  are released now. Without the port, `master` keeps fragments that a later
  major-release fold would announce a second time. Once the port is in,
  `master`'s consistency check fails any PR that leaves an announced fragment
  behind. The port carries `no-changelog`.
- For a **major** release only: the release branch now enters maintenance mode
  for patch releases, and `master` should already be on `<next-minor>.99-beta`
  from when the branch was created.

## Reference docs in-repo

- `docs/release_branch_management.md` — branch model, milestones, backport
  automation, version-bump timing.
- `docs/release.md` — reproducible build system, verifying a release, signing
  an existing manifest.
- `docs/signing_release_candidate.md` — signer onboarding and the signing flow.
- `scripts/tag-release.sh` — the guarded tagging helper.
- `docs/release-notes/README.md` (on `master`) — the fragment workflow and the
  fold.
- `scripts/release-notes/` (on `master`) — the fold tool.
- `scripts/check-release-notes-consistency.sh` — the release-readiness check
  (`pr` and `release` modes).

## Common mistakes

- Assuming rc vs final from the phrasing of the request. Ask (Step 1). An rc
  needs `beta.rcN` in `build/version.go` **and** `-beta.rcN` in the tag; a final
  needs a bare `beta` in both. `tag-release.sh` will refuse a mismatch, but only
  after the bump PR has already merged.
- Cutting a patch release on an end-of-life series. Only the two newest minor
  series get patch releases; confirm with the user before touching an older one.
- Tagging a commit that is not yet merged upstream, or tagging local `HEAD`
  that is ahead of the release branch. `tag-release.sh` catches both; do not
  work around it with a bare `git tag`.
- Using an unsigned tag. Releases require `git tag -s`; the helper does this.
- Pushing the bump branch to `origin` — `origin` is `lightningnetwork/lnd` and
  disallows new branches. Push to `fork`.
- Bumping to final when code landed after the last rc. Cut another rc instead.
- Forgetting `no-changelog`/`no-itest` at PR creation time.
- Editing `release-notes-<version>.md` by hand instead of folding, or
  treating the fold as a final-only step. rc bumps need it too.
- Merging the bump PR without re-checking `unreleased/` on the release branch.
  A backport that merged in the meantime leaves a fragment behind, and the
  version can't be tagged.
- Tagging on a release branch without running
  `scripts/check-release-notes-consistency.sh release` first. That branch's
  `tag-release.sh` doesn't check it.
