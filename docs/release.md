# `lnd`'s Reproducible Build System

This package contains the build script that the `lnd` project uses in order to
build binaries for each new release. As of `go1.13`, with some new build flags,
binaries are now reproducible, allowing developers to build the binary on
distinct machines, and end up with a byte-for-byte identical binary. However,
this wasn't _fully_ solved in `go1.13`, as the build system still includes the
directory the binary is built into the binary itself. As a result, our scripts
utilize a workaround needed until `go1.13.2`.  

## Preparing a Release

Release managers prepare artifacts before publishing the signed Git tag. When
a version-bump pull request merges into a `v0.*-branch` release branch, a push
that changes `build/version.go` automatically starts the release workflow.
Consolidate the release-note fragments before merging the bump. The workflow
checks those notes, builds all release architectures, and uploads the assets
to a **draft** release. It creates an unsigned tag only inside its runner so
the normal tag and binary-version checks run; it never pushes that tag.

The release workflow must be present on the release branch. Development
versions with patch number `.99` and versions whose tags already exist are
skipped. Later pushes of signed tags do not rerun the build or replace assets.

For each release candidate, final release, or patch release:

1. Merge the version bump and wait for the automatic release workflow to
   succeed. The draft records its exact build commit and links to that run.
   Confirm that the linked run succeeded and all expected assets are present;
   a draft left by a failed upload is not ready for signing or publication.
2. Update your release-branch checkout to that exact commit. Compare
   `git rev-parse HEAD` with the draft's build commit. If the upstream branch
   has advanced, follow the retry procedure below to prepare its updated head
   instead of signing another commit against the old assets. The tagging
   helper still requires HEAD to match the current upstream branch.
3. Create the signed tag with the existing helper, then push it to the
   upstream remote printed by the helper. For example:

   ```shell
   # Sign the tested branch head after the helper verifies upstream/version.
   ./scripts/tag-release.sh v0.21.0-beta.rc1 --branch v0.21.x-branch
   # Publish the signed tag to that same upstream; the release stays a draft.
   git push <upstream-remote> v0.21.0-beta.rc1
   ```

4. Review and finalize the draft's release notes, manifest signatures, and
   required timestamp artifacts, then publish the draft manually. Push the
   signed tag **before** publishing the draft so GitHub uses that signed tag
   instead of creating an unsigned one. Existing release verification and
   Docker image requirements still apply.

### Retrying Preparation

Notes or build failures leave the real tag unpublished. Correct the problem
on the release branch and use the workflow's **Run workflow** action to prepare
the current branch head again. Manual dispatch also requires the workflow to
exist on the repository's default branch. It reads the version from the
selected commit, so no tag input or local preflight command is needed.

Before any same-version retry, including preparation after the branch
advances, stop any active preparation runs for that version and delete its
stale draft releases and assets through the Releases UI. Do not delete or
replace a real tag or a published release. The upload action cannot reliably
reuse drafts across paginated release lists and replaces matching assets
individually, so a failed retry can leave partial or mixed assets. Start with
a fresh draft and wait for its linked run to succeed before trusting its
build commit and assets, signing the tag, or publishing the release.

## Building a New Release

### macOS

The first requirement is to have [`docker`](https://www.docker.com/)
installed locally and running. The second requirement is to have `make`
installed. Everything else (including `golang`) is included in the release
helper image.

To build a release, run the following commands:

```shell
$  git clone https://github.com/lightningnetwork/lnd.git
$  cd lnd
$  git checkout <TAG> # <TAG> is the name of the next release/tag
$  make docker-release tag=<TAG>
```

Where `<TAG>` is the name of the next release of `lnd`.

### Linux/Windows (WSL)

No prior set up is needed on Linux or macOS is required in order to build the
release binaries. However, on Windows, the only way to build the release
binaries at the moment is by using the Windows Subsystem Linux. One can build
the release binaries following these steps:

```shell
$  git clone https://github.com/lightningnetwork/lnd.git
$  cd lnd
$  git checkout <TAG> # <TAG> is the name of the next release/tag
$  make release tag=<TAG>
```

This will then create a directory of the form `lnd-<TAG>` containing archives
of the release binaries for each supported operating system and architecture,
and a manifest file containing the hash of each archive.

## Verifying a Release

With `go1.13`, it's now possible for third parties to verify release binaries.
Before this version of `go`, one had to trust the release manager(s) to build the
proper binary. With this new system, third parties can now _independently_ run
the release process, and verify that all the hashes of the release binaries
match exactly that of the release binaries produced by said third parties.

To verify a release, one must obtain the following tools (many of these come
installed by default in most Unix systems): `gpg`/`gpg2`, `shashum`, and
`tar`/`unzip`.

Once done, verifiers can proceed with the following steps:

1. Acquire the archive containing the release binaries for one's specific
   operating system and architecture, and the manifest file along with its
   signature.
2. Verify the signature of the manifest file with `gpg --verify
   manifest-<TAG>.txt.sig`. This will require obtaining the PGP keys which
   signed the manifest file, which are included in the release notes.
3. Recompute the `SHA256` hash of the archive with `shasum -a 256 <filename>`,
   locate the corresponding one in the manifest file, and ensure they match
   __exactly__.

At this point, verifiers can use the release binaries acquired if they trust
the integrity of the release manager(s). Otherwise, one can proceed with the
guide to verify the release binaries were built properly by obtaining `shasum`
and `go` (matching the same version used in the release):

4. Extract the release binaries contained within the archive, compute their
   hashes as done above, and note them down.
5. Ensure `go` is installed, matching the same version as noted in the release
   notes. 
6. Obtain a copy of `lnd`'s source code with `git clone
   https://github.com/lightningnetwork/lnd` and checkout the source code of the
   release with `git checkout <TAG>`.
7. Proceed to verify the tag with `git verify-tag <TAG>` and compile the
   binaries from source for the intended operating system and architecture with
   `make release sys=OS-ARCH tag=<TAG>`.
8. Extract the archive found in the `lnd-<TAG>` directory created by the
   release script and recompute the `SHA256` hash of the release binaries (lnd
   and lncli) with `shasum -a 256 <filename>`. These should match __exactly__
   as the ones noted above.

## Verifying Docker Images

To verify the `lnd` and `lncli` binaries inside the
[official provided docker images](https://hub.docker.com/r/lightninglabs/lnd)
against the signed, reproducible release binaries, there is a verification
script in the image that can be called (before starting the container for
example):

```shell
$  docker run --rm --entrypoint="" lightninglabs/lnd:v0.12.1-beta /verify-install.sh v0.12.1-beta
$  OK=$?
$  if [ "$OK" -ne "0" ]; then echo "Verification failed!"; exit 1; done
$  docker run lightninglabs/lnd [command-line options]
```

# Signing an Existing Manifest File

If you're a developer of `lnd` and are interested in attaching your signature
to the final release archive, the manifest MUST be signed in a manner that
allows your signature to be verified by our verify script
`scripts/verify-install.sh`. 

Assuming you've done a local build for _all_ release targets, then you should
have a file called `manifest-TAG.txt` where `TAG` is the actual release tag
description being signed. The release script expects a particular file name for
each included signature, so we'll need to modify the name of our output
signature during signing.

Assuming `USERNAME` is your current nick as a developer, then the following
command will generate a proper signature:
```shell
$  gpg --detach-sig --output manifest-USERNAME-TAG.sig manifest-TAG.txt
```
