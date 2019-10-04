# lnd's Reproducible Build System

This package contains the buidl script that the `lnd` project uses in order to
build binaries for each new release. As of Go 1.13, with some new build flags,
binaries are now reproducible, allowing developers to build the binary on
distinct machines, and end up with a byte-for-byte identical binary.  However,
this wasn't _fully_ solved in Go 1.13, as the build system still includes the
directory the binary is built into the binary itself. As a result, our scripts
utilize a work around needed until Go 1.13.2.  

## Building a New Release

### MacOS/Linux/Windows (WSL)

No prior set up is needed on Linux or MacOS is required in order to build the
release binaries.  However, on Windows, the only way to build the binaries atm
is using the Windows Subsystem Linux. One can build the release binaries from
one's `lnd` directory, no matter where it's located.  One can download `lnd` to

```
git clone https://github.com/lightningnetwork/lnd.git
```

Afterwards, the release manager/verifier simply needs to run:
`./build/release/release.sh <TAG>` (from the top-level `lnd` directory) , where
`<TAG>` is the name of the next release/tag.

## Verifying a Release

With Go 1.13, it's now possible for third parties to verify a release binary.
Before this release, one had to trust that release manager to build the proper
binary. With this new system, third parties can now _independently_ run the
release process, and verify that all the hashes in the final `manifest.txt`
match up.
