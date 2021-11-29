chainhash
=========

[![Build Status](http://img.shields.io/travis/btcsuite/ltcd.svg)](https://travis-ci.org/ltcsuite/ltcd)
[![ISC License](http://img.shields.io/badge/license-ISC-blue.svg)](http://copyfree.org)
[![GoDoc](https://img.shields.io/badge/godoc-reference-blue.svg)](http://godoc.org/github.com/ltcsuite/ltcd/chaincfg/chainhash)
=======

chainhash provides a generic hash type and associated functions that allows the
specific hash algorithm to be abstracted.

## Installation and Updating

```bash
$ go get -u github.com/ltcsuite/ltcd/chaincfg/chainhash
```

## GPG Verification Key

All official release tags are signed by Conformal so users can ensure the code
has not been tampered with and is coming from the btcsuite developers.  To
verify the signature perform the following:

- Download the public key from the Conformal website at
  https://opensource.conformal.com/GIT-GPG-KEY-conformal.txt

- Import the public key into your GPG keyring:
  ```bash
  gpg --import GIT-GPG-KEY-conformal.txt
  ```

- Verify the release tag with the following command where `TAG_NAME` is a
  placeholder for the specific tag:
  ```bash
  git tag -v TAG_NAME
  ```

## License

Package chainhash is licensed under the [copyfree](http://copyfree.org) ISC
License.
