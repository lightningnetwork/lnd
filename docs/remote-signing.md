# Remote signing

TODO(guggero): Write actual documentation, this is work in progress.


## Test setup

**Node "zane"**:

The node "zane" is the hardened node that contains the private key material and
is not connected to the internet or LN P2P network at all. Ideally only a single
RPC based connection (that can be firewalled off specifically) can be opened
to this node from the host on which the node "yara" is running.

> lnd.conf
```text
# No special configuration required other than basic "hardening" parameters to
# make sure no connections to the internet are opened.

[Application Options]
# Don't listen on the p2p port.
nolisten=true

# Don't reach out to the bootstrap nodes, we don't need a synced graph.
nobootstrap=true

# Just an example, this is the port that needs to be opened in the firewall and
# reachable from the node "yara".
rpclisten=10019
```

Initialize wallet with root key
`tprv8ZgxMBicQKsPe6jS4vDm2n7s42Q6MpvghUQqMmSKG7bTZvGKtjrcU3PGzMNG37yzxywrcdvgkwrr8eYXJmbwdvUNVT4Ucv7ris4jvA7BUmg`
(same as in itest) using the command line.


**Node "yara"**:

The node "yara" is the public, internet facing node that does not contain any
private keys in its wallet but delegates all signing operations to the node
"zane" over a single RPC connection.

> lnd.conf
```text
[remotesigner]
remotesigner.enable=true
remotesigner.rpchost=zane.example.internal:10019
remotesigner.tlscertpath=/home/yara/example/zane.tls.cert
remotesigner.macaroonpath=/home/yara/example/zane.admin.macaroon
```

We use the following script for initializing the watch-only wallet of yara:

```javascript
const fs = require('fs');
const grpc = require('@grpc/grpc-js');
const protoLoader = require('@grpc/proto-loader');
const loaderOptions = {
    keepCase: true,
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true
};
const packageDefinition = protoLoader.loadSync(['../../lnrpc/walletunlocker.proto'], loaderOptions);

process.env.GRPC_SSL_CIPHER_SUITES = 'HIGH+ECDSA'

// build ssl credentials using the cert the same as before
let lndCert = fs.readFileSync('/home/guggero/.lnd-dev-yara/tls.cert');
let sslCreds = grpc.credentials.createSsl(lndCert);

let lnrpcDescriptor = grpc.loadPackageDefinition(packageDefinition);
let lnrpc = lnrpcDescriptor.lnrpc;
var client = new lnrpc.WalletUnlocker('localhost:10018', sslCreds);

client.initWallet({
    wallet_password: Buffer.from('testnet3', 'utf-8'),
    watch_only: {
        accounts: [{
            purpose: 49,
            coin_type: 0,
            account: 0,
            xpub: 'tpubDDXEYWvGCTytEF6hBog9p4qr2QBUvJhh4P2wM4qHHv9N489khkQoGkBXDVoquuiyBf8SKBwrYseYdtq9j2v2nttPpE8qbuW3sE2MCkFPhTq',
        }, {
            purpose: 84,
            coin_type: 0,
            account: 0,
            xpub: 'tpubDDWAWrSLRSFrG1KdqXMQQyTKYGSKLKaY7gxpvK7RdV3e3DkhvuW2GgsFvsPN4RGmuoYtUgZ1LHZE8oftz7T4mzc1BxGt5rt8zJcVQiKTPPV',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 0,
            xpub: 'tpubDDXFHr67Ro2tHKVWG2gNjjijKUH1Lyv5NKFYdJnuaLGVNBVwyV5AbykhR43iy8wYozEMbw2QfmAqZhb8gnuL5mm9sZh8YsR6FjGAbew1xoT',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 1,
            xpub: 'tpubDDXFHr67Ro2tKkccDqNfDqZpd5wCs2n6XRV2Uh185DzCTbkDaEd9v7P837zZTYBNVfaRriuxgGVgxbGjDui4CKxyzBzwz4aAZxjn2PhNcQy',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 2,
            xpub: 'tpubDDXFHr67Ro2tNH4KH41i4oTsWfRjFigoH1Ee7urvHow51opH9xJ7mu1qSPMPVtkVqQZ5tE4NTuFJPrbDqno7TQietyUDmPTwyVviJbGCwXk',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 3,
            xpub: 'tpubDDXFHr67Ro2tQj5Zvav2ALhkU6dRQAhEtNPnYJVBC8hs2U1A9ecqxRY3XTiJKBDD7e8tudhmTRs8aGWJAiAXJN5kXy3Hi6cmiwGWjXK5Cv5',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 4,
            xpub: 'tpubDDXFHr67Ro2tSSR2LLBJtotxx2U45cuESLWKA72YT9td3SzVKHAptzDEx5chsUNZ4WRMY5h6HJxRSebjRatxQKX1uUsux1LvKS1wsfNJ2PH',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 5,
            xpub: 'tpubDDXFHr67Ro2tTwzfWvNoMoPpZbxdMEfe1WhbXJxvXikGixPa4ggSRZeGx6T5yxVHTVT3rjVh35Veqsowj7emX8SZfXKDKDKcLduXCeWPUU3',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 6,
            xpub: 'tpubDDXFHr67Ro2tYEDS2EByRedfsUoEwBtrzVbS1qdPrX6sAkUYGLrZWvMmQv8KZDZ4zd9r8WzM9bJ2nGp7XuNVC4w2EBtWg7i76gbrmuEWjQh',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 7,
            xpub: 'tpubDDXFHr67Ro2tYpwnFJEQaM8eAPM2UV5uY6gFgXeSzS5aC5T9TfzXuawYKBbQMZJn8qHXLafY4tAutoda1aKP5h6Nbgy3swPbnhWbFjS5wnX',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 8,
            xpub: 'tpubDDXFHr67Ro2tddKpAjUegXqt7EGxRXnHkeLbUkfuFMGbLJYgRpG4ew5pMmGg2nmcGmHFQ29w3juNhd8N5ZZ8HwJdymC4f5ukQLJ4yg9rEr3',
        }, {
            purpose: 1017,
            coin_type: 1,
            account: 9,
            xpub: 'tpubDDXFHr67Ro2tgE89V8ZdgMytC2Jq1iT9ttGhdzR1X7haQJNBmXt8kau6taC6DGASYzbrjmo9z9w6JQFcaLNqbhS2h2PVSzKf79j265Zi8hF',
        }]
    }
}, (err, res) => {
    if (err != null) {
        console.log(err);
    }
    console.log(res);
});
```
