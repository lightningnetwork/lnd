# NAT Traversal

`lnd` has support for NAT traversal using a number of different techniques. At
the time of writing this documentation, UPnP and NAT-PMP are supported. NAT
traversal can be enabled through `lnd`'s `--nat` flag.

```shell
$  lnd ... --nat
```

On startup, `lnd` will try the different techniques until one is found that's
supported by your hardware. The underlying dependencies used for these
techniques rely on using system-specific binaries in order to detect your
gateway device's address. This is needed because we need to be able to reach the
gateway device to determine if it supports the specific NAT traversal technique
currently being tried. Because of this, due to uncommon setups, it is possible
that these binaries are not found in your system. If this is case, `lnd` will
exit stating such error.

As a bonus, `lnd` spawns a background thread that automatically detects IP
address changes and propagates the new address update to the rest of the
network. This is especially beneficial for users who were provided dynamic IP
addresses from their internet service provider.
