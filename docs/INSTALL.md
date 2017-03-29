# Installation for Lnd and Btcd

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

### Create lnd.conf:
**On MacOS, located at:**
/Users/[username]/Library/Application Support/Lnd/lnd.conf

**On Linux, located at:**
~/.lnd/lnd.conf

**lnd.conf:**
(Note: Replace `kek` with the username and password you prefer.)
```
[Application Options]
rpcuser=kek
rpcpass=kek
btcdhost=127.0.0.1
debuglevel=debug
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
(Note: It may take several minutes to find segwit-enabled peers.)

### Add a limited username and password to btcd.conf and restart
(Note: Replace `kek` with the username and password you prefer.)

**On Linux:**
```
$ sed -i 's#; rpclimituser=whatever_limited_username_you_want#rpclimituser=kek#' ~/.btcd/btcd.conf
$ sed -i 's#; rpclimitpass=#rpclimitpass=kek#' ~/.btcd/btcd.conf
```

**On MacOS:**
```
$ sed -i 's#; rpclimituser=whatever_limited_username_you_want#rpclimituser=kek#' /Users/[username]/Library/Application Support/Btcd/btcd.conf
$ sed -i 's#; rpclimitpass=#rpclimitpass=kek#' /Users/[username]/Library/Application Support/Btcd/btcd.conf
```

If you did not have a `btcd.conf` file yet, you can simply paste the following into it:
````
[Application Options]
rpclimituser=<the username you picked in lnd.conf>
rpclimitpass=<the password you picked in lnd.conf>
````

**Then, regardless of OS:**
```
$ btcctl --testnet stop
$ btcd --testnet
```

### Check to see that peers are connected:
```
$ btcctl --testnet getpeerinfo | more
```

### Start Lnd: (Once btcd has synced testnet)
```
$ lnd --testnet
```

### Start Lnd on Simnet: (Doesn’t require testnet syncing.)
```
$ lnd --simnet --debughtlc
```

#### Accurate as of:
roasbeef/btcd commit: 54362e17a5b80643ef1e12edc08895a2e2a1202b

roasbeef/btcutil commit: d347e49

lightningnetwork/lnd commit: d7b36c6
