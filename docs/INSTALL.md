# Installation

### Preliminaries
  In order to work with [`lnd`](https://github.com/lightningnetwork/lnd), the
  following build dependencies are required:
  
  * **Go:** `lnd` is written in Go. To install, run one of the following commands:

  
    **Note**: The minimum version of Go supported is Go 1.8.

    
    On Linux:
    ```
    sudo apt-get install golang-1.8-go
    ```

    On Mac OS X
    ```
    brew install go
    ```

    Alternatively, one can download the pre-compiled binaries hosted on the
    [golang download page](https://golang.org/dl/). If one seeks to install
    from source, then more detailed installation instructions can be found
    [here](http://golang.org/doc/install). 

    At this point, you should set your `$GOPATH` environment variable, which
    represents the path to your workspace. By default, `$GOPATH` is set to
    `~/go`. You wll also need to add `$GOPATH/bin` to your `PATH`. This ensures
    that your shell will be able to detect the binaries you install.

    ```bash
    export GOPATH=~/gocode
    export PATH=$PATH:$GOPATH/bin
    ```

    We recommend placing the above in your .bashrc or in a setup script so that
    you can avoid typing this every time you open a new terminal window.

  * **Glide:** This project uses `Glide` to manage dependencies as well 
    as to provide *reproducible builds*. To install `Glide`, execute the
    following command (assumes you already have Go properly installed):
    ```
    go get -u github.com/Masterminds/glide
    ```

### Installing lnd

With the preliminary steps completed, to install `lnd`, `lncli`, and all
related dependencies run the following commands:
```
git clone https://github.com/lightningnetwork/lnd $GOPATH/src/github.com/lightningnetwork/lnd
cd $GOPATH/src/github.com/lightningnetwork/lnd
glide install
go install . ./cmd/...
```

**Updating**

To update your version of `lnd` to the latest version run the following 
commands:
```
cd $GOPATH/src/github.com/lightningnetwork/lnd
git pull && glide install
go install . ./cmd/...
```

**Tests**

To check that `lnd` was installed properly run the following command:
```
go install; go test -v -p 1 $(go list ./... | grep -v  '/vendor/')
```

### Installing btcd

`lnd` currently requires `btcd` with segwit support, which is not yet merged
into the master branch. Instead, [roasbeef](https://github.com/roasbeef/btcd)
maintains a fork with his segwit implementation applied. To install, run the
following commands:

Install **btcd**: (must be from roasbeef fork, not from btcsuite)
```
git clone https://github.com/roasbeef/btcd $GOPATH/src/github.com/roasbeef/btcd
cd $GOPATH/src/github.com/roasbeef/btcd
glide install
go install . ./cmd/...
```

### Starting btcd

Running the following command will create `rpc.cert` and default `btcd.conf`.

```
btcd --testnet --txindex --rpcuser=kek --rpcpass=kek
```
If you want to use `lnd` on testnet, `btcd` needs to first fully sync the
testnet blockchain. Depending on your hardware, this may take up to a few
hours.

(NOTE: It may take several minutes to find segwit-enabled peers.)

While `btcd` is syncing you can check on its progress using btcd's `getinfo`
RPC command:
```
btcctl --testnet --rpcuser=kek --rpcpass=kek getinfo
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
btcctl --testnet --rpcuser=kek --rpcpass=kek getpeerinfo | more
```

### lnd

#### Simnet vs. Testnet Development

If you are doing local development, such as for the tutorial, you'll want to
start both `btcd` and `lnd` in the `simnet` mode. Simnet is similar to regtest
in that you'll be able to instantly mine blocks as needed to test `lnd`
locally. In order to start either daemon in the `simnet` mode use `simnet`
instead of `testnet`, adding the `--bitcoin.simnet` flag instead of the
`--bitcoin.testnet` flag.

Another relevant command line flag for local testing of new `lnd` developments
is the `--debughtlc` flag. When starting `lnd` with this flag, it'll be able to
automatically settle a special type of HTLC sent to it. This means that you
won't need to manually insert invoices in order to test payment connectivity.
To send this "special" HTLC type, include the `--debugsend` command at the end
of your `sendpayment` commands.


There are currently two primary ways to run `lnd`, one requires a local `btcd`
instance with the RPC service exposed, and the other uses a fully integrate
light client powered by [neutrino](https://github.com/lightninglabs/neutrino).

#### Running lnd in light client mode

In order to run `lnd` in its light client mode, you'll need to locate a
full-node which is capable of serving this new light client mode. A [BIP
draft](https://github.com/Roasbeef/bips/blob/master/gcs_light_client.mediawiki)
exists, and will be finalized in the near future, but for now you'll need to be
running `roasbeef`'s fork of btcd. A public instance of such a node can be
found at `faucet.lightning.community`.

To run lnd in neutrino mode, run `lnd` with the following arguments, (swapping
in `--bitcoin.simnet` for `simnet` mode if needed), and also your own `btcd`
node if available:
```
lnd --bitcoin.active --bitcoin.testnet --debuglevel=debug --neutrino.active --neutrino.connect=faucet.lightning.community
```

#### Running lnd using the btcd backend

If you are on testnet, run this command after `btcd` has finished syncing.
Otherwise, replace `--bitcoin.testnet` with `--bitcoin.simnet`. If you
installing `lnd` in preparation for the
[tutorial](http://dev.lightning.community/tutorial), you may skip this step.
```
lnd --bitcoin.active --bitcoin.testnet --debuglevel=debug --bitcoin.rpcuser=kek --bitcoin.rpcpass=kek --externalip=X.X.X.X
```

#### Network Reachability 

If you'd like to signal to other nodes on the network that you'll accept
incoming channels (as peers need to connect inbound to initiate a channel
funding workflow), then the `--externalip` flag should be set to your publicly
reachable IP address.

# Creating an lnd.conf (Optional)

Optionally, if you'd like to have a persistent configuration between `lnd`
launches, allowing you to simply type `lnd --bitcoin.testnet --bitcoin.active`
at the command line, you can create an `lnd.conf`. 

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

[Bitcoin]
bitcoin.active=1
```

Notice the `[Bitcoin]` section. This section houses the parameters for the
Bitcoin chain. `lnd` also supports Litecoin testnet4 (but not both BTC and LTC
at the same time), so when working with Litecoin be sure to set to parameters
for Litecoin accordingly.

# Accurate as of:
- _roasbeef/btcd commit:_ `f8c02aff4e7a807ba0c1349e2db03695d8e790e8` 
- _roasbeef/btcutil commit:_ `a259eaf2ec1b54653cdd67848a41867f280797ee` 
- _lightningnetwork/lnd commit:_ `08de2becf8d77fae192205172c4fb17bb09bd0dbf49e64aa323b2fcbf9fe2a35`
