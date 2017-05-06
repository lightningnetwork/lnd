# Installation for lnd and btcd

### If Glide isn't installed, install it:

```
$ go get -u github.com/Masterminds/glide
```

### Install lnd:

```
$ cd $GOPATH
$ git clone https://github.com/lightningnetwork/lnd $GOPATH/src/github.com/lightningnetwork/lnd
$ cd $GOPATH/src/github.com/lightningnetwork/lnd
$ glide install
$ go install . ./cmd/...
```

### Install btcutil: (must be from roasbeef fork, not from btcsuite)

```
$ go get -u github.com/roasbeef/btcutil
```

### Install btcd: (must be from roasbeef fork, not from btcsuite)

```
$ cd $GOPATH/src/github.com/roasbeef/btcd
$ glide install
$ go install . ./cmd/...
```

### Start btcd (will create rpc.cert and default btcd.conf):

```
$ btcd --testnet --txindex --rpcuser=kek --rpcpass=kek
```

Before you'll be able to use `lnd` on testnet, `btcd` needs to first fully sync
the blockchain. Depending on your hardware, this may take up to a few hours.

(NOTE: It may take several minutes to find segwit-enabled peers.)

While `btcd` is syncing you can check on it's progress using btcd's `getinfo`
RPC command:
```
$ btcctl --testnet getinfo
{
  "version": 120000,
  "protocolversion": 70002,
  "blocks": 1114996, 
  "timeoffset": 0,
  "connections": 7,
  "proxy": "",
  "difficulty": 422570.58270815,
  "testnet": true,
  "relayfee": 0.00001,
  "errors": ""
}
```

Additionally, you can monitor btcd's logs to track its syncing progress in real
time. 

You can test your `btcd` node's connectivity using the `getpeerinfo` command:
```
$ btcctl --testnet getpeerinfo | more
```

### Start lnd: (once btcd has synced testnet)

```
$ lnd --bitcoin.active --bitcoin.testnet --debuglevel=debug --externalip=X.X.X.X
```

If you'd like to signal to other nodes on the network that you'll accept
incoming channels (as peers need to connect inbound to initiate a channel
funding workflow), then the `--externalip` flag should be set to your publicly
reachable IP address.

#### Simnet Development

If doing local development, you'll want to start both `btcd` and `lnd` in the
`simnet` mode. Simnet is similar to regtest in that you'll be able to instantly
mine blocks as needed to test `lnd` locally. In order to start either daemon in
the `simnet` mode add the `--bitcoin.simnet` flag instead of the
`--bitcoin.testnet` flag.

Another relevant command line flag for local testing of new `lnd` developments
is the `--debughtlc` flag. When starting `lnd` with this flag, it'll be able to
automatically settle a special type of HTLC sent to it. This means that you
won't need to manually insert invoices in order to test payment connectivity.
To send this "special" HTLC type, include the `--debugsend` command at the end
of your `sendpayment` commands.
```
$ lnd --bitcoin.active --bitcoin.simnet --debughtlc
```

### Create an lnd.conf (Optional)

Optionally, if you'd like to have a persistent configuration between `lnd`
launches, allowing you to simply type `lnd --bitcoin.testnet --bitcoin.active`
at the command line. You can create an `lnd.conf`. 

**On MacOS, located at:**
`/Users/[username]/Library/Application Support/Lnd/lnd.conf`

**On Linux, located at:**
`~/.lnd/lnd.conf`

Here's a sample `lnd.conf` to get you started:
```
[Application Options]
debuglevel=trace
debughtlc=true
maxpendingchannels=10
profile=5060
externalip=128.111.13.23
externalip=111.32.29.29

[Bitcoin]
bitcoin.active=1
bitcoin.rpchost=localhost:18334
```

Notice the `[Bitcoin]` section. This section houses the parameters for the
Bitcoin chain. Also `lnd` also supports Litecoin, one is able to also specified
(but not concurrently with Bitcoin!) the proper parameters, so `lnd` knows to
be active on Litecoin's testnet4.

#### Accurate as of:
roasbeef/btcd commit: 54362e17a5b80643ef1e12edc08895a2e2a1202b

roasbeef/btcutil commit: d347e49

lightningnetwork/lnd commit: d7b36c6
