# Automated Backport Workflow

This document describes the automated backport workflow for the LND project.

## Table of Contents

1. [Overview](#overview)
2. [How to Use](#how-to-use)
3. [Workflow Triggers](#workflow-triggers)
4. [Label Format](#label-format)
5. [Workflow Steps](#workflow-steps)
6. [Handling Conflicts](#handling-conflicts)
7. [Multiple Backports](#multiple-backports)
8. [Technical Details](#technical-details)
9. [Troubleshooting](#troubleshooting)

## Overview

The automated backport workflow simplifies the process of backporting merged PRs from the `master` branch to release branches (e.g., `v0.20.x-branch`, `v0.19.x-branch`).

Instead of manually creating branches, cherry-picking commits, and creating PRs, maintainers can simply add a label to the master PR, and the workflow handles the rest.

## How to Use

### Basic Usage

1. **Merge a PR to master** (or have it already merged)
2. **Add a backport label** in the format: `backport-v<version>-branch`
   - Example: `backport-v0.20.x-branch`
3. **The workflow automatically**:
   - Validates the target branch exists
   - Cherry-picks the commits
   - Creates a new PR targeting the release branch
   - Adds the `no-changelog` label (since release notes are in the master PR)

### Example Scenario

```
Day 1, 10:00 - PR #1234 "Fix critical bug" merged to master
Day 1, 10:30 - Add label: backport-v0.20.x-branch
Day 1, 10:31 - Workflow creates PR #1235 automatically
              Title: "[v0.20.x-branch] Backport #1234: Fix critical bug"
              Base: v0.20.x-branch
              Labels: no-changelog
Day 1, 14:00 - Maintainer reviews and merges PR #1235
```

## Workflow Triggers

The backport workflow triggers in two scenarios:

### Scenario 1: Label Before Merge

```
1. Open PR #1234
2. Add label: backport-v0.20.x-branch
3. Review and approve PR
4. Merge PR #1234
5. → Workflow triggers on PR close event
6. → Backport PR #1235 created immediately
```

### Scenario 2: Label After Merge

```
1. Open PR #1234
2. Review, approve, and merge PR #1234
3. Later... decide it needs backporting
4. Add label: backport-v0.20.x-branch
5. → Workflow triggers on label event
6. → Backport PR #1235 created immediately
```

Both scenarios work identically.

## Label Format

### Valid Labels

Labels **must** start with `backport-v` to trigger the workflow:

- ✅ `backport-v0.20.x-branch` → backports to `v0.20.x-branch`
- ✅ `backport-v0.19.x-branch` → backports to `v0.19.x-branch`
- ✅ `backport-v0.18.x-beta-branch` → backports to `v0.18.x-beta-branch`

### Invalid Labels (Will NOT Trigger)

These labels are ignored by the workflow:

- ❌ `backport candidate` - discussion label only
- ❌ `backport-candidate` - doesn't start with `backport-v`
- ❌ `backport-needed` - doesn't start with `backport-v`
- ❌ `needs-backport` - wrong prefix

This allows you to use discussion labels without accidentally triggering backports.

### Label to Branch Mapping

The label format directly maps to the target branch:

```
Label: backport-v0.20.x-branch
           ↓ (removes "backport-" prefix)
Branch: v0.20.x-branch
```

## Workflow Steps

The workflow executes the following steps when triggered:

### Step 1: Checkout Repository

```yaml
- Fetches the full git history
- Checks out the base branch (usually master)
```

### Step 2: Validate Target Branches

```bash
For each backport label:
  1. Extract branch name from label
     backport-v0.20.x-branch → v0.20.x-branch

  2. Check if branch exists in remote repository
     git ls-remote --heads origin v0.20.x-branch

  3. If branch doesn't exist:
     - Log error message
     - Add branch to missing_branches list

  4. After checking all labels:
     - If any branches are missing → FAIL workflow
     - If all branches exist → Continue
```

**Example validation output:**

```
All labels: ["backport-v0.20.x-branch", "bug-fix", "backport-v0.19.x-branch"]
Found backport labels:
backport-v0.20.x-branch
backport-v0.19.x-branch

Checking if branch exists: v0.20.x-branch
✓ Branch 'v0.20.x-branch' exists

Checking if branch exists: v0.19.x-branch
✓ Branch 'v0.19.x-branch' exists

✓ All target branches validated successfully
```

### Step 3: Create Backport PRs

For each valid backport label, the workflow:

1. **Creates a new branch**
   - Branch name: `backport-<pr-number>-to-<target-branch>`
   - Example: `backport-1234-to-v0.20.x-branch`
   - Based on: the target release branch

2. **Cherry-picks commits**
   - Uses `git cherry-pick` (not merge or rebase)
   - Cherry-picks all commits from the original PR
   - Preserves commit messages and authorship
   - Skips merge commits

3. **Creates a new PR**
   - Title: `[v0.20.x-branch] Backport #1234: <original-title>`
   - Base branch: `v0.20.x-branch`
   - Head branch: `backport-1234-to-v0.20.x-branch`
   - Labels: `no-changelog` (automatically added)

4. **PR Description**
   ```markdown
   Backport of #1234

   Original PR: https://github.com/lightningnetwork/lnd/pull/1234

   ---

   [Original PR description here]
   ```

## Handling Conflicts

The workflow handles merge conflicts gracefully using the `draft_commit_conflicts` strategy.

### When Cherry-pick Succeeds

```
1. Cherry-pick completes cleanly
2. Creates regular PR (ready for review)
3. PR is NOT in draft mode
4. Maintainer can review and merge immediately
```

### When Cherry-pick Has Conflicts

```
1. Cherry-pick encounters conflicts
2. Workflow commits the conflict markers:
   <<<<<<< HEAD
   [code from release branch]
   =======
   [code from master PR]
   >>>>>>> commit-hash

3. Creates DRAFT PR
4. PR description indicates there were conflicts
5. Manual resolution required:
   a. git fetch origin
   b. git checkout backport-1234-to-v0.20.x-branch
   c. Resolve conflicts in affected files
   d. git add <resolved-files>
   e. git commit -m "Resolve backport conflicts"
   f. git push origin backport-1234-to-v0.20.x-branch
   g. Mark PR as "Ready for review" in GitHub UI
6. Maintainer reviews and merges
```

### Conflict Resolution Best Practices

- **Review the original PR**: Understand what changed
- **Check the release branch**: Understand why conflicts occurred
- **Test after resolving**: Run tests locally before pushing
- **Update commit message**: Explain what conflicts were resolved and how
- **Request review**: Don't merge without review, even after resolving conflicts

## Multiple Backports

You can backport to multiple release branches simultaneously by adding multiple labels.

### Example: Backport to Two Branches

```
PR #1234 merged with labels:
- backport-v0.20.x-branch
- backport-v0.19.x-branch

Workflow creates TWO backport PRs:

PR #1235:
  Title: [v0.20.x-branch] Backport #1234: Original title
  Base: v0.20.x-branch
  Labels: no-changelog

PR #1236:
  Title: [v0.19.x-branch] Backport #1234: Original title
  Base: v0.19.x-branch
  Labels: no-changelog
```

### Independent Processing

Each backport is processed independently:

- One backport may succeed while another has conflicts
- One backport may fail validation while another succeeds
- Each backport creates a separate branch and PR
- Each backport PR is reviewed and merged independently

### Example with Mixed Results

```
PR #1234 with labels:
- backport-v0.20.x-branch  →  ✓ Clean cherry-pick, regular PR created
- backport-v0.19.x-branch  →  ✗ Conflicts, draft PR created
- backport-v0.99.x-branch  →  ✗ Branch doesn't exist, workflow fails
```

In this case:
1. PR #1235 to v0.20.x-branch is ready for review
2. PR #1236 to v0.19.x-branch needs conflict resolution
3. No PR created for v0.99.x-branch (validation failed)
4. Remove the incorrect label and add the correct one. The workflow will 
   re-trigger when you add the new label.

## Technical Details

### Workflow File

Location: `.github/workflows/backport.yml`

### Trigger Events

```yaml
on:
  pull_request_target:
    types: [closed, labeled]
```

- **closed**: Triggers when PR is closed (checks if merged)
- **labeled**: Triggers when any label is added

### Permissions Required

```yaml
permissions:
  contents: write        # Create branches and commits
  pull-requests: write   # Create and manage PRs
  issues: read          # Read PR metadata
```

### Workflow Condition

```yaml
if: |
  github.event.pull_request.merged == true &&
  contains(join(github.event.pull_request.labels.*.name, ','), 'backport-v')
```

Only runs when:
1. PR is actually merged (not just closed)
2. At least one label contains `backport-v`

### Label Pattern

```yaml
label_pattern: '^backport-(v.+)$'
```

Regex explanation:
- `^` - Start of string
- `backport-` - Literal text
- `(v.+)` - Capture group: "v" followed by one or more characters
- `$` - End of string

Examples:
- `backport-v0.20.x-branch` → matches, captures `v0.20.x-branch`
- `backport-v0.19.x-branch` → matches, captures `v0.19.x-branch`
- `backport-candidate` → doesn't match (no "v" after dash)

### Cherry-pick Strategy

```yaml
merge_commits: skip
```

- Uses `git cherry-pick` for clean history
- Skips merge commits (only cherry-picks actual changes)
- Preserves original commit messages and authorship
- Maintains PGP signatures where present

### Conflict Resolution Strategy

```yaml
experimental: |
  conflict_resolution: draft_commit_conflicts
```

- Creates draft PR with conflict markers
- Allows manual resolution
- Preserves all context and metadata

## Troubleshooting

### Problem: Workflow Doesn't Trigger

**Symptoms:**
- Added `backport-v0.20.x-branch` label
- No workflow run appears in Actions tab

**Possible causes:**

1. **Label format is wrong**
   - ❌ `backport-0.20.x-branch` (missing "v")
   - ✅ `backport-v0.20.x-branch`

2. **PR is not merged**
   - Workflow only runs on merged PRs
   - Check PR status

3. **Workflow is disabled**
   - Check `.github/workflows/backport.yml` exists
   - Check workflow is enabled in Settings → Actions

### Problem: Workflow Fails with "Branch doesn't exist"

**Error message:**
```
Error: Target branch 'v0.21.x-branch' does not exist (from label 'backport-v0.21.x-branch')
Error: The following target branches do not exist: v0.21.x-branch
Error: Please ensure the branch exists before adding the backport label
```

**Solution:**

1. **Verify branch name:**
   ```bash
   git ls-remote --heads origin | grep v0.21
   ```

2. **Check available release branches:**
   ```bash
   git branch -r | grep origin/v0 | grep -v fork
   ```

3. **Fix the label:**
   - Remove incorrect label
   - Add correct label with existing branch name

### Problem: Cherry-pick Has Conflicts

**Symptoms:**
- Backport PR created as DRAFT
- PR description mentions conflicts
- Branch has files with conflict markers

**Solution:**

1. **Fetch and checkout the branch:**
   ```bash
   git fetch origin
   git checkout backport-1234-to-v0.20.x-branch
   ```

2. **Find conflicted files:**
   ```bash
   grep -r "<<<<<<< HEAD" .
   ```

3. **Resolve each conflict:**
   - Open the file in an editor
   - Review both versions:
     ```
     <<<<<<< HEAD
     [Release branch version]
     =======
     [Master PR version]
     >>>>>>> commit-hash
     ```
   - Choose the correct code or merge both
   - Remove conflict markers

4. **Commit the resolution:**
   ```bash
   git add <resolved-files>
   git commit -m "Resolve backport conflicts for PR #1234

   Conflicts occurred due to [explanation].
   Resolution: [describe what you did]"
   git push origin backport-1234-to-v0.20.x-branch
   ```

5. **Mark PR ready for review:**
   - Go to PR on GitHub
   - Click "Ready for review"

### Problem: Multiple Labels but Only One Backport Created

**Symptoms:**
- Added `backport-v0.20.x-branch` and `backport-v0.19.x-branch`
- Only one PR created

**Possible causes:**

1. **One label format is wrong**
   - Check both labels start with `backport-v`
   - Fix incorrect label, workflow will retry

2. **One branch doesn't exist**
   - Check workflow logs for validation errors
   - Verify both branches exist

3. **Workflow still running**
   - Check Actions tab for in-progress runs
   - Wait for workflow to complete

### Problem: Backport PR Missing `no-changelog` Label

**Symptoms:**
- Backport PR created successfully
- CI fails on changelog check

**Solution:**

1. **Manually add the label:**
   - Add `no-changelog` label to the backport PR

2. **Check workflow configuration:**
   - Verify `.github/workflows/backport.yml` has:
     ```yaml
     add_labels: no-changelog
     ```

3. **Re-run the workflow:**
   - Remove and re-add the backport label on original PR
   - New backport PR will have correct label

### Getting Help

If you encounter issues not covered here:

1. **Check workflow logs:**
   - Go to Actions tab
   - Click on the failed workflow run
   - Review step-by-step logs

2. **Check workflow file:**
   - `.github/workflows/backport.yml`
   - Verify configuration matches this documentation

3. **Manual backport:**
   - If automated backport fails repeatedly, you can backport manually:
     ```bash
     # Create branch from target release branch
     git checkout v0.20.x-branch
     git checkout -b manual-backport-123-to-v0.20.x

     # Cherry-pick commits from the original PR
     git cherry-pick <commit-hash>

     # Resolve any conflicts, then:
     git add .
     git commit
     git push origin manual-backport-123-to-v0.20.x

     # Create PR targeting v0.20.x-branch with no-changelog label
     gh pr create --base v0.20.x-branch --label no-changelog
     ```

4. **Report issues:**
   - If you find a bug in the workflow
   - Open an issue with workflow logs and details
