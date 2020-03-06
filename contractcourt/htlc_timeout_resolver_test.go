package contractcourt

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
)

type mockSigner struct {
}

func (m *mockSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) ([]byte, error) {
	return nil, nil
}

func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {
	return nil, nil
}

type mockWitnessBeacon struct {
	preImageUpdates chan lntypes.Preimage
	newPreimages    chan []lntypes.Preimage
	lookupPreimage  map[lntypes.Hash]lntypes.Preimage
}

func newMockWitnessBeacon() *mockWitnessBeacon {
	return &mockWitnessBeacon{
		preImageUpdates: make(chan lntypes.Preimage, 1),
		newPreimages:    make(chan []lntypes.Preimage),
		lookupPreimage:  make(map[lntypes.Hash]lntypes.Preimage),
	}
}

func (m *mockWitnessBeacon) SubscribeUpdates() *WitnessSubscription {
	return &WitnessSubscription{
		WitnessUpdates:     m.preImageUpdates,
		CancelSubscription: func() {},
	}
}

func (m *mockWitnessBeacon) LookupPreimage(payhash lntypes.Hash) (lntypes.Preimage, bool) {
	preimage, ok := m.lookupPreimage[payhash]
	if !ok {
		return lntypes.Preimage{}, false
	}
	return preimage, true
}

func (m *mockWitnessBeacon) AddPreimages(preimages ...lntypes.Preimage) error {
	m.newPreimages <- preimages
	return nil
}

