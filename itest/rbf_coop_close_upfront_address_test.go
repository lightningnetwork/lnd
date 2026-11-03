//go:build integration

package itest

import (
    "bytes"
    "github.com/btcsuite/btcd/btcutil/v2"
    "github.com/btcsuite/btcd/chainhash/v2"
    "github.com/lightningnetwork/lnd/lnrpc"
    "github.com/lightningnetwork/lnd/lntest"
    "github.com/stretchr/testify/require"
)

// testRbfCoopCloseUpfrontAddress observes the original and corrected real daemon
// close-address contract through the same two-node Core regtest journey.
func testRbfCoopCloseUpfrontAddress(ht *lntest.HarnessTest) {
    ht.SetFeeEstimate(250)
    ht.SetFeeEstimateWithConf(250, 6)
    args := append(
        lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS),
        "--protocol.rbf-coop-close",
    )
    alice := ht.NewNode("Alice", args)
    bob := ht.NewNode("Bob", args)
    ht.EnsureConnected(alice, bob)
    ht.FundCoins(btcutil.Amount(2_000_000), alice)

    addrReq := &lnrpc.NewAddressRequest{
        Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
    }
    upfront := alice.RPC.NewAddress(addrReq).Address
    conflicting := alice.RPC.NewAddress(addrReq).Address
    require.NotEmpty(ht, upfront)
    require.NotEqual(ht, upfront, conflicting)

    cp := ht.OpenChannel(alice, bob, lntest.OpenChannelParams{
        Amt: btcutil.Amount(1_000_000),
        PushAmt: btcutil.Amount(500_000),
        CommitmentType: lnrpc.CommitmentType_ANCHORS,
        CloseAddress: upfront,
    })
    ht.AssertChannelExists(alice, cp)
    ht.AssertChannelExists(bob, cp)
    ht.Logf("RBF_COOP_UPFRONT=%s CONFLICT=%s", upfront, conflicting)

    req := &lnrpc.CloseChannelRequest{
        ChannelPoint: cp,
        Force: false,
        NoWait: true,
        SatPerVbyte: 5,
        DeliveryAddress: conflicting,
    }

    // Reject a conflicting explicit destination without closing the channel.
    err := ht.CloseChannelAssertErr(alice, req)
    require.ErrorContains(ht, err,
        "shutdown script does not match upfront shutdown script")
    ht.AssertChannelExists(alice, cp)
    ht.AssertChannelExists(bob, cp)
    ht.AssertNumTxsInMempool(0)

    // A valid default close of the same channel must still succeed.
    req.DeliveryAddress = ""

    stream := alice.RPC.CloseChannel(req)
    var pendingID *chainhash.Hash
    for n := 0; n < 6; n++ {
        update, err := ht.ReceiveCloseChannelUpdate(stream)
        require.NoError(ht, err, "close RPC returned error")
        if pending := update.GetClosePending(); pending != nil {
            pendingID, err = chainhash.NewHash(pending.Txid)
            require.NoError(ht, err)
            break
        }
    }
    require.NotNil(ht, pendingID, "close stream omitted a pending event")

    // RBF closes may first report a provisional transaction, then negotiate
    // and publish a distinct final txid. Check Core's actual settlement.
    mempool := ht.Miner().AssertNumTxsInMempool(1)
    closeID := mempool[0]
    tx := ht.GetRawTransaction(closeID)
    msgTx := tx.MsgTx()
    require.Len(ht, msgTx.TxIn, 1, "close must spend channel funding only")
    fundingID := ht.GetChanPointFundingTxid(cp)
    require.Equal(ht, fundingID, msgTx.TxIn[0].PreviousOutPoint.Hash)
    require.Equal(ht, cp.OutputIndex,
        msgTx.TxIn[0].PreviousOutPoint.Index)

    upfrontScript := ht.PayToAddrScript(ht.DecodeAddress(upfront))
    conflictScript := ht.PayToAddrScript(ht.DecodeAddress(conflicting))
    foundUpfront := false
    foundConflict := false
    totalOut := int64(0)
    for _, out := range msgTx.TxOut {
        totalOut += out.Value
        foundUpfront = foundUpfront || bytes.Equal(out.PkScript, upfrontScript)
        foundConflict = foundConflict || bytes.Equal(out.PkScript, conflictScript)
    }
    require.True(ht, foundUpfront, "settlement skipped committed upfront script")
    require.False(ht, foundConflict, "settlement used conflicting delivery script")
    fee := int64(1_000_000) - totalOut
    require.Positive(ht, fee, "cooperative closing fee")
    require.LessOrEqual(ht, fee, int64(50_000), "fee budget")
    ht.Logf("RBF_COOP_SETTLEMENT_PROVISIONAL_TXID=%s FINAL_CORE_TXID=%s FEE_SAT=%d", pendingID, closeID, fee)

    block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
    ht.AssertTxInBlock(block, closeID)
    // RBF close streams can send a second ClosePending for the final
    // broadcast txid. Consume all pending events until the confirmed
    // close event; every pending event must remain a valid close status.
    var confirmed *chainhash.Hash
    for n := 0; n < 8; n++ {
        event, err := ht.ReceiveCloseChannelUpdate(stream)
        require.NoError(ht, err)
        if close := event.GetChanClose(); close != nil {
            confirmed, err = chainhash.NewHash(close.ClosingTxid)
            require.NoError(ht, err)
            break
        }
        pending := event.GetClosePending()
        require.NotNil(ht, pending, "unexpected close stream update")
        ht.Logf("RBF_COOP_CLOSE_STREAM_PENDING_TXID=%x", pending.Txid)
    }
    require.NotNil(ht, confirmed, "stream omitted final confirmed close")
    require.Equal(ht, closeID, *confirmed,
        "stream final close must identify Core-confirmed transaction")
    ht.Logf("RBF upfront default close confirmed: txid=%s fee=%d sat",
        closeID, fee)
    ht.Shutdown(alice)
    ht.Shutdown(bob)
}
