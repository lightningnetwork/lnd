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
  * If the option `--noencryptwallet` is used, then the default passphrase
    `hello` is used to encrypt the root key.

## Generated macaroons

With the root key set up, `lnd` continues with creating three macaroon files:

* `invoice.macaroon`: Grants read and write access to all invoice related gRPC
  commands (like generating an address or adding an invoice). Can be used for a
  web shop application for example. Paying an invoice is not possible, even if
  the name might suggest it. The permission `offchain` is needed to pay an
  invoice which is currently only granted in the admin macaroon.
* `readonly.macaroon`: Grants read-only access to all gRPC commands. Could be
  given to  a monitoring application for example.
* `admin.macaroon`: Grants full read and write access to all gRPC commands.
  This is used by the `lncli` client.

These three macaroons all have the location field set to `lnd` and have no
conditions/first party caveats or third party caveats set.

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
	}
```

## Constraints / First party caveats

There are currently two constraints implemented that can be used by `lncli` to
restrict the macaroon it uses to communicate with the gRPC interface. These can
be found in `constraints.go`:

* `TimeoutConstraint`: Set a timeout in seconds after which the macaroon is no
  longer valid.
  This constraint can be set by adding the parameter `--macaroontimeout xy` to
  the `lncli` command.
* `IPLockConstraint`: Locks the macaroon to a specific IP address.
  This constraint can be set by adding the parameter `--macaroonip a.b.c.d` to
  the `lncli` command.
