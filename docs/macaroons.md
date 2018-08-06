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
it won't be checked for validity.

Since `lnd` requires macaroons by default in order to call RPC methods, `lncli`
now reads a macaroon and provides it in the RPC call. Unless the path is
changed by the `--macaroonpath` option, `lncli` tries to read the macaroon from
`~/.lnd/admin.macaroon` by default and will error if that file doesn't exist
unless provided the `--no-macaroons` option. Keep this in mind when running
`lnd` with `--no-macaroons`, as `lncli` will error out unless called the same
way **or** `lnd` has generated a macaroon on a previous run without this
option.

`lncli` also adds a caveat which makes it valid for only 60 seconds by default
to help prevent replay in case the macaroon is somehow intercepted in
transmission. This is unlikely with TLS, but can happen e.g. when using a PKI
and network setup which allows inspection of encrypted traffic, and an attacker
gets access to the traffic logs after interception. The default 60 second
timeout can be changed with the `--macaroontimeout` option; this can be
increased for making RPC calls between systems whose clocks are more than 60s
apart.

## Using Macaroons with GRPC clients

When interacting with `lnd` using the GRPC interface, the macaroons are encoded
as a hex string over the wire and can be passed to `lnd` by specifying the
hex-encoded macaroon as GRPC metadata:

    GET https://localhost:8080/v1/getinfo
    Grpc-Metadata-macaroon: <macaroon>

Where `<macaroon>` is the hex encoded binary data from the macaroon file itself.

A very simple example using `curl` may look something like this:

    curl --insecure --header "Grpc-Metadata-macaroon: $(xxd -ps -u -c 1000  $HOME/.lnd/admin.macaroon)" https://localhost:8080/v1/getinfo

Have a look at the [Java GRPC example](/docs/grpc/java.md) for programmatic usage details.

## Future improvements to the `lnd` macaroon implementation

The existing macaroon implementation in `lnd` and `lncli` lays the groundwork
for future improvements in functionality and security. We will add features
such as:

* Improved replay protection for securing RPC calls

* Macaroon database encryption

* Root key rotation and possibly macaroon invalidation/rotation

* Tools to allow you to easily delegate macaroons in more flexible ways

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
