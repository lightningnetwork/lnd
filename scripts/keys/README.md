# Release verification keys

This directory contains all keys that are currently signing `lnd` releases.

The name of the file must match exactly the suffix that user is going to use
when signing a release.
For example, if the key is called `eugene_.asc` then that user must upload a
signature file called `manifest-eugene_-v0.xx.yy-beta.sig`, otherwise the
verification will fail. See [the release
documentation](../../docs/release.md#signing-an-existing-manifest-file) for
details on how to create the signature.

In addition to adding the key file here as a `.asc` file the
`scripts/verify-install.sh` file must also be updated with the key ID and the
reference to the key file.

## Removing keys

A key is removed from this directory and from `scripts/verify-install.sh` when
it expires or when its owner stops signing releases. Removing a key can drop
the number of recognized signatures on an older release below
`MIN_REQUIRED_SIGNATURES`. The current script is only expected to verify
releases within the supported release lines (see
[SECURITY.md](../../SECURITY.md)), where users are expected to run the latest
patch release. To verify an older release, use the script that shipped with
that release, for example from its Docker image:

```shell
$  docker run --rm --entrypoint="" lightninglabs/lnd:v0.20.1-beta /verify-install.sh v0.20.1-beta
```
