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

## Bakery

As of lnd `v0.9.0-beta` there is a macaroon bakery available through gRPC and
command line.
Users can create their own macaroons with custom permissions if the provided
default macaroons (`admin`, `invoice` and `readonly`) are not sufficient.

For example, a macaroon that is only allowed to manage peers would be created
with the following command:

`lncli bakemacaroon peers:read peers:write`

A full and up-to-date list of available entity/action pairs can be found by
looking at the `rpcserver.go` in the root folder of the project.

### Upgrading from v0.8.0-beta or earlier

Users upgrading from a version prior to `v0.9.0-beta` might get a `permission
denied ` error when trying to use the `lncli bakemacaroon` command.
This is because the bakery requires a new permission (`macaroon/generate`) to
access.
Users can obtain a new `admin.macaroon` that contains this permission by
removing all three default macaroons (`admin.macaroon`, `invoice.macaroon` and
`readonly.macaroon`, **NOT** the `macaroons.db`!) from their
`data/chain/<chain>/<network>/` directory inside the lnd data directory and
restarting lnd.

## Accounts

As introduced in this first
[PR](https://github.com/lightningnetwork/lnd/pull/2390),
an account is a simple construct that has a balance and an optional expiration
date. The balance represents satoshis that can be spent by the "owner" of the
account.  
Every invoice that is paid from/with an account reduces the balance of that
account by the payment amount plus fees.
Once the balance of an account is 0, further payments will be refused from/with
that account.

Applying an account to a macaroon is a *restriction*. The bearer of a macaroon
that is locked to an account will only be able to spend **at most** as many
satoshis as the account's balance.  
If a macaroon is not locked to an account, there is no restriction.

Accounts only assert a maximum amount spendable. Having a certain account
balance does not guarantee that the node has the channel liquidity to actually
spend that amount.

There are three commands in `lncli` that can be used to manage accounts:
* `createaccount balance [expiration_date]` Creates a new account with the given
  balance and the optional expiration date.
* `listaccounts` Lists all accounts that are currently stored in the account
  database.
* `removeaccount id` Removes the account with the given ID.

Example output of `listaccounts`:
```json
{
    "accounts": [
        {
            "id": "945ab38d3890c267",
            "initial_balance": "100",
            "current_balance": "100",
            "last_update": "1546254113",
            "expiration_date": "0"
        }
    ]
}
```

A new macaroon that is locked to an account can then be created using the
`delegatemacaroon` command:  
`lncli --macaroonpath /some/dir/admin.macaroon delegatemacaroon --save_to
/some/dir/account.macaroon --account_id abcdef01abcdef01`

With this first version there are several limitations that will be addressed with further PRs:
* Accounts cannot be replenished.
* Only payment methods (`SendPayment`, `SendPaymentSync`, `SendToRoute` and `SendToRouteSync`) are currently checked for the account constraint. It can be discussed if the `ListAccounts` method should only show the current account when the macaroon is locked to an account.
* There is no link between accounts and the payments that have been paid from/with them.
* There are no accounts with on-chain balance.
* There are no accounts with a periodic balance (e.g. for subscription payments).
