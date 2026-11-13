---
name: cut-release
description: Cut an lnd release or release candidate. Use when the user wants to release a version (e.g. "release 0.20.3", "cut rc2 for 0.21.2"), bump build/version.go for a release, create/push a signed release tag, or walk the post-tag publish and signing steps. Always asks which version and whether it is an rc or the final, and checks the series is still supported. Covers both major (0.X.0) and patch (0.X.Y) releases.
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

## Step 2: Make sure the release notes are ready

Release notes for a version live on the release branch at
`docs/release-notes/release-notes-<version>.md` (e.g.
`release-notes-0.21.2.md`). Before a **final** release, check that:

- Every backported PR in the release is mentioned. CI enforces this per-PR via
  `scripts/check-release-notes.sh` (a PR must reference its own number under
  `docs/release-notes/`, unless it carries the `no-changelog` label), so gaps
  usually come from manually cherry-picked commits.
- The **Contributors (Alphabetical Order)** section lists everyone.

Release notes for patch versions also get ported to `master` in a separate
`docs:`-prefixed PR (see `bd6e91a36 docs: add 0.20.2 and 0.20.3 release notes
to master`). That port is independent of the tag and can happen after.

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

Edit **only** `build/version.go` — one file, one line changed in the common
case:

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
git commit -am "build: bump version to v0.21.2"
git push -u fork bump-v0.21.2-final
gh pr create --repo lightningnetwork/lnd \
  --head <your-gh-user>:bump-v0.21.2-final \
  --base v0.21.x-branch \
  --label no-changelog --label no-itest \
  --title "build: bump version to v0.21.2" \
  --body "Bumps the version from \`0.21.2-beta.rc1\` to \`0.21.2-beta\`, dropping the release candidate suffix for the final v0.21.2 release."
```

The commit subject is `build: bump version to <tag-without-beta-suffix-noise>`,
matching history (`build: bump version to v0.20.2`,
`build: bump version to v0.21.2-beta.rc1`).

Both labels matter: `no-changelog` skips the release-notes check, `no-itest`
skips the integration-test suite for a one-line constant change. Apply them at
PR creation — the changelog check only honors the label if it is present when
the PR event fires.

## Step 4: Create and push the signed tag

Wait for the bump PR to merge, then check out the release branch at the merge
commit and use the helper — it exists so you cannot tag the wrong thing:

```bash
git fetch origin
git checkout v0.21.x-branch
git merge --ff-only origin/v0.21.x-branch
./scripts/tag-release.sh v0.21.2-beta --branch v0.21.x-branch
```

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

- Port the release notes to `master` if not already done (Step 2).
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
