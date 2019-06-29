
# Private Altruist Watchtowers

As of v0.7.0, `lnd` supports the ability to run a private, altruist watchtower
as a fully-integrated subsystem of `lnd`. Watchtowers act as a second line of
defense in responding to malicious or accidental breach scenarios in the event
that the client’s node is offline or unable to respond at the time of a breach,
offering greater degree of safety to channel funds.

In contrast to a _reward watchtower_ which demand a portion of the channel funds
as a reward for fulfilling its duty, an _altruist watchtower_ returns all of the
victim’s funds (minus on-chain fees) without taking a cut. Reward watchtowers
will be enabled in a subsequent release, though are still undergoing further
testing and refinement.

In addition, `lnd` can now be configured to operate as a _watchtower client_,
backing up encrypted breach-remedy transactions (aka. justice transactions) to
other altruist watchtowers. The watchtower stores fixed-size, encrypted blobs
and is only able to decrypt and publish the justice transaction after the
offending party has broadcast a revoked commitment state. Client communications
with a watchtower are encrypted and authenticated using ephemeral keypairs,
mitigating the amount of tracking the watchtower can perform on its clients
using long-term identifiers.

Note that we have chosen to deploy a restricted set of features in this release
that can begin to provide meaningful security to `lnd` users. Many more
watchtower-related features are nearly complete or have meaningful progress, and
we will continue to ship them as they receive further testing and become safe to
release.

Note: *For now, watchtowers will only backup the `to_local` and `to_remote` outputs
from revoked commitments; backing up HTLC outputs is slated to be deployed in a
future release, as the protocol can be extended to include the extra signature
data in the encrypted blobs.*

## Configuring a Watchtower

To set up a watchtower, command line users should compile in the optional
`watchtowerrpc` subserver, which will offer the ability to interface with the
tower via gRPC or `lncli`. The release binaries will include the `watchtowerrpc`
subserver by default.

The minimal configuration needed to activate the tower is `watchtower.active=1`.

Retrieving information about your tower’s configurations can be done using
`lncli tower info`:
```
🏔 lncli tower info
{
        "pubkey": "03281d603b2c5e19b8893a484eb938d7377179a9ef1a6bca4c0bcbbfc291657b63",
        "listeners": [
                "[::]:9911"
        ],
        "uris": null,
}
```
The entire set of watchtower configuration options can be found using 
`lnd -h`:
```
watchtower:
      --watchtower.active                                     If the watchtower should be active or not
      --watchtower.towerdir=                                  Directory of the watchtower.db (default: $HOME/.lnd/data/watchtower)
      --watchtower.listen=                                    Add interfaces/ports to listen for peer connections
      --watchtower.externalip=                                Add interfaces/ports where the watchtower can accept peer connections
      --watchtower.readtimeout=                               Duration the watchtower server will wait for messages to be received before hanging up on client connections
      --watchtower.writetimeout=                              Duration the watchtower server will wait for messages to be written before hanging up on client connections
```

### Listening Interfaces

By default, the watchtower will listen on `:9911` which specifies port `9911`
listening on all available interfaces. Users may configure their own listeners
via the `--watchtower.listen=` option. You can verify your configuration by
checking the `"listeners"` field in `lncli tower info`. If you're having trouble
connecting to your watchtower, ensure that `<port>` is open or your proxy is
properly configured to point to an active listener.

### External IP Addresses

Additionally, users can specify their tower’s external IP address(es) using
`watchtower.externalip=`, which will expose the full tower URIs
(pubkey@host:port) over RPC or `lncli tower info`: 
```
        ...
        "uris": [
                "03281d603b2c5e19b8893a484eb938d7377179a9ef1a6bca4c0bcbbfc291657b63@1.2.3.4:9911"
        ]
```

The watchtower's URIs can be given to clients in order to connect and use the
tower by setting the `wtclient.private-tower-uris` option:
```
wtclient.private-tower-uris=03281d603b2c5e19b8893a484eb938d7377179a9ef1a6bca4c0bcbbfc291657b63@1.2.3.4:9911
```

If the watchtower's clients will need remote access, be sure to either:
 - Open port 9911 or a port chosen via `watchtower.listen`.
 - Use a proxy to direct traffic from an open port to the watchtower's listening
   address.

Note: *The watchtower’s public key is distinct from `lnd`’s node public key. For
now this acts as a soft whitelist as it requires clients to know the tower’s
public key in order to use it for backups before more advanced whitelisting
features are implemented. We recommend NOT disclosing this public key openly,
unless you are prepared to open your tower up to the entire Internet.*

### Watchtower Database Directory

The watchtower's database can be moved using the `watchtower.towerdir=`
configuration option. Note that a trailing `/bitcoin/mainnet/watchtower.db`
will be appended to the chosen directory to isolate databases for different
chains, so setting `watchtower.towerdir=/path/to/towerdir` will yield a
watchtower database at `/path/to/towerdir/bitcoin/mainnet/watchtower.db`.

On linux, for example, the default watchtower database will be located at:
```
/$USER/.lnd/data/watchtower/bitcoin/mainnet/watchtower.db
```

## Configuring a Watchtower Client

In order to set up a watchtower client, you’ll need the watchtower URI of an
active watchtower, which will appear like:
```
03281d603b2c5e19b8893a484eb938d7377179a9ef1a6bca4c0bcbbfc291657b63@1.2.3.4:9911
```

The client will automatically be enabled if a URI is configured using
`wtclient.private-tower-uris`:
```
wtclient.private-tower-uris=03281d603b2c5e19b8893a484eb938d7377179a9ef1a6bca4c0bcbbfc291657b63@1.2.3.4:9911
```

At the moment, at most one private watchtower can be configured. If none are
provided, `lnd` will disable the watchtower client.

The entire set of watchtower client configuration options can be found using
`lnd -h`:
```
wtclient:
      --wtclient.private-tower-uris=                          Specifies the URIs of private watchtowers to use in backing up revoked states. URIs must be of the form <pubkey>@<addr>. Only 1 URI is supported at this time, if none are provided the tower will not be enabled.
      --wtclient.sweep-fee-rate=                              Specifies the fee rate in sat/byte to be used when constructing justice transactions sent to the watchtower.
```

### Justice Fee Rates

Users may optionally configure the fee rate of justice transactions by setting
the `wtclient.sweep-fee-rate` option, which accepts values in sat/byte. The
default value is 10 sat/byte, though users may choose to target higher rates to
offer greater priority during fee-spikes. Modifying the `sweep-fee-rate` will be
applied to all new updates after the daemon has been restarted.

### Monitoring

For now, no information regarding the operation of the watchtower client is
exposed over the RPC interface. We are working to expose this information in a
later release, progress on this can be tracked [in this
PR](https://github.com/lightningnetwork/lnd/pull/3184). Users will be reliant on
WTCL logs for observing the behavior of the client. We also plan to expand on
the initial feature set by permitting multiple active towers for redundancy, as
well as modifying the chosen set of towers dynamically without restarting the
daemon.
