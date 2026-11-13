---
name: lnd-backport
description: Create or repair a backport of an lnd PR onto a release branch (v0.20.x-branch, v0.21.x-branch, ...). Use when the user asks to backport a PR, fix or resolve a backport PR (especially a bot "BACKPORT-CONFLICT" draft), or "properly apply the parent" on a backport. Rebuilds the backport commit by commit with cherry-pick -x, resolves conflicts so they match the parent, checks dependencies, vets and tests the result, and posts a commit-by-commit parity table in the PR description.
---

# lnd Backport

A backport is correct when every commit matches its parent commit except where
the release branch makes that impossible, and when every such difference is
named in the PR description. The table is how reviewers check this, so treat it
as required output, not decoration.

Two entry points:

- **Repair a bot backport.** `korthout/backport-action` opens
  `[<branch>] Backport #N: <title>` on the `lightningnetwork` repo, on branch
  `backport-N-to-<branch>`, labelled `no-changelog`. When a commit conflicts it
  pushes a draft with a single ` BACKPORT-CONFLICT` commit that holds only part
  of the change. Never fix that commit in place. Rebuild from the parent.
- **Create a new backport.** Same rebuild, then push it to the fork and open the
  PR yourself (see step 7).

The scripts live next to this file in `.claude/skills/lnd-backport/scripts/`.
Set `$S` from a checkout of `master`, before you create any worktree, because
release branches don't contain the skill. `$SP` is any scratch directory
outside the repo.

```bash
S=$(git rev-parse --show-toplevel)/.claude/skills/lnd-backport/scripts
SP=$(mktemp -d)
```

## 1. Gather the facts

Get the parent PR, the target branch(es), and, when repairing, the existing
backport PR's head SHA. Record that SHA; the force-push lease uses it.

```bash
gh pr view <BACKPORT_PR> --repo lightningnetwork/lnd \
  --json baseRefName,headRefName,headRefOid,commits,body
git fetch -q origin <target-branch> master
$S/parent_commits.sh <PARENT_PR> > $SP/commits-<PARENT_PR>.txt
```

`parent_commits.sh` uses the merge commit's `^1..^2` range and checks its commit
count against the PR. Use its output as the only list of commits. The bot
cherry-picks those same commits (`merge_commits: skip`).

## 2. Check dependencies before cherry-picking

Read the parent body for "Builds on", "Depends on", and "Requires". For each
dependency:

- **lnd PR:** confirm its backport is merged on the target branch, e.g.
  `git log --oneline origin/<branch> --grep '<N>'`. If it isn't, stop and tell
  the user. Don't fold it in.
- **btcwallet/btcd PR:** compare the target branch's `go.mod` pin against the
  maintenance branch (v0.20.x uses `ver-0-16`, v0.21.x uses `ver-0-16.19`, and
  master uses btcwallet master). Look at
  `gh api 'repos/btcsuite/btcwallet/commits?sha=<pin>&per_page=10'` for a
  "backport-<N>" merge. If the parent bumps a dependency and the backport
  can't, test the behaviour that bump provided. A clean compile is not enough.

## 3. Rebuild in an isolated worktree

Never rebuild in a checkout that you or another agent session might switch to
another branch midway through. A long `go test` would then mix sources from both
branches. Always work in a detached worktree of your own:

```bash
W=$SP/bp<PARENT_PR>-<branch-short>
git worktree add -q --detach $W origin/<target-branch>
cd $W && git checkout -q -b bp<PARENT_PR>-local
for c in $(cut -d' ' -f1 $SP/commits-<PARENT_PR>.txt); do
  git cherry-pick -x $c >/dev/null 2>&1 \
    && echo "OK $(git log -1 --format=%s)" \
    || { echo "CONFLICT at $c"; git status --short | grep -v '^[MAD] '; break; }
done
```

After resolving each conflict, run
`go build ./... && go vet <touched pkgs>`, then
`git add -A && GIT_EDITOR=true git cherry-pick --continue`, then restart the
loop from the next commit. For several target branches, rebuild each one
separately. Apply the same resolution only where `git diff` shows the same
conflict.

## 4. Resolve conflicts against the parent commit, not the incoming hunk

Always compare against `git show <parent-sha> -- <file>`. The incoming side of a
conflict hunk can carry lines from earlier master-only PRs. Recurring cases:

| Conflict shape | Resolution |
|---|---|
| Import block, master uses `btcutil/v2`, `chainhash/v2` or `wire/v2` | Keep the branch's v1 imports. Add only the new non-v2 imports the parent adds. Use `$S/resolve_v2_imports.py <files>` then `gofmt -w`. |
| Branch-only context such as `tc := tc` or `outpoint := outpoint`, and the incoming side is empty | Keep the branch side. Don't import master's cleanups. |
| The parent rewrites a whole block that contains branch-only lines | Take the parent's block. The branch-only lines go away with it. Note this in the table. |
| The incoming hunk includes fields, functions or tests that the parent doesn't add | Take only what the parent adds. Find where the extra code came from with `git log -S'<symbol>' origin/master -- <file>`, and name that commit in the table. |
| Comments, doc text, release-note fragment (`docs/release-notes/unreleased/<N>.md`) | Copy the parent verbatim, even when a commit is dropped or a code path can't be reached on the branch. |

