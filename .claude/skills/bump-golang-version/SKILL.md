---
name: bump-golang-version
description: Updates the Go versions lnd pins. Use when bumping the Go toolchain (build/CI/Docker/release) version or the minimum Go version (go.mod language version and docs/INSTALL.md), or when checking that all Go version pins are consistent. Drives the scripts/bump-go Go tool (make bump-go), which finds every pin, rewrites it, fetches the official archive checksums from go.dev and verifies the result.
allowed-tools: Read, Edit, Grep, Glob, Bash
---

# Bump the Go Version

lnd pins two Go versions, and the Go tool in `scripts/bump-go` handles both. Run
it from the repo root with `make bump-go` or `go run ./scripts/bump-go`.

- **Toolchain:** the Go release used to build lnd. It appears as Makefile
  `GO_VERSION`, in the CI and release workflows, in every `golang:` Docker image,
  in `.golangci.yml`, and in the build docs (`mobile/README.md`,
  `docs/ci-runs.md`, the setup-go cache-key example). One pin holds only the
  major release: "with Go 1.27 only" in `.github/actions/setup-go/action.yml`.
- **Minimum:** the `go` directive in every tracked `go.mod`, which is the oldest
  Go that can build lnd. `docs/INSTALL.md` names it and lists the official
  archive URLs and SHA256 checksums for it.

The tool does all the edits. Don't change version strings by hand, and never
type a checksum: the tool finds each checksum in `INSTALL.md` by its value on
go.dev (`https://go.dev/dl/?mode=json&include=all`), whatever the line format,
and swaps in the matching archive's hash for the new version.

The tool has been checked against lnd's history:
- **Recent bumps:** every bump since the pins became consistent reproduces
  byte for byte. Those are `df48bd51c6`, `b1f9ca66d` (a major bump, 1.26 →
  1.27), `d67c9edf8` and `aab1e4661`. `scripts/bump-go/replay_history.sh`
  re-runs that check, and `go test ./scripts/bump-go` covers each rule offline.
- **Older bumps:** replaying them showed pins the real PRs had missed:
  `INSTALL.md` and `tools/linters/go.mod` in `7bc88e8360`,
  `gen_protos_docker.sh` in `1c76c6198c`, and `kvdb`/`sqldb` `go.mod` in
  `19b5a1fb05`.
- **Forward bump:** the bump to 1.27.4 (#11587) built, passed `make lint` with
  the new Docker images, and pointed only at Docker tags and URLs that exist.

## Step 1: Inspect the current state

```bash
make bump-go          # same as: go run ./scripts/bump-go status
```

`status` prints every pin with its file and line. It compares each pin against
its anchor: Makefile `GO_VERSION` for the toolchain, the root `go.mod` for the
minimum. It fails in three cases:
- a pin disagrees with its anchor;
- a hash in `INSTALL.md` isn't the go.dev checksum of a minimum-version
  archive;
- a pinned file no longer matches its rule.

`set` refuses to start on an inconsistent tree. Fix the drift in its own commit
first: real bumps used to fix drift by hand, e.g. `e26a114cd` moved five
`go.mod` files off `go 1.19`. If you really need to bump a tree that drifted,
such as an old release branch, run
`go run ./scripts/bump-go set -allow-inconsistent ...`. Only pins that hold
the anchor version are rewritten, and every other pin is reported as
`WARN left alone`.

## Step 2: Choose the versions

Ask the user for the target versions if they didn't give them. The tool
rejects versions that aren't stable releases on go.dev, and a minimum that is
newer than the toolchain.

Policy: lnd supports the last two Go releases, so the minimum is usually the
latest patch of the previous major release. The tool warns, but doesn't fail,
when the minimum falls further behind. Sometimes the minimum has to be newer
than that, as with `aab1e4661`, where btcwallet required Go 1.27.3. In that case
explain why in the commit message.

## Step 3: Apply

```bash
make bump-go toolchain=1.27.5              # toolchain only
make bump-go min=1.26.12                   # minimum only
make bump-go toolchain=1.28.1 min=1.27.5   # both
```

After rewriting, `set` runs the full `status` check again and confirms each kind
of pin has the requested value. It then searches the whole tree (except `go.sum`,
release notes and the skills) for any old version that is no longer pinned. A
`FAIL leftover` line means the old version shows up somewhere the tool has no
rule for. Decide whether that place is a pin:
- **If it is a pin,** add a rule to `rules` in `scripts/bump-go/main.go`, with a
  test case, and re-run. Don't edit the file by hand, because the next bump
  would miss it again.
- **If it isn't a pin** (e.g. a historical mention), check the line, add its
  file to `scanExclude`, and re-run.

The tool's exit status is the result. Exit 0 means every pin is consistent
and every checksum is verified.

## Step 4: Verify the build

```bash
git diff --stat
go build ./... && make lint
```

When the toolchain moves to a new **major** release, also regenerate the protos
(`make rpc`). Fix anything the new release flags, as in `6b54a387c build: fix
Go 1.27 compatibility` and `dd5231087 lnrpc: regenerate protos with Go 1.27`.
Read the Go release notes for removed ports, too: Go 1.26 dropped
`windows/arm`, so `d3dad1690` had to switch the release targets in
`make/release_flags.mk` and the CI matrix to `windows-arm64`. Put those fixes
in separate commits before the bump.

## Step 5: Commit

Follow recent history and use one commit per bump, with a `build:` prefix:

| Change | Subject | Example |
|---|---|---|
| Both | `build: bump Go language and toolchain versions` | `d67c9edf8` |
| Minimum only | `build: require Go <min>` | `aab1e4661` |
| Toolchain only | `build: update toolchain to Go <toolchain>` | |

In the body, state the new versions and the reason, e.g. keeping the previous
release supported, or a dependency that requires the new minimum (link it).
Open the PR with the `no-changelog` label, as #11383 did for both a toolchain
and a minimum bump.

## Changing the tool

When `set` reports `FAIL leftover` or `FAIL rule ... matched nothing`, update
`rules` in `scripts/bump-go/main.go` and add a case to `main_test.go`. Then run
the unit tests and the history replay:

```bash
go test ./scripts/bump-go
scripts/bump-go/replay_history.sh            # the consistent-era bumps
scripts/bump-go/replay_history.sh <commit>   # or specific bump commits
```

It checks out each commit's parent in a temporary worktree, runs the bump with
that commit's target versions, and requires a byte-identical result for every
file the bump touches. Add the commit of each new bump to its default list.

## Release branches

Go bumps are usually backported to the supported release branches (#11383
carried both `backport-v0.20.x-branch` and `backport-v0.21.x-branch`). Use the
`lnd-backport` skill for that. If a backport conflicts on version pins, run the
tool on the release branch rather than resolving the pins by hand.

Release branches don't contain the tool. Build it from a `master` checkout and
run the binary at the root of the release-branch checkout, since it works on
the tracked files of the current directory:

```bash
go build -o /tmp/bump-go ./scripts/bump-go     # in a master checkout
/tmp/bump-go status                            # in the release branch
```

A rule is skipped when none of its files exist, e.g. older trees without
`docs/ci-runs.md`. It fails when a file exists but the pattern no longer
matches it.
