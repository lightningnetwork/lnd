# macaroons

This is a more detailed, technical description of how macaroons work and how
authentication and authorization is implemented in `lnd`.

For a more high-level overview see
[macaroons.md in the docs](../docs/macaroons.md).

## Root key

At startup, if the option `--no-macaroons` is **not** used, a Bolt DB key/value
store named `data/macaroons.db` is created with a bucket named `macrootkeys`.
In this DB the following two key/value pairs are stored:

* Key `0`: the encrypted root key (32 bytes).
  * If the root key does not exist yet, 32 bytes of pseudo-random data is
    generated and used.
* Key `enckey`: the parameters used to derive a secret encryption key from a
  passphrase.
  * The following parameters are stored: `<salt><digest><N><R><P>`
    * `salt`: 32 byte of random data used as salt for the `scrypt` key
      derivation.
    * `digest`: sha256 hashed key derived from the `scrypt` operation. Is used
      to verify if the password is correct.
    * `N`, `P`, `R`: Parameters used for the `scrypt` operation.
  * The root key is symmetrically encrypted with the derived secret key, using
    the `secretbox` method of the library
    [btcsuite/golangcrypto](https://github.com/btcsuite/golangcrypto).
  * If the option `--noseedbackup` is used, then the default passphrase
    `hello` is used to encrypt the root key.

## Generated macaroons

With the root key set up, `lnd` continues with creating three macaroon files:

* `invoice.macaroon`: Grants read and write access to all invoice related gRPC
  commands (like generating an address or adding an invoice). Can be used for a
  web shop application for example. Paying an invoice is not possible, even if
  the name might suggest it. The permission `offchain` is needed to pay an
  invoice which is currently only granted in the admin macaroon.
* `readonly.macaroon`: Grants read-only access to all gRPC commands. Could be
  given to a monitoring application for example.
* `admin.macaroon`: Grants full read and write access to all gRPC commands.
  This is used by the `lncli` client.

In addition to those three, every sub server that is compiled into the binary
bakes a macaroon of its own on first startup, scoped to the permissions that
sub server needs:

* `router.macaroon` (`routerrpc`, always compiled in): `offchain:read`,
  `offchain:write`.
* `signer.macaroon` (`signrpc` build tag): `signer:generate`, `signer:read`.
* `walletkit.macaroon` (`walletrpc` build tag): `address:read`,
  `address:write`, `onchain:read`, `onchain:write`.
* `chainnotifier.macaroon` (`chainrpc` build tag): `onchain:read`.
* `invoices.macaroon` (`invoicesrpc` build tag): `invoices:read`,
  `invoices:write`.

The release builds enable all of those build tags, so an official `lnd` binary
creates all of these files. The location of each one can be overridden with
its own command line option (`--routermacaroonpath`, `--signermacaroonpath`
and so on).

None of these files are written to disk if `--stateless_init` is used. The
macaroons are then only returned in the response of the `InitWallet` or
`UnlockWallet` call.

All macaroons that `lnd` bakes on startup have the location field set to `lnd`
and have no conditions/first party caveats or third party caveats set.

The access restrictions are implemented with a list of entity/action pairs that
is mapped to the gRPC functions by the `rpcserver.go`.
For example, the permissions for the `invoice.macaroon` looks like this:

```go
	// invoicePermissions is a slice of all the entities that allows a user
	// to only access calls that are related to invoices, so: streaming
	// RPCs, generating, and listening invoices.
	invoicePermissions = []bakery.Op{
		{
			Entity: "invoices",
			Action: "read",
		},
		{
			Entity: "invoices",
			Action: "write",
		},
		{
			Entity: "address",
			Action: "read",
		},
		{
			Entity: "address",
			Action: "write",
		},
		{
			Entity: "onchain",
			Action: "read",
		},
	}
```

## Constraints / First party caveats

A constraint is a first party caveat that further restricts an existing
macaroon. Appending a caveat only requires the macaroon itself and not the root
key, so constraints can be added offline by anyone holding the macaroon, and
they can only ever narrow what a macaroon allows, never widen it.

The following constraints are implemented in `constraints.go`:

* `TimeoutConstraint`: Sets a timeout in seconds after which the macaroon is no
  longer valid. Encoded as the standard `time-before` caveat.
* `IPLockConstraint`: Locks the macaroon to a single IP address, encoded as an
  `ipaddr` caveat.
* `IPRangeLockConstraint`: Locks the macaroon to an IP range in CIDR notation,
  encoded as an `iprange` caveat.
* `CustomConstraint`: Adds an `lnd-custom <name> <condition>` caveat. `lnd`
  does not interpret the condition itself. A component such as a registered
  RPC middleware has to declare that it handles that caveat name, otherwise
  the macaroon is rejected as a whole.
The `time-before` caveat is validated by the macaroon bakery's own standard
checker. The checker functions for the lnd specific conditions (`ipaddr`,
`iprange` and `lnd-custom`) are registered on the macaroon service
in `config_builder.go`.

Constraints can be added while baking a macaroon, or appended to an existing
macaroon file:

```shell
$  lncli bakemacaroon --timeout 3600 --ip_address 10.0.0.5 info:read
$  lncli constrainmacaroon --timeout 3600 admin.macaroon limited.macaroon
```

Two of them can also be applied by `lncli` on the fly, without changing the
macaroon file at all:

* `--macaroontimeout xy` sets the timeout. Note that this one is applied to
  **every** call with a default of 60 seconds, as a basic anti-replay measure:
  every macaroon that `lncli` sends already carries a short lived `time-before`
  caveat.
* `--macaroonip a.b.c.d` locks the macaroon to an IP address.

`IPRangeLockConstraint` is currently only reachable from Go code, neither
`bakemacaroon` nor `constrainmacaroon` registers a command line flag for it.

## Bakery

As of lnd `v0.9.0-beta` there is a macaroon bakery available through gRPC and
command line.
Users can create their own macaroons with custom permissions if the provided
default macaroons (`admin`, `invoice` and `readonly`) are not sufficient.

For example, a macaroon that is only allowed to manage peers with a default root
key `0` would be created with the following command:

```shell
$  lncli bakemacaroon peers:read peers:write
```

For even more fine-grained permission control, it is also possible to specify
single RPC method URIs that are allowed to be accessed by a macaroon. This can
be achieved by passing `uri:<methodURI>` pairs to `bakemacaroon`, for example:

```shell
$  lncli bakemacaroon uri:/lnrpc.Lightning/GetInfo uri:/verrpc.Versioner/GetVersion
```

The macaroon created by this call would only be allowed to call the `GetInfo` and
`GetVersion` methods instead of all methods that have similar permissions (like
`info:read` for example).

If you need a macaroon file with rights similar to `admin.macaroon` for a
custom use case, you can create one as shown in the following example.
Note that a macaroon created in this way will have extensive rights, allowing
it to create macaroons with more permissions than the original one.
```shell
$  lncli bakemacaroon --save_to lnbits.macaroon \
   address:read address:write \
   info:read info:write \
   invoices:read invoices:write \
   macaroon:generate macaroon:read macaroon:write \
   message:read message:write \
   offchain:read offchain:write \
   onchain:read onchain:write \
   peers:read peers:write \
   signer:generate signer:read
```

A full list of available entity/action pairs and RPC method URIs can be queried
by using the `lncli listpermissions` command.

## Root key rotation

To manage the root keys used by macaroons, there are `listmacaroonids` and
`deletemacaroonid` available through gRPC and command line.
Users can view a list of all macaroon root key IDs that are in use using:

```shell
$  lncli listmacaroonids
```

And remove a specific macaroon root key ID using command:

```shell
$  lncli deletemacaroonid root_key_id
```

Be careful with the `deletemacaroonid` command as when a root key is deleted,
**all the macaroons created from it are invalidated**.