Check: once a conflicted commit is resolved,
`git diff HEAD --shortstat` (before `--continue`) should match
`git show --shortstat <parent>`. If it doesn't, explain the gap before moving
on.

New test files can cherry-pick cleanly and still import btcd v2. Run `go vet`
including tests on every touched package.

## 5. Verify

```bash
$S/parity.sh $SP/commits-<PARENT_PR>.txt origin/<target-branch> > $SP/parity-<branch>.md
go vet <touched pkgs>/...
go test -count=1 <touched pkg>/...   # one package at a time
```

- Run `htlcswitch` tests with `-tags dev`. Run Postgres-backed suites
  (`invoices`, etc.) one at a time.
- `parity.sh` prints SAME or DIFF for each commit using `git patch-id --stable`,
  and the line-level difference for every DIFF commit. "same changed lines; only
  hunk context differs" means the difference is context only (imports or nearby
  branch-only code).
- It also prints the aggregate difference between the whole parent and the
  whole backport. Every line it reports must trace to a forced resolution in
  step 4. Revert anything you can't justify.

## 6. Write the PR description

Keep the bot's format and insert a parity section after the first line:

```
Backport of #N

## Backport parity with #N

All K parent commits are cherry-picked in order with `-x`. X are identical
(same `git patch-id`). The other Y differ only where <branch> forces it:
<one-line summary of the forced categories>.

| # | Parent | Backport | Subject | Difference from parent |
|---|--------|----------|---------|------------------------|
| 1 | <10-char parent> | <9-char backport> | <subject> | Identical |
| 2 | ... | ... | ... | <Exact difference, and why the branch forces it> |

Overall, `git diff <branch>..HEAD` and the parent's merge diff
(`<base>..<merge>`) touch the same N files with +A lines. The only
differing lines are <...>.

**Dependencies on <branch>:**
- #X: backported as #Y (merged).
- btcsuite/btcwallet#Z: backported to `<maint branch>` as btcsuite/btcwallet#W. This is the pinned `<version>`.

**Verification:** `go vet` and `go test` pass for `<pkgs>`.

---

<parent PR body, unchanged>
```

Write the "Difference" column by hand from `parity.sh`'s DIFF sections. Name
each difference concretely: the file, the symbol, and the master-only commit
that caused it. Never write "conflict resolved". Use backport SHAs only after
you push, because rebasing changes them.

Save the body to `$SP/pr<BACKPORT_PR>_body.md`, and apply it with
`gh pr edit <BACKPORT_PR> --repo lightningnetwork/lnd --body-file <file>`.

## 7. Push

Pushing is outward-facing. Do it only when the user asked for it in this
conversation, for example "force push both PRs". If a permission check blocks a
force-push, stop and give the user the exact command to run.

- **Repairing a bot PR:** its branch lives on `origin`. Force-push with a lease
  on the head SHA you recorded in step 1:
  ```bash
  git push --force-with-lease=backport-N-to-<branch>:<recorded-sha> \
    origin HEAD:refs/heads/backport-N-to-<branch>
  ```
- **New backport:** `origin` (`lightningnetwork/lnd`) doesn't allow new
  branches. Push to your fork (the remote is called `fork` here) and open the PR
  from there:
  ```bash
  git push -u fork HEAD:refs/heads/backport-N-to-<branch>
  gh pr create --repo lightningnetwork/lnd --base <branch> \
    --head <your-gh-user>:backport-N-to-<branch> \
    --title "[<branch>] Backport #N: <parent title>" \
    --label no-changelog --body-file <file>
  ```
  Add the `no-changelog` label when you create the PR. A label added later
  doesn't re-trigger the release-notes check.

Then update the description (step 6), and confirm with
`gh pr view <PR> --json headRefOid,commits` that the head matches your local
HEAD and the commit count matches the parent.

## 8. After pushing

Check that CI actually runs on the new head with
`gh pr checks <PR> --repo lightningnetwork/lnd`. Two or three checks, or only
"cancelled" ones, mean CI didn't run. Tell the user, because backports have been
merged untested before. If a draft bot PR is now ready, say so and let the user
mark it ready for review. Remove your worktree when you're done:
`git worktree remove $W`.

## Report back

Give the user both PR links and heads, the identical and differing commit
counts, each forced difference in one line, the dependency status, and the test
result. Don't report a task as done if a push or description update was blocked.
