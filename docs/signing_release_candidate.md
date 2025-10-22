# Signing a LND Release

When a new version of LND is released, binaries for the lnd and lncli programs
are provided for various platforms and CPU architectures. The hashes of all
these binaries are written into a file known as the "Manifest". This Manifest is
then signed by a quorum of trusted LND contributors (see [verify-install.sh](/scripts/verify-install.sh)
script for more details), ensuring that users can trust the binaries they 
download, knowing they haven't been modified during the automated build process.

To verify a release binary, users have two options:

* Manual Verification: Users can manually download the signature files and 
Manifest from GitHub LND release page, then verify the PGP signatures and 
hashes.

* Automated Verification: The LND repository provides a script, 
[verify-install.sh](/scripts/verify-install.sh), that automates the verification process. This script uses a
set of trusted developer keys (located in the repo under [scripts/keys/](/scripts/keys)) and
downloads the necessary data from the GitHub server to verify the integrity of
the local lnd/lncli binaries.

Running [verify-install.sh](/scripts/verify-install.sh) validates that trusted developers attest to the authenticity between the lnd release binaries hosted on Github and the developer's local builds.

## Adding a new developer as a signer

When another developer is added to the trusted group of people which are
allowed to sign the lnd/lncli releases, their public PGP key needs to be added to
the LND repo. These keys are added in a PR in which 2 reviewers ensure the developer
is in possession of the PGP key which will be added to the LND repo.
(See https://github.com/lightningnetwork/lnd/pull/8788 as an example).
It is important that the name of the PGP key equals the name in the
[verify-install.sh](/scripts/verify-install.sh) script. See also [scripts/keys/README.md](/scripts/keys/README.md) for more information.

## Signing a release binary package

If the new developer's PGP key has been successfully added to the LND repository,
through the aforementioned PR example, they are now able to provide their
signature for the new release's "Manifest" file. To do so, the developer must
follow these steps:

* Follow the [build instructions](/docs/release.md#building-a-new-release).

* After a successful build, all binaries and Manifest files, will be placed
in a directory, named after the tag, created within the directory in which the build occurred. For 
instance, in the case mentioned above, the folder will be named 
`lnd-v0.18.3-beta`.
Ensure that the SHA-256 hashes, in your locally-generated Manifest file, match
those in the Manifest file of the official release on the LND GitHub repository.  
Tip: Download the official release Manifest file to your local maschine and do:  
`diff lnd-v0.18.3-beta/manifest-v0.18.3-beta.txt ~/Downloads/manifest-v0.18.3-beta.txt`
(example command for a release candidate called `v0.18.3-beta`)  
Only if all hashes are identical, should you sign the release.  If the digests
match, see the example signing command, assuming your PGP signing key is
available on your local device:  
`gpg --local-user $KEYID  --detach-sig --output manifest-$USERNAME-v0.18.3-beta.sig manifest-v0.18.3-beta.txt`.  
`USERNAME` being the name in the `[verify-install.sh](../scripts/verify-install.sh)`
script and also the name of your PGP key file. The whole argument `--local-user $KEYID`
is only needed if there's more than one signing key on your local machine. Be
sure to substitute the TAG value `v0.18.3-beta` with the version you are 
currently signing.

* Finally, upload the signature file
(e.g. manifest-USERNAME-v0.18.3-beta.sig) to the GitHub release page.
Github write permissions are required to upload signatures to the LND release
page. To avoid interfering with other signers who may be updating the GitHub
release page, LND developers use a `KeyBase` communication channel to signal
when an edit is in progress. Once your signature file is successfully uploaded
and the release page is unlocked, the signing process is complete.

Congratulations signing the LND release 🎉.