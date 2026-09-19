# LND #10271 Implementation Guide

## Changes Needed

### 1. lnrpc/lightning.proto
Add to `SendCoinsRequest`:
```protobuf
string change_address = 15;
```

### 2. cmd/lncli/commands.go
Add flag:
```go
cli.StringFlag{
    Name:  "change_address",
    Usage: "Optional address to send change to",
},
```

### 3. lnd/lnwallet/btcwallet/btcwallet.go
Update `SendCoins` to use change address.

## Generate Protobuf
```bash
make rpc
```

## Test
```bash
make check
```
