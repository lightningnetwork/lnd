package itest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
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

// testDescriptorSweep exercises both branches of toy native P2WSH and P2TR
// HTLC descriptors:
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
		taproot         bool
		providePreimage bool
		keyFamilyOffset int32
	}{
		{
			name:            "p2wsh preimage branch",
			taproot:         false,
			providePreimage: true,
			keyFamilyOffset: 0,
		},
		{
			name:            "p2wsh cltv branch",
			taproot:         false,
			providePreimage: false,
			keyFamilyOffset: 10,
		},
		{
			name:            "p2tr preimage branch",
			taproot:         true,
			providePreimage: true,
			keyFamilyOffset: 20,
		},
		{
			name:            "p2tr cltv branch",
			taproot:         true,
			providePreimage: false,
			keyFamilyOffset: 30,
		},
	}

	for _, test := range tests {
		if !ht.Run(test.name, func(t *testing.T) {
			st := ht.Subtest(t)
			testDescriptorSweepBranch(
				st, test.taproot, test.providePreimage,
				test.keyFamilyOffset,
			)
		}) {

			break
		}
	}
}

func testDescriptorSweepBranch(ht *lntest.HarnessTest, taproot,
	providePreimage bool, keyFamilyOffset int32) {

	branchName := "preimage"
	if !providePreimage {
		branchName = "cltv"
	}
	descriptorType := "p2wsh"
	if taproot {
		descriptorType = "p2tr"
	}
	alice := ht.NewNodeWithCoins(
		"descriptor-sweeper-"+descriptorType+"-"+branchName, nil,
	)

	successKey := alice.RPC.DeriveNextKey(&walletrpc.KeyReq{
		KeyFamily: descriptorSweepSuccessKeyFamily + keyFamilyOffset,
	})
	timeoutKey := alice.RPC.DeriveNextKey(&walletrpc.KeyReq{
		KeyFamily: descriptorSweepTimeoutKeyFamily + keyFamilyOffset,
	})

	preimage := bytes.Repeat([]byte{byte(keyFamilyOffset + 1)}, 32)
	paymentHash := sha256.Sum256(preimage)
	cltvHeight := ht.CurrentHeight() + 6
	descriptor, keyBindings := descriptorSweepHTLC(
		ht, taproot, successKey, timeoutKey, paymentHash, cltvHeight,
		keyFamilyOffset,
	)

	registerResp := alice.RPC.RegisterSweepDescriptor(
		&walletrpc.RegisterSweepDescriptorRequest{
			OutputDescriptor: descriptor,
			HeightHint:       ht.CurrentHeight(),
			MinConfs:         1,
			ExpectedValueSat: uint64(descriptorSweepValue),
			KeyBindings:      keyBindings,
			BudgetSat:        100_000,
			DeadlineDelta:    10,
			Immediate:        true,
			Label:            "itest toy htlc",
		},
	)
	require.Len(ht, registerResp.RegistrationId, 32)
	require.NotEmpty(ht, registerResp.Address)
	require.NotEmpty(ht, registerResp.PkScript)
	if taproot {
		require.True(ht, txscript.IsPayToTaproot(registerResp.PkScript))
	} else {
		require.True(
			ht, txscript.IsPayToWitnessScriptHash(
				registerResp.PkScript,
			),
		)
	}

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
				Data: &walletrpc.
					AddSweepDescriptorDataRequest_Preimage{
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
	witnessType := walletrpc.WitnessType_DESCRIPTOR_WSH
	if taproot {
		witnessType = walletrpc.WitnessType_DESCRIPTOR_TR
	}
	require.Equal(ht, witnessType, pendingSweep.WitnessType)
	assertDescriptorSweepWitness(
		ht, descriptorInput.Witness, registerResp.PkScript, taproot,
	)
	if providePreimage {
		// lnd uses the current height as the default transaction
		// locktime. What matters here is that the success branch does
		// not inherit the future CLTV from the timeout branch.
		require.Less(ht, sweepTx.LockTime, cltvHeight)
		require.True(
			ht, witnessContains(descriptorInput.Witness, preimage),
			"success witness is missing the supplied preimage",
		)
	} else {
		require.Equal(ht, cltvHeight, sweepTx.LockTime)
		require.False(
			ht, witnessContains(descriptorInput.Witness, preimage),
			"timeout witness unexpectedly contains the preimage",
		)
	}

	ht.MineBlockWithTx(sweepTx)
	waitSweepDescriptorState(
		ht, alice.RPC, registerResp.RegistrationId,
		walletrpc.SweepDescriptorState_SWEEP_DESCRIPTOR_STATE_SWEPT,
	)
}

func descriptorSweepHTLC(ht *lntest.HarnessTest, taproot bool,
	successKey, timeoutKey *signrpc.KeyDescriptor,
	paymentHash [sha256.Size]byte, cltvHeight uint32,
	keyFamilyOffset int32) (string,
	[]*walletrpc.SweepDescriptorKeyBinding) {

	if !taproot {
		success := hex.EncodeToString(successKey.RawKeyBytes)
		timeout := hex.EncodeToString(timeoutKey.RawKeyBytes)
		return fmt.Sprintf(
				"wsh(or_i(and_v(v:pk(%s),sha256(%x)),"+
					"and_v(v:pk(%s),after(%d))))",
				success, paymentHash, timeout, cltvHeight,
			), []*walletrpc.SweepDescriptorKeyBinding{
				descriptorSweepKeyBinding(
					ht, successKey, false,
				),
				descriptorSweepKeyBinding(
					ht, timeoutKey, false,
				),
			}
	}

	_, internalKey := btcec.PrivKeyFromBytes(
		bytes.Repeat([]byte{byte(keyFamilyOffset + 2)}, 32),
	)
	internal := hex.EncodeToString(schnorr.SerializePubKey(internalKey))
	success := descriptorSweepKeyString(ht, successKey, true)
	timeout := descriptorSweepKeyString(ht, timeoutKey, true)

	// Put the two alternatives in distinct leaves. A successful spend must
	// therefore reveal both the selected leaf and its Merkle control path.
	// The deterministic internal key remains external and unbound, so lnd
	// can use only the script paths.
	descriptor := fmt.Sprintf(
		"tr(%s,{and_v(v:pk(%s),sha256(%x)),"+
			"and_v(v:pk(%s),after(%d))})",
		internal, success, paymentHash, timeout, cltvHeight,
	)

	return descriptor, []*walletrpc.SweepDescriptorKeyBinding{
		descriptorSweepKeyBinding(ht, successKey, true),
		descriptorSweepKeyBinding(ht, timeoutKey, true),
	}
}

func descriptorSweepKeyBinding(ht *lntest.HarnessTest,
	key *signrpc.KeyDescriptor,
	taproot bool) *walletrpc.SweepDescriptorKeyBinding {

	return &walletrpc.SweepDescriptorKeyBinding{
		DescriptorKey: descriptorSweepKeyString(ht, key, taproot),
		KeyLocator:    key.KeyLoc,
	}
}

func descriptorSweepKeyString(ht *lntest.HarnessTest,
	key *signrpc.KeyDescriptor, taproot bool) string {

	if !taproot {
		return hex.EncodeToString(key.RawKeyBytes)
	}

	pubKey, err := btcec.ParsePubKey(key.RawKeyBytes)
	require.NoError(ht, err)

	return hex.EncodeToString(schnorr.SerializePubKey(pubKey))
}

func assertDescriptorSweepWitness(ht *lntest.HarnessTest,
	witness wire.TxWitness, pkScript []byte, taproot bool) {

	require.NotEmpty(ht, witness)
	version, program, err := txscript.ExtractWitnessProgramInfo(pkScript)
	require.NoError(ht, err)

	if !taproot {
		require.Equal(ht, 0, version)
		witnessScript := witness[len(witness)-1]
		scriptHash := sha256.Sum256(witnessScript)
		require.Equal(ht, program, scriptHash[:])

		return
	}

	require.Equal(ht, 1, version)
	require.GreaterOrEqual(ht, len(witness), 3)
	revealedScript := witness[len(witness)-2]
	controlBlockBytes := witness[len(witness)-1]
	require.NotEmpty(ht, revealedScript)
	require.Len(ht, controlBlockBytes,
		txscript.ControlBlockBaseSize+txscript.ControlBlockNodeSize)

	controlBlock, err := txscript.ParseControlBlock(controlBlockBytes)
	require.NoError(ht, err)
	require.NoError(ht, txscript.VerifyTaprootLeafCommitment(
		controlBlock, program, revealedScript,
	))
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

	require.Failf(
		ht, "descriptor output not swept",
		"transaction %v does not spend %v", tx.TxHash(), want,
	)

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
