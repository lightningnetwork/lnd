# `lnd`'s Reproducible Build System

This package contains the build script that the `lnd` project uses in order to
build binaries for each new release. As of `go1.13`, with some new build flags,
binaries are now reproducible, allowing developers to build the binary on
distinct machines, and end up with a byte-for-byte identical binary. However,
this wasn't _fully_ solved in `go1.13`, as the build system still includes the
directory the binary is built into the binary itself. As a result, our scripts
utilize a work around needed until `go1.13.2`.  

## Building a New Release

### macOS/Linux/Windows (WSL)

No prior set up is needed on Linux or macOS is required in order to build the
release binaries. However, on Windows, the only way to build the release
binaries at the moment is by using the Windows Subsystem Linux. One can build
the release binaries following these steps:

1. `git clone https://github.com/lightningnetwork/lnd.git`
2. `cd lnd`
3. `make release tag=<TAG> # <TAG> is the name of the next release/tag`

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
