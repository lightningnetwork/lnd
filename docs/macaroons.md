As part of [the `lnd` 0.3-alpha
release](https://github.com/lightningnetwork/lnd/releases/tag/v0.3-alpha), we
have addressed [issue 20](https://github.com/lightningnetwork/lnd/issues/20),
which is RPC authentication. Until this was implemented, all RPC calls to `lnd`
were unauthenticated. To fix this, we've utilized
[macaroons](https://research.google.com/pubs/pub41892.html), which are similar
to cookies but more capable. This brief overview explains, at a basic level,
how they work, how we use them for `lnd` authentication, and our future plans.

## What are macaroons?

You can think of a macaroon as a cookie, in a way. Cookies are small bits of
data that your browser stores and sends to a particular website when it makes a
request to that website. If you're logged into a website, that cookie can store
a session ID, which the site can look up in its own database to check who you
are and give you the appropriate content.

A macaroon is similar: it's a small bit of data that a client (like `lncli`)
can send to a service (like `lnd`) to assert that it's allowed to perform an
action. The service looks up the macaroon ID and verifies that the macaroon was
initially signed with the service's root key. However, unlike a cookie, you can
*delegate* a macaroon, or create a version of it that has more limited
capabilities, and then send it to someone else to use.

Just like a cookie, a macaroon should be sent over a secure channel (such as a
TLS-encrypted connection), which is why we've also begun enforcing TLS for RPC
requests in this release. Before SSL was enforced on websites such as Facebook
and Google, listening to HTTP sessions on wireless networks was one way to
hijack the session and log in as that user, gaining access to the user's
account. Macaroons are similar in that intercepting a macaroon in transit
allows the interceptor to use the macaroon to gain all the privileges of the
legitimate user.

## Macaroon delegation

A macaroon is delegated by adding restrictions (called caveats) and an
authentication code similar to a signature (technically an HMAC) to it. The
technical method of doing this is outside the scope of this overview
documentation, but the [README in the macaroons package](../macaroons/README.md)
or the macaroon paper linked above describe it in more detail. The
user must remember several things:

* Sharing a macaroon allows anyone in possession of that macaroon to use it to
  access the service (in our case, `lnd`) to do anything permitted by the
  macaroon. There is a specific type of restriction, called a "third party
  caveat," that requires an external service to verify the request; however,
  `lnd` doesn't currently implement those.

* If you add a caveat to a macaroon and share the resulting macaroon, the
  person receiving it cannot remove the caveat.

This is used in `lnd` in an interesting way. By default, when `lnd` starts, it
creates three files which contain macaroons: a file called `admin.macaroon`,
which contains a macaroon with no caveats, a file called `readonly.macaroon`,
which is the *same* macaroon but with an additional caveat, that permits only
methods that don't change the state of `lnd`, and `invoice.macaroon`, which
only has access to invoice related methods.

## How macaroons are used by `lnd` and `lncli`.

On startup, `lnd` checks to see if the `admin.macaroon`, `readonly.macaroon`
and `invoice.macaroon` files exist. If they don't exist, `lnd` updates its
database with a new macaroon ID, generates the three files `admin.macaroon`,
`readonly.macaroon` and `invoice.macaroon`, all with the same ID. The
`readonly.macaroon` file has an additional caveat which restricts the caller
to using only read-only methods and the `invoice.macaroon` also has an
additional caveat which restricts the caller to using only invoice related
methods. This means a few important things:

* You can delete the `admin.macaroon` and be left with only the
  `readonly.macaroon`, which can sometimes be useful (for example, if you want
  your `lnd` instance to run in autopilot mode and don't want to accidentally
  change its state).

* If you delete the data directory which contains the `macaroons.db` file, this
  invalidates the `admin.macaroon`, `readonly.macaroon` and `invoice.macaroon`
  files. Invalid macaroon files give you errors like `cannot get macaroon: root
  key with id 0 doesn't exist` or `verification failed: signature mismatch
  after caveat verification`.

You can also run `lnd` with the `--no-macaroons` option, which skips the
creation of the macaroon files and all macaroon checks within the RPC server.
This means you can still pass a macaroon to the RPC server with a client, but
it won't be checked for validity. Note that disabling authentication of a server
that's listening on a public interface is not allowed. This means the
`--no-macaroons` option is only permitted when the RPC server is in a private
network. In CIDR notation, the following IPs are considered private,
- [`169.254.0.0/16` and `fe80::/10`](https://en.wikipedia.org/wiki/Link-local_address).
- [`224.0.0.0/4` and `ff00::/8`](https://en.wikipedia.org/wiki/Multicast_address).
- [`10.0.0.0/8`, `172.16.0.0/12` and `192.168.0.0/16`](https://tools.ietf.org/html/rfc1918).
- [`fc00::/7`](https://tools.ietf.org/html/rfc4193).

Since `lnd` requires macaroons by default in order to call RPC methods, `lncli`
now reads a macaroon and provides it in the RPC call. Unless the path is
changed by the `--macaroonpath` option, `lncli` tries to read the macaroon from
the network directory of `lnd`'s currently active network (e.g. for simnet
`lnddir/data/chain/bitcoin/simnet/admin.macaroon`) by default and will error if
that file doesn't exist unless provided the `--no-macaroons` option. Keep this
in mind when running `lnd` with `--no-macaroons`, as `lncli` will error out
unless called the same way **or** `lnd` has generated a macaroon on a previous
run without this option.

`lncli` also adds a caveat which makes it valid for only 60 seconds by default
to help prevent replay in case the macaroon is somehow intercepted in
transmission. This is unlikely with TLS, but can happen e.g. when using a PKI
and network setup which allows inspection of encrypted traffic, and an attacker
gets access to the traffic logs after interception. The default 60 second
timeout can be changed with the `--macaroontimeout` option; this can be
increased for making RPC calls between systems whose clocks are more than 60s
apart.

## Stateless initialization

As mentioned above, by default `lnd` creates several macaroon files in its
directory. These are unencrypted and in case of the `admin.macaroon` provide
full access to the daemon. This can be seen as quite a big security risk if
the `lnd` daemon runs in an environment that is not fully trusted.

The macaroon files are the only files with highly sensitive information that
are not encrypted (unlike the wallet file and the macaroon database file that
contains the [root key](../macaroons/README.md), these are always encrypted,
even if no password is used).

To avoid leaking the macaroon information, `lnd` supports the so called 
`stateless initialization` mode:
* The three startup commands `create`, `unlock` and `changepassword` of `lncli`
  all have a flag called `--stateless_init` that instructs the daemon **not**
  to create `*.macaroon` files.
* The two operations `create` and `changepassword` that actually create/update
  the macaroon database will return the admin macaroon in the RPC call.
  Assuming the daemon and the `lncli` are not used on the same machine, this
  will leave no unencrypted information on the machine where `lnd` runs on.
  * To be more precise: By default, when using the `changepassword` command, the
    macaroon root key in the macaroon DB is just re-encrypted with the new
    password. But the key remains the same and therefore the macaroons issued
    before the `changepassword` command still remain valid. If a user wants to
    invalidate all previously created macaroons, the `--new_mac_root_key` flag
    of the `changepassword` command should be used! 
* A user of `lncli` will see the returned admin macaroon printed to the screen
  or saved to a file if the parameter `--save_to=some_file.macaroon` is used.
* **Important:** By default, `lnd` will create the macaroon files during the
  `unlock` phase, if the `--stateless_init` flag is not used. So to avoid
  leakage of the macaroon information, use the stateless initialization flag
  for all three startup commands of the wallet unlocker service!

Examples:

* Create a new wallet stateless (first run):
  ```shell
    $  lncli create --stateless_init --save_to=/safe/location/admin.macaroon
  ```
* Unlock a wallet that has previously been initialized stateless:
  ```shell
    $  lncli unlock --stateless_init
  ```
* Use the created macaroon:
  ```shell
    $  lncli --macaroonpath=/safe/location/admin.macaroon getinfo
  ```

## Using deterministic/pre-generated macaroons

All macaroons are derived from a secret root key (by default from the root key
with the ID `"0"`). That root key is randomly generated when the macaroon store
is first initialized (when the wallet is created) and is therefore not
deterministic by default.

It can be useful to use a deterministic (or pre-generated) root key, which is
why the `InitWallet` RPC (or the `lncli create` or `lncli createwatchonly`
counterparts) allows a root key to be specified.

Using a pre-generated root key can be useful for scenarios like:
* Testing: If a node is always initialized with the same root key for each test
  run, then macaroons generated in one test run can be re-used in another run
  and don't need to be re-derived.
* Remote signing setup: When using a remote signing setup where there are two
  related `lnd` nodes (e.g. a watch-only and a signer pair), it can be useful
  to generate a valid macaroon _before_ any of the nodes are even started up.

**Example**:

The following example shows how a valid macaroon can be generated before even
starting a node:

```shell
# Randomly generate a 32-byte long secret root key and encode it as hex.
ROOT_KEY=$(cat /dev/urandom | head -c32 | xxd -p -c32)

# Derive a read-only macaroon from that root key.
# NOTE: When using the --root_key flag, the `lncli bakemacaroon` command is
# fully offline and does not need to connect to any lnd node.
lncli bakemacaroon --root_key $ROOT_KEY --save_to /tmp/info.macaroon info:read

# Create the lnd node now, using the same root key.
lncli create --mac_root_key $ROOT_KEY

# Use the pre-generated macaroon for a call.
lncli --macaroonpath /tmp/info.macaroon getinfo
```

## Using Macaroons with GRPC clients

When interacting with `lnd` using the GRPC interface, the macaroons are encoded
as a hex string over the wire and can be passed to `lnd` by specifying the
hex-encoded macaroon as GRPC metadata:

```text
GET https://localhost:8080/v1/getinfo
Grpc-Metadata-macaroon: <macaroon>
```

Where `<macaroon>` is the hex encoded binary data from the macaroon file itself.

A very simple example using `curl` may look something like this:

```shell
$  curl --insecure --header "Grpc-Metadata-macaroon: $(xxd -ps -u -c 1000  $HOME/.lnd/data/chain/bitcoin/simnet/admin.macaroon)" https://localhost:8080/v1/getinfo
```

Have a look at the [Java GRPC example](/docs/grpc/java.md) for programmatic usage details.

## Creating macaroons with custom permissions

The macaroon bakery is described in more detail in the
[README in the macaroons package](../macaroons/README.md).

## Future improvements to the `lnd` macaroon implementation

The existing macaroon implementation in `lnd` and `lncli` lays the groundwork
for future improvements in functionality and security. We will add features
such as:

* Improved replay protection for securing RPC calls

* Macaroon database encryption

* Root key rotation and possibly macaroon invalidation/rotation

* Additional restrictions, such as limiting payments to use (or not use)
  specific routes, channels, nodes, etc.

* Accounting-based macaroons, which can make an instance of `lnd` act almost
  like a bank for apps: for example, an app that pays to consume APIs whose
  budget is limited to the money it receives by providing an API/service

* Support for third-party caveats, which allows external plugins for
  authorization and authentication

With this new feature, we've started laying the groundwork for flexible
authentication and authorization for RPC calls to `lnd`. We look forward to
expanding its functionality to make it easy to develop secure apps.  