// TestHtlcTimeoutResolver tests that the timeout resolver properly handles all
// variations of possible local+remote spends.
func TestHtlcTimeoutResolver(t *testing.T) {
	t.Parallel()

	fakePreimageBytes := bytes.Repeat([]byte{1}, lntypes.HashSize)

	var (
		htlcOutpoint wire.OutPoint
		fakePreimage lntypes.Preimage
	)
	fakeSignDesc := &input.SignDescriptor{
		Output: &wire.TxOut{},
	}

	copy(fakePreimage[:], fakePreimageBytes)

	signer := &mockSigner{}
	sweepTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: htlcOutpoint,
				Witness:          [][]byte{{0x01}},
			},
		},
	}
	fakeTimeout := int32(5)

	templateTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: htlcOutpoint,
			},
		},
	}

	testCases := []struct {
		// name is a human readable description of the test case.
		name string

		// remoteCommit denotes if the commitment broadcast was the
		// remote commitment or not.
		remoteCommit bool

		// timeout denotes if the HTLC should be let timeout, or if the
		// "remote" party should sweep it on-chain. This also affects
		// what type of resolution message we expect.
		timeout bool

		// txToBroadcast is a function closure that should generate the
		// transaction that should spend the HTLC output. Test authors
		// can use this to customize the witness used when spending to
		// trigger various redemption cases.
		txToBroadcast func() (*wire.MsgTx, error)
	}{
		// Remote commitment is broadcast, we time out the HTLC on
		// chain, and should expect a fail HTLC resolution.
		{
			name:         "timeout remote tx",
			remoteCommit: true,
			timeout:      true,
			txToBroadcast: func() (*wire.MsgTx, error) {
				witness, err := input.ReceiverHtlcSpendTimeout(
					signer, fakeSignDesc, sweepTx,
					fakeTimeout,
				)
				if err != nil {
					return nil, err
				}

				templateTx.TxIn[0].Witness = witness
				return templateTx, nil
			},
		},

		// Our local commitment is broadcast, we timeout the HTLC and
		// still expect an HTLC fail resolution.
		{
			name:         "timeout local tx",
			remoteCommit: false,
			timeout:      true,
			txToBroadcast: func() (*wire.MsgTx, error) {
				witness, err := input.SenderHtlcSpendTimeout(
					nil, txscript.SigHashAll, signer,
					fakeSignDesc, sweepTx,
				)
				if err != nil {
					return nil, err
				}

				templateTx.TxIn[0].Witness = witness
				return templateTx, nil
			},
		},

		// The remote commitment is broadcast, they sweep with the
		// pre-image, we should get a settle HTLC resolution.
		{
			name:         "success remote tx",
			remoteCommit: true,
			timeout:      false,
			txToBroadcast: func() (*wire.MsgTx, error) {
				witness, err := input.ReceiverHtlcSpendRedeem(
					nil, txscript.SigHashAll,
					fakePreimageBytes, signer,
					fakeSignDesc, sweepTx,
				)
				if err != nil {
					return nil, err
				}

				templateTx.TxIn[0].Witness = witness
				return templateTx, nil
			},
		},

		// The local commitment is broadcast, they sweep it with a
		// timeout from the output, and we should still get the HTLC
		// settle resolution back.
		{
			name:         "success local tx",
			remoteCommit: false,
			timeout:      false,
			txToBroadcast: func() (*wire.MsgTx, error) {
				witness, err := input.SenderHtlcSpendRedeem(
					signer, fakeSignDesc, sweepTx,
					fakePreimageBytes,
				)
				if err != nil {
					return nil, err
				}

				templateTx.TxIn[0].Witness = witness
				return templateTx, nil
			},
		},
	}

	notifier := &mockNotifier{
		epochChan: make(chan *chainntnfs.BlockEpoch),
		spendChan: make(chan *chainntnfs.SpendDetail),
		confChan:  make(chan *chainntnfs.TxConfirmation),
	}
	witnessBeacon := newMockWitnessBeacon()

	for _, testCase := range testCases {
		t.Logf("Running test case: %v", testCase.name)

		checkPointChan := make(chan struct{}, 1)
		incubateChan := make(chan struct{}, 1)
		resolutionChan := make(chan ResolutionMsg, 1)

		chainCfg := ChannelArbitratorConfig{
			ChainArbitratorConfig: ChainArbitratorConfig{
				Notifier:   notifier,
				PreimageDB: witnessBeacon,
				IncubateOutputs: func(wire.OutPoint,
					*lnwallet.OutgoingHtlcResolution,
					*lnwallet.IncomingHtlcResolution,
					uint32) error {

					incubateChan <- struct{}{}
					return nil
				},
				DeliverResolutionMsg: func(msgs ...ResolutionMsg) error {
					if len(msgs) != 1 {
						return fmt.Errorf("expected 1 "+
							"resolution msg, instead got %v",
							len(msgs))
					}

					resolutionChan <- msgs[0]
					return nil
				},
			},
		}

		cfg := ResolverConfig{
			ChannelArbitratorConfig: chainCfg,
			Checkpoint: func(_ ContractResolver) error {
				checkPointChan <- struct{}{}
				return nil
			},
		}
		resolver := &htlcTimeoutResolver{
			contractResolverKit: *newContractResolverKit(
				cfg,
			),
		}
		resolver.htlcResolution.SweepSignDesc = *fakeSignDesc

		// If the test case needs the remote commitment to be
		// broadcast, then we'll set the timeout commit to a fake
		// transaction to force the code path.
		if !testCase.remoteCommit {
			resolver.htlcResolution.SignedTimeoutTx = sweepTx
		}

		// With all the setup above complete, we can initiate the
		// resolution process, and the bulk of our test.
		var wg sync.WaitGroup
		resolveErr := make(chan error, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()

			_, err := resolver.Resolve()
			if err != nil {
				resolveErr <- err
			}
		}()

		// At the output isn't yet in the nursery, we expect that we
		// should receive an incubation request.
		select {
		case <-incubateChan:
		case err := <-resolveErr:
			t.Fatalf("unable to resolve HTLC: %v", err)
		case <-time.After(time.Second * 5):
			t.Fatalf("failed to receive incubation request")
		}

		// Next, the resolver should request a spend notification for
		// the direct HTLC output. We'll use the txToBroadcast closure
		// for the test case to generate the transaction that we'll
		// send to the resolver.
		spendingTx, err := testCase.txToBroadcast()
		if err != nil {
			t.Fatalf("unable to generate tx: %v", err)
		}
		select {
		case notifier.spendChan <- &chainntnfs.SpendDetail{
			SpendingTx: spendingTx,
		}:
		case <-time.After(time.Second * 5):
			t.Fatalf("failed to request spend ntfn")
		}

		if !testCase.timeout {
			// If the resolver should settle now, then we'll
			// extract the pre-image to be extracted and the
			// resolution message sent.
			select {
			case newPreimage := <-witnessBeacon.newPreimages:
				if newPreimage[0] != fakePreimage {
					t.Fatalf("wrong pre-image: "+
						"expected %v, got %v",
						fakePreimage, newPreimage)
				}

			case <-time.After(time.Second * 5):
				t.Fatalf("pre-image not added")
			}

			// Finally, we should get a resolution message with the
			// pre-image set within the message.
			select {
			case resolutionMsg := <-resolutionChan:
				// Once again, the pre-images should match up.
				if *resolutionMsg.PreImage != fakePreimage {
					t.Fatalf("wrong pre-image: "+
						"expected %v, got %v",
						fakePreimage, resolutionMsg.PreImage)
				}
			case <-time.After(time.Second * 5):
				t.Fatalf("resolution not sent")
			}
		} else {

			// Otherwise, the HTLC should now timeout.  First, we
			// should get a resolution message with a populated
			// failure message.
			select {
			case resolutionMsg := <-resolutionChan:
				if resolutionMsg.Failure == nil {
					t.Fatalf("expected failure resolution msg")
				}
			case <-time.After(time.Second * 5):
				t.Fatalf("resolution not sent")
			}

			// We should also get another request for the spend
			// notification of the second-level transaction to
			// indicate that it's been swept by the nursery, but
			// only if this is a local commitment transaction.
			if !testCase.remoteCommit {
				select {
				case notifier.spendChan <- &chainntnfs.SpendDetail{
					SpendingTx: spendingTx,
				}:
				case <-time.After(time.Second * 5):
					t.Fatalf("failed to request spend ntfn")
				}
			}
		}

		// In any case, before the resolver exits, it should checkpoint
		// its final state.
		select {
		case <-checkPointChan:
		case err := <-resolveErr:
			t.Fatalf("unable to resolve HTLC: %v", err)
		case <-time.After(time.Second * 5):
			t.Fatalf("check point not received")
		}

		wg.Wait()

		// Finally, the resolver should be marked as resolved.
		if !resolver.resolved {
			t.Fatalf("resolver should be marked as resolved")
		}
	}
}
