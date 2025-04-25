# LND Sweeper Fee Bump Test - Competency Test Summary

## Overview
This test implements a fee bumping mechanism for the Lightning Network Daemon (LND) sweeper component. The test verifies that the sweeper can properly increase transaction fees to ensure timely confirmation of transactions in the mempool.

## Test Purpose
The test verifies three key aspects of the sweeper's fee bumping functionality:
1. Initial fee rate is set correctly (low fee rate)
2. Fee bumping increases the fee rate monotonically
3. Fee bumping respects the maximum fee rate limit

## Key Components

### Test Parameters
```go
const (
    maxFeeRate    = 50     // sat/vbyte
    deadlineDelta = 10     // blocks
    budget        = 100000 // sats
    testTimeout   = 30 * time.Second
    pollInterval  = 1 * time.Second
    maxTestDuration = 5 * time.Minute
)
```

### Main Test Function
The `testSweeperFeeBump` function:
1. Creates a context with timeout for the entire test
2. Uses Alice's node for testing
3. Creates and sends an initial low-fee transaction
4. Monitors the mempool for fee-bumped versions
5. Verifies fee rate increases and maximum fee rate compliance

### Helper Functions

#### `sendLowFeeTx`
- Creates a new address
- Prepares a SendOutputsRequest with:
  - Value: 50,000,000 satoshis
  - Fee rate: 1 sat/vbyte
- Sends the transaction
- Verifies initial fee rate is below maximum

#### `waitForTxInMempool`
- Waits for a transaction to appear in the mempool
- Uses a timeout to prevent infinite waiting
- Returns the transaction when found

#### `getTxFeeRate`
- Calculates the fee rate of a transaction
- Formula: FeeSat / VSize
- Returns the fee rate in sat/vbyte

## Improvements Made

1. **Enhanced Context Handling**
   - Added proper timeout handling with `context.WithTimeout`
   - Implemented cleanup with `defer cancel()`

2. **Better Error Handling**
   - Improved error messages in `Fatalf` calls
   - Added maximum test duration check
   - More robust fee rate verification

3. **Code Organization**
   - Created helper function `sendLowFeeTx` for low fee transaction creation
   - Refined mempool monitoring logic
   - Improved code readability and maintainability

4. **Cleanup Procedures**
   - Ensured graceful test failures with timeouts
   - Proper resource management

## Test Flow
1. Initialize test with timeout context
2. Create and send low-fee transaction
3. Monitor mempool for fee-bumped versions
4. Track fee rate increases
5. Verify fee rate increases are monotonic
6. Confirm maximum fee rate is respected
7. Clean up resources

## Verification Points
- Initial fee rate is below maximum
- Fee rates increase monotonically
- Final fee rate meets or exceeds target
- Test completes within timeout
- Resources are properly cleaned up

## Notes
- The test uses a maximum fee rate of 50 sat/vbyte
- Test timeout is set to 30 seconds
- Maximum test duration is 5 minutes
- Polling interval is 1 second 