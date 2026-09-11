# LND #9952 Implementation Guide

## Changes Needed

### 1. lnrpc/routerrpc/router.proto
Add to `QueryRoutesRequest`:
```protobuf
bytes payment_addr = 15;
```

Add to `SendToRouteRequest`:
```protobuf
MPPRecord mpp_record = 3;
```

### 2. lnrpc/lightning.proto
Ensure MPPRecord is defined:
```protobuf
message MPPRecord {
    bytes payment_addr = 1;
    uint64 total_amt_msat = 2;
}
```

### 3. routing/router.go
Update `QueryRoutes` to include MPP record.

### 4. cmd/lncli/commands.go
Add flags for payment_addr.

## Generate & Test
```bash
make rpc
make check
```
