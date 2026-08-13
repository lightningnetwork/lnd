package itest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	lntestrpc "github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/stretchr/testify/require"
)

const (
	descriptorSweepSuccessKeyFamily = 9100
	descriptorSweepTimeoutKeyFamily = 9101
	descriptorSweepValue            = btcutil.Amount(1_000_000)
)

// testDescriptorSweep exercises both branches of a toy P2WSH HTLC descriptor:
//
//	(key_success && sha256(preimage)) ||
//	(key_timeout && after(cltv_height))
//
// The success case registers the descriptor before funding it and supplies the
// preimage only after the output confirms. The timeout case never supplies a
// preimage and verifies that reaching the absolute lock height triggers the
// sweep automatically.
func testDescriptorSweep(ht *lntest.HarnessTest) {
	tests := []struct {
		name            string
		providePreimage bool
		keyFamilyOffset int32
	}{
		{
			name:            "preimage branch",
			providePreimage: true,
			keyFamilyOffset: 0,
		},
		{
			name:            "cltv branch",
			providePreimage: false,
			keyFamilyOffset: 10,
		},
	}

	for _, test := range tests {
		test := test
		if !ht.Run(test.name, func(t *testing.T) {
			st := ht.Subtest(t)
			testDescriptorSweepBranch(
				st, test.providePreimage, test.keyFamilyOffset,
			)
		}) {
			break
		}
	}
}

func testDescriptorSweepBranch(ht *lntest.HarnessTest, providePreimage bool,
	keyFamilyOffset int32) {

	branchName := "preimage"
	if !providePreimage {
		branchName = "cltv"
	}
	alice := ht.NewNodeWithCoins("descriptor-sweeper-"+branchName, nil)

	successKey := alice.RPC.DeriveNextKey(&walletrpc.KeyReq{
		KeyFamily: descriptorSweepSuccessKeyFamily + keyFamilyOffset,
	})
	timeoutKey := alice.RPC.DeriveNextKey(&walletrpc.KeyReq{
		KeyFamily: descriptorSweepTimeoutKeyFamily + keyFamilyOffset,
	})

	preimage := bytes.Repeat([]byte{byte(keyFamilyOffset + 1)}, 32)
	paymentHash := sha256.Sum256(preimage)
	cltvHeight := ht.CurrentHeight() + 6
	descriptor := fmt.Sprintf(
		"wsh(or_i(and_v(v:pk(%s),sha256(%x)),"+
			"and_v(v:pk(%s),after(%d))))",
		hex.EncodeToString(successKey.RawKeyBytes), paymentHash,
		hex.EncodeToString(timeoutKey.RawKeyBytes), cltvHeight,
	)

	registerResp := alice.RPC.RegisterSweepDescriptor(
		&walletrpc.RegisterSweepDescriptorRequest{
			OutputDescriptor: descriptor,
			HeightHint:       ht.CurrentHeight(),
			MinConfs:         1,
			ExpectedValueSat: uint64(descriptorSweepValue),
			KeyBindings: []*walletrpc.SweepDescriptorKeyBinding{
				descriptorSweepKeyBinding(successKey),
				descriptorSweepKeyBinding(timeoutKey),
			},
			BudgetSat:     100_000,
			DeadlineDelta: 10,
			Immediate:     true,
			Label:         "itest toy htlc",
		},
	)
	require.Len(ht, registerResp.RegistrationId, 32)
	require.NotEmpty(ht, registerResp.Address)
	require.NotEmpty(ht, registerResp.PkScript)

	// Fund the descriptor only after the chain watch has been installed.
	fundResp := alice.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:        registerResp.Address,
		Amount:      int64(descriptorSweepValue),
		SatPerVbyte: 2,
	})
	fundHash, err := chainhash.NewHashFromStr(fundResp.Txid)
	require.NoError(ht, err)
	fundingTx := ht.AssertTxInMempool(*fundHash)
	fundingOutpoint := findDescriptorOutput(
		ht, fundingTx, registerResp.PkScript,
	)
	ht.MineBlockWithTx(fundingTx)

	waitSweepDescriptorState(
		ht, alice.RPC, registerResp.RegistrationId,
		walletrpc.SweepDescriptorState_SWEEP_DESCRIPTOR_STATE_WAITING,
	)

	if providePreimage {
		alice.RPC.AddSweepDescriptorData(
			&walletrpc.AddSweepDescriptorDataRequest{
				RegistrationId: registerResp.RegistrationId,
				Data: &walletrpc.AddSweepDescriptorDataRequest_Preimage{
					Preimage: preimage,
				},
			},
		)
	} else {
		// No data is added for this branch. Advancing the chain to the
		// descriptor's absolute lock height must make it satisfiable.
		require.Less(ht, ht.CurrentHeight(), cltvHeight)
		ht.MineEmptyBlocks(int(cltvHeight - ht.CurrentHeight()))
	}

	sweepTx := ht.GetNumTxsFromMempool(1)[0]
	descriptorInput := findDescriptorInput(ht, sweepTx, fundingOutpoint)
	pendingSweep := ht.AssertNumPendingSweeps(alice, 1)[0]
	require.Equal(ht, fundingOutpoint.Hash.String(),
		pendingSweep.Outpoint.TxidStr)
	require.Equal(ht, fundingOutpoint.Index,
		pendingSweep.Outpoint.OutputIndex)
	require.Equal(ht, walletrpc.WitnessType_DESCRIPTOR_WSH,
		pendingSweep.WitnessType)
	if providePreimage {
		// lnd uses the current height as the default transaction
		// locktime. What matters here is that the success branch does
		// not inherit the future CLTV from the timeout branch.
		require.Less(ht, sweepTx.LockTime, cltvHeight)
		require.True(ht, witnessContains(descriptorInput.Witness, preimage),
			"success witness does not contain the supplied preimage")
	} else {
		require.Equal(ht, cltvHeight, sweepTx.LockTime)
		require.False(ht, witnessContains(descriptorInput.Witness, preimage),
			"timeout witness unexpectedly contains the preimage")
	}

	ht.MineBlockWithTx(sweepTx)
	waitSweepDescriptorState(
		ht, alice.RPC, registerResp.RegistrationId,
		walletrpc.SweepDescriptorState_SWEEP_DESCRIPTOR_STATE_SWEPT,
	)
}

