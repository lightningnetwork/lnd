# PR #10769 Completion Checklist

## Completed ✅
- [x] Add `change_addr` field to `SendCoinsRequest` proto
- [x] Add `--change_addr` flag to `sendcoins` CLI command
- [x] Pass change address to SendCoins RPC call
- [x] Push branch to fork
- [x] Create upstream PR

## Remaining Work 📝

### 1. Regenerate Protobuf Code
```bash
make rpc
```
This will regenerate:
- `lnrpc/lightning.pb.go`
- `lnrpc/lightning_grpc.pb.go`

### 2. Add Unit Tests
Create `cmd/commands/commands_test.go` or update existing tests:
```go
func TestSendCoinsWithChangeAddr(t *testing.T) {
    // Test that change_addr is properly parsed and passed
}
```

### 3. Add Integration Tests
Update `lntest/itest/lnd_on_chain_test.go`:
```go
func testSendCoinsWithChangeAddr(t *harnessTest) {
    // Test sending coins with custom change address
}
```

### 4. Update Documentation
- Update `docs/cmd/lncli.md` if it exists
- Add example usage to PR description

### 5. Wallet Implementation
The wallet needs to actually use the change address. Update:
- `lnwallet/btcwallet/btcwallet.go` - `SendCoins` method
- `rpcserver.go` - Pass change address to wallet

## How to Get Write Access

1. **Become a regular contributor:**
   - Submit quality PRs
   - Review other PRs
   - Participate in discussions

2. **Join the LND community:**
   - Discord: https://discord.gg/lightning
   - IRC: #lnd on Libera.Chat
   - Mailing list: lightning-dev

3. **Build reputation:**
   - Fix bugs
   - Add features
   - Write documentation

4. **Apply for maintainership:**
   - After consistent contributions
   - Ask existing maintainers

## Current Status
- PR is open and ready for review
- Basic implementation is complete
- Needs protobuf regeneration and tests
