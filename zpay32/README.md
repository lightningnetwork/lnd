zpay32
=======

[![Build Status](http://img.shields.io/travis/lightningnetwork/lnd.svg)](https://travis-ci.org/lightningnetwork/lnd) 
[![MIT licensed](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/lightningnetwork/lnd/blob/master/LICENSE)
[![GoDoc](https://img.shields.io/badge/godoc-reference-blue.svg)](http://godoc.org/github.com/lightningnetwork/lnd/zpay32)

The zpay32 package implements a basic scheme for the encoding of payment
requests between two `lnd` nodes within the Lightning Network. The zpay32
encoding scheme uses the
[zbase32](https://philzimmermann.com/docs/human-oriented-base-32-encoding.txt)
scheme along with a checksum to encode a serialized payment request.

The payment request serialized by the package consist of: the destination's
public key, the payment hash to use for the payment, and the value of payment
to send.

## Installation and Updating

```shell
$  go get -u github.com/lightningnetwork/lnd/zpay32
```