func descriptorSweepKeyBinding(
	key *signrpc.KeyDescriptor) *walletrpc.SweepDescriptorKeyBinding {

	return &walletrpc.SweepDescriptorKeyBinding{
		DescriptorKey: hex.EncodeToString(key.RawKeyBytes),
		KeyLocator:    key.KeyLoc,
	}
}

func findDescriptorOutput(ht *lntest.HarnessTest, tx *wire.MsgTx,
	pkScript []byte) wire.OutPoint {

	for index, txOut := range tx.TxOut {
		if bytes.Equal(txOut.PkScript, pkScript) {
			return wire.OutPoint{
				Hash:  tx.TxHash(),
				Index: uint32(index),
			}
		}
	}

	require.Fail(ht, "descriptor output not found in funding transaction")
	return wire.OutPoint{}
}

func findDescriptorInput(ht *lntest.HarnessTest, tx *wire.MsgTx,
	want wire.OutPoint) *wire.TxIn {

	for _, txIn := range tx.TxIn {
		if txIn.PreviousOutPoint == want {
			return txIn
		}
	}

	require.Failf(ht, "descriptor output not swept", "transaction %v does not "+
		"spend %v", tx.TxHash(), want)
	return nil
}

func witnessContains(witness wire.TxWitness, want []byte) bool {
	for _, element := range witness {
		if bytes.Equal(element, want) {
			return true
		}
	}

	return false
}

func waitSweepDescriptorState(ht *lntest.HarnessTest,
	rpc *lntestrpc.HarnessRPC, registrationID []byte,
	want walletrpc.SweepDescriptorState) {

	require.Eventually(ht, func() bool {
		resp := rpc.ListSweepDescriptors(
			&walletrpc.ListSweepDescriptorsRequest{
				RegistrationId: registrationID,
			},
		)
		return len(resp.Descriptors) == 1 &&
			resp.Descriptors[0].State == want
	}, lntest.DefaultTimeout, 100*time.Millisecond,
		"descriptor sweep did not reach state %v", want)
}
