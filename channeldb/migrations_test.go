package channeldb

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	prand "math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/coreos/bbolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
)

func createTestEdge(t *testing.T, db *DB) (*ChannelEdgeInfo,
	*ChannelEdgePolicy, *ChannelEdgePolicy) {
	node1, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	node2, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

	height := uint32(prand.Int31())
	edgeInfo, chanID := createEdge(height, 0, 0, 0, node1, node2)

	edge1 := &ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 chanID.ToUint64(),
		LastUpdate:                time.Unix(433453, 0),
		Flags:                     0,
		TimeLockDelta:             99,
		MinHTLC:                   2342135,
		FeeBaseMSat:               4352345,
		FeeProportionalMillionths: 3452352,
		Node:                      node2,
		ExtraOpaqueData:           []byte("new unknown feature2"),
		db:                        db,
	}
	edge2 := &ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 chanID.ToUint64(),
		LastUpdate:                time.Unix(124234, 0),
		Flags:                     1,
		TimeLockDelta:             99,
		MinHTLC:                   2342135,
		FeeBaseMSat:               4352345,
		FeeProportionalMillionths: 90392423,
		Node:                      node1,
		ExtraOpaqueData:           []byte("new unknown feature1"),
		db:                        db,
	}

	return &edgeInfo, edge1, edge2
}

// TestMigrateEdgePolices checks that we properly migrate edge polices,
// regardless of the state of the original database.
func TestMigrateEdgePolicies(t *testing.T) {
	t.Parallel()

	// A channel policy can be one of three types:
	// 1) in the db
	// 2) not in the db (we will migrate away from this)
	// 3) in the db, but marked as "unknown" (this is the new format we'll
	// migrate to)
	type policyStatus int
	const (
		existing policyStatus = iota
		nonExisting
		unknown
	)

	// We generate all combinations of previous database states an edge can
	// be in, to make sure we are able to migrate them all without
	// problems.
	type testCase struct {
		policy1Status  policyStatus
		policy2Status  policyStatus
		node1Exists    bool
		node2Exists    bool
		edgeInfoExists bool
	}

	var tests []testCase
	for _, pol1 := range []policyStatus{existing, nonExisting, unknown} {
		for _, pol2 := range []policyStatus{existing, nonExisting, unknown} {
			for _, node1 := range []bool{true, false} {
				for _, node2 := range []bool{true, false} {
					for _, e := range []bool{true, false} {
						tests = append(tests, testCase{
							policy1Status:  pol1,
							policy2Status:  pol2,
							node1Exists:    node1,
							node2Exists:    node2,
							edgeInfoExists: e,
						})
					}
				}
			}
		}
	}

	// helper method to set the node's policy to a specific state
	// in the db.
	setPolicy := func(tx *bbolt.Tx, node [33]byte, channelID uint64,
		status policyStatus) error {

		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			t.Fatalf("edge bucket did not exist")
		}
		var edgeKey [33 + 8]byte
		copy(edgeKey[:], node[:])
		byteOrder.PutUint64(edgeKey[33:], channelID)

		switch status {

		// We keep it, assert it is already there, and not unknown.
		case existing:
			pol := edges.Get(edgeKey[:])
			if pol == nil || len(pol) < 10 {
				return fmt.Errorf("unexpected policy: %v", pol)
			}

		// We delete the policy from the DB.
		case nonExisting:
			err := edges.Delete(edgeKey[:])
			if err != nil {
				return fmt.Errorf("unable to delete key: %v",
					err)
			}

			// We replace the policy in the DB with an unknown policy.
		case unknown:
			err := edges.Put(edgeKey[:], unknownPolicy)
			if err != nil {
				return fmt.Errorf("unable to put edgepolicy: "+
					"%v", err)
			}
		}
		return nil
	}

	// helper method to add or delete a node in the db.
	setNode := func(tx *bbolt.Tx, node [33]byte, exists bool) error {
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return fmt.Errorf("node bucket not found")
		}

		switch exists {

		// It should already exist.
		case true:
			nodeBytes := nodes.Get(node[:])
			if nodeBytes == nil {
				return fmt.Errorf("couldn't find node")
			}

		// Delete the node from the db.
		case false:
			err := nodes.Delete(node[:])
			if err != nil {
				return fmt.Errorf("unable to delete node")
			}
		}

		return nil
	}

	// helper method to add or delete the ede info from the db.
	setInfo := func(tx *bbolt.Tx, edge *ChannelEdgeInfo,
		exists bool) error {

		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			t.Fatalf("edge bucket did not exist")
		}

		edgeIndex := edges.Bucket(edgeIndexBucket)
		if edgeIndex == nil {
			t.Fatalf("edgeIndex bucket did not exist")
		}

		chanIndex := edges.Bucket(channelPointBucket)
		if chanIndex == nil {
			return fmt.Errorf("channe index not found")
		}

		var chanKey [8]byte
		binary.BigEndian.PutUint64(chanKey[:], edge.ChannelID)
		var op bytes.Buffer
		err := writeOutpoint(&op, &edge.ChannelPoint)
		if err != nil {
			return err
		}

		switch exists {

		// Check edge exists.
		case true:
			e := edgeIndex.Get(chanKey[:])
			if e == nil {
				return fmt.Errorf("edge not found")
			}

			c := chanIndex.Get(op.Bytes())
			if c == nil {
				return fmt.Errorf("channel not in chan index")
			}

		// Delete edge.
		case false:
			err := edgeIndex.Delete(chanKey[:])
			if err != nil {
				return fmt.Errorf("unable to delete edge: %v",
					err)
			}

			err = chanIndex.Delete(op.Bytes())
			if err != nil {
				return fmt.Errorf("unable to delete from chan "+
					"index: %v", err)
			}
		}

		return nil
	}

	// helper method that asserts the policy with the original status is in
	// the expected state after migration.
	assertMigratedPolicy := func(tx *bbolt.Tx, node [33]byte,
		channelID uint64, status policyStatus) error {

		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return fmt.Errorf("edge bucket did not exist")
		}

		var edgeKey [33 + 8]byte
		copy(edgeKey[:], node[:])
		byteOrder.PutUint64(edgeKey[33:], channelID)

		edgeBytes := edges.Get(edgeKey[:])

		// Policy statuses should should be either complete or unknown.
		switch status {

		// Existing policy should be here still, and not unknown.
		case existing:
			if edgeBytes == nil || len(edgeBytes) < 10 {
				return fmt.Errorf("expected existing policy " +
					"to be present")
			}

		// Otherwise it should be unknown.
		case nonExisting:
			fallthrough
		case unknown:
			if !bytes.Equal(edgeBytes, unknownPolicy) {
				return fmt.Errorf("expected policy to " +
					"be unknown")
			}
		}
		return nil
	}

	// Run through all test cases.
	for _, test := range tests {
		var chanID uint64

		// beforeMigrationFunc will take the db into the state set by
		// this particular test case.
		beforeMigrationFunc := func(db *DB) {
			graph := db.ChannelGraph()

			// We being by adding the edge, the two nodes and the
			// two channel edge policies to the DB.
			info, policy1, policy2 := createTestEdge(t, db)
			if err := graph.AddChannelEdge(info); err != nil {
				t.Fatalf("unable to add edge: %v", err)
			}
			if err := graph.UpdateEdgePolicy(policy1); err != nil {
				t.Fatalf("unable to update edge: %v", err)
			}
			if err := graph.UpdateEdgePolicy(policy2); err != nil {
				t.Fatalf("unable to update edge: %v", err)
			}

			// Then we delete or modify the information in the
			// database according to the testcase.
			err := db.Update(func(tx *bbolt.Tx) error {

				// Set the edge policies.
				err := setPolicy(
					tx, info.NodeKey1Bytes,
					policy1.ChannelID, test.policy1Status,
				)
				if err != nil {
					return err
				}

				err = setPolicy(
					tx, info.NodeKey2Bytes,
					policy2.ChannelID, test.policy2Status,
				)
				if err != nil {
					return err
				}

				// Set the nodes.
				err = setNode(
					tx, info.NodeKey1Bytes,
					test.node1Exists,
				)
				if err != nil {
					return err
				}

				err = setNode(
					tx, info.NodeKey2Bytes,
					test.node2Exists,
				)
				if err != nil {
					return err
				}

				// And finally set the edge info.
				err = setInfo(tx, info, test.edgeInfoExists)
				if err != nil {
					return err
				}

				return nil
			})
			if err != nil {
				t.Fatalf("unable to update db: %v", err)
			}

			chanID = info.ChannelID
		}

		// afterMigrationFunc asserts that the db is migrated to the
		// expected format.
		afterMigrationFunc := func(db *DB) {
			meta, err := db.FetchMeta(nil)
			if err != nil {
				t.Fatal(err)
			}

			if meta.DbVersionNumber != 1 {
				t.Fatal("migration 'migrateEdgePolicies' " +
					"wasn't applied")
			}

			graph := db.ChannelGraph()

			// If we were migrating from a DB having the edge info
			// present, it SHOULD still be present. If the original
			// DB didn't have the edge info, it should not be
			// there.
			shouldExist := test.edgeInfoExists
			_, _, found, err := graph.HasChannelEdge(chanID)
			if err != nil {
				t.Fatalf("unable to query for edge: %v", err)
			}

			if shouldExist != found {
				t.Fatalf("expected to find edge: %v ",
					test.edgeInfoExists)
			}

			// If the edge existed, we should be able to properly
			// fetch policies and nodes.
			if shouldExist {
				e, _, _, err := graph.FetchChannelEdgesByID(
					chanID,
				)
				if err != nil {
					t.Fatalf("unable to fetch edge: %v",
						err)
				}

				if e == nil {
					t.Fatalf("expected edge info to be " +
						"present")
				}

				err = db.View(func(tx *bbolt.Tx) error {
					err := assertMigratedPolicy(
						tx, e.NodeKey1Bytes,
						e.ChannelID, test.policy1Status,
					)
					if err != nil {
						return err
					}
					return assertMigratedPolicy(
						tx, e.NodeKey2Bytes,
						e.ChannelID, test.policy2Status,
					)

				})
				if err != nil {
					t.Fatal(err)
				}

				// Both nodes should be there.
				var numNodes int
				err = graph.ForEachNode(nil,
					func(*bbolt.Tx, *LightningNode) error {
						numNodes++
						return nil
					})
				if err != nil {
					t.Fatalf("unable to terate nodes: %v",
						err)
				}

				if numNodes != 2 {
					t.Fatalf("expected 2 nodes, found %v",
						numNodes)
				}
			}
		}

		applyMigration(t,
			beforeMigrationFunc,
			afterMigrationFunc,
			migrateEdgePolicies,
			false)
	}
}

// TestPaymentStatusesMigration checks that already completed payments will have
// their payment statuses set to Completed after the migration.
func TestPaymentStatusesMigration(t *testing.T) {
	t.Parallel()

	fakePayment := makeFakePayment()
	paymentHash := sha256.Sum256(fakePayment.PaymentPreimage[:])

	// Add fake payment to test database, verifying that it was created,
	// that we have only one payment, and its status is not "Completed".
	beforeMigrationFunc := func(d *DB) {
		if err := d.AddPayment(fakePayment); err != nil {
			t.Fatalf("unable to add payment: %v", err)
		}

		payments, err := d.FetchAllPayments()
		if err != nil {
			t.Fatalf("unable to fetch payments: %v", err)
		}

		if len(payments) != 1 {
			t.Fatalf("wrong qty of paymets: expected 1, got %v",
				len(payments))
		}

		paymentStatus, err := d.FetchPaymentStatus(paymentHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		// We should receive default status if we have any in database.
		if paymentStatus != StatusGrounded {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusGrounded.String(), paymentStatus.String())
		}

		// Lastly, we'll add a locally-sourced circuit and
		// non-locally-sourced circuit to the circuit map. The
		// locally-sourced payment should end up with an InFlight
		// status, while the other should remain unchanged, which
		// defaults to Grounded.
		err = d.Update(func(tx *bbolt.Tx) error {
			circuits, err := tx.CreateBucketIfNotExists(
				[]byte("circuit-adds"),
			)
			if err != nil {
				return err
			}

			groundedKey := make([]byte, 16)
			binary.BigEndian.PutUint64(groundedKey[:8], 1)
			binary.BigEndian.PutUint64(groundedKey[8:], 1)

			// Generated using TestHalfCircuitSerialization with nil
			// ErrorEncrypter, which is the case for locally-sourced
			// payments. No payment status should end up being set
			// for this circuit, since the short channel id of the
			// key is non-zero (e.g., a forwarded circuit). This
			// will default it to Grounded.
			groundedCircuit := []byte{
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x01,
				// start payment hash
				0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				// end payment hash
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x0f,
				0x42, 0x40, 0x00,
			}

			err = circuits.Put(groundedKey, groundedCircuit)
			if err != nil {
				return err
			}

			inFlightKey := make([]byte, 16)
			binary.BigEndian.PutUint64(inFlightKey[:8], 0)
			binary.BigEndian.PutUint64(inFlightKey[8:], 1)

			// Generated using TestHalfCircuitSerialization with nil
			// ErrorEncrypter, which is not the case for forwarded
			// payments, but should have no impact on the
			// correctness of the test. The payment status for this
			// circuit should be set to InFlight, since the short
			// channel id in the key is 0 (sourceHop).
			inFlightCircuit := []byte{
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x01,
				// start payment hash
				0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				// end payment hash
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x0f,
				0x42, 0x40, 0x00,
			}

			return circuits.Put(inFlightKey, inFlightCircuit)
		})
		if err != nil {
			t.Fatalf("unable to add circuit map entry: %v", err)
		}
	}

	// Verify that the created payment status is "Completed" for our one
	// fake payment.
	afterMigrationFunc := func(d *DB) {
		meta, err := d.FetchMeta(nil)
		if err != nil {
			t.Fatal(err)
		}

		if meta.DbVersionNumber != 1 {
			t.Fatal("migration 'paymentStatusesMigration' wasn't applied")
		}

		// Check that our completed payments were migrated.
		paymentStatus, err := d.FetchPaymentStatus(paymentHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		if paymentStatus != StatusCompleted {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusCompleted.String(), paymentStatus.String())
		}

		inFlightHash := [32]byte{
			0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		}

		// Check that the locally sourced payment was transitioned to
		// InFlight.
		paymentStatus, err = d.FetchPaymentStatus(inFlightHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		if paymentStatus != StatusInFlight {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusInFlight.String(), paymentStatus.String())
		}

		groundedHash := [32]byte{
			0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		}

		// Check that non-locally sourced payments remain in the default
		// Grounded state.
		paymentStatus, err = d.FetchPaymentStatus(groundedHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		if paymentStatus != StatusGrounded {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusGrounded.String(), paymentStatus.String())
		}
	}

	applyMigration(t,
		beforeMigrationFunc,
		afterMigrationFunc,
		paymentStatusesMigration,
		false)
}

// TestMigrateOptionalChannelCloseSummaryFields properly converts a
// ChannelCloseSummary to the v7 format, where optional fields have their
// presence indicated with boolean markers.
func TestMigrateOptionalChannelCloseSummaryFields(t *testing.T) {
	t.Parallel()

	chanState, err := createTestChannelState(nil)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}

	var chanPointBuf bytes.Buffer
	err = writeOutpoint(&chanPointBuf, &chanState.FundingOutpoint)
	if err != nil {
		t.Fatalf("unable to write outpoint: %v", err)
	}

	chanID := chanPointBuf.Bytes()

	testCases := []struct {
		closeSummary     *ChannelCloseSummary
		oldSerialization func(c *ChannelCloseSummary) []byte
	}{
		{
			// A close summary where none of the new fields are
			// set.
			closeSummary: &ChannelCloseSummary{
				ChanPoint:      chanState.FundingOutpoint,
				ShortChanID:    chanState.ShortChanID(),
				ChainHash:      chanState.ChainHash,
				ClosingTXID:    testTx.TxHash(),
				CloseHeight:    100,
				RemotePub:      chanState.IdentityPub,
				Capacity:       chanState.Capacity,
				SettledBalance: btcutil.Amount(50000),
				CloseType:      RemoteForceClose,
				IsPending:      true,

				// The last fields will be unset.
				RemoteCurrentRevocation: nil,
				LocalChanConfig:         ChannelConfig{},
				RemoteNextRevocation:    nil,
			},

			// In the old format the last field written is the
			// IsPendingField. It should be converted by adding an
			// extra boolean marker at the end to indicate that the
			// remaining fields are not there.
			oldSerialization: func(cs *ChannelCloseSummary) []byte {
				var buf bytes.Buffer
				err := WriteElements(&buf, cs.ChanPoint,
					cs.ShortChanID, cs.ChainHash,
					cs.ClosingTXID, cs.CloseHeight,
					cs.RemotePub, cs.Capacity,
					cs.SettledBalance, cs.TimeLockedBalance,
					cs.CloseType, cs.IsPending,
				)
				if err != nil {
					t.Fatal(err)
				}

				// For the old format, these are all the fields
				// that are written.
				return buf.Bytes()
			},
		},
		{
			// A close summary where the new fields are present,
			// but the optional RemoteNextRevocation field is not
			// set.
			closeSummary: &ChannelCloseSummary{
				ChanPoint:               chanState.FundingOutpoint,
				ShortChanID:             chanState.ShortChanID(),
				ChainHash:               chanState.ChainHash,
				ClosingTXID:             testTx.TxHash(),
				CloseHeight:             100,
				RemotePub:               chanState.IdentityPub,
				Capacity:                chanState.Capacity,
				SettledBalance:          btcutil.Amount(50000),
				CloseType:               RemoteForceClose,
				IsPending:               true,
				RemoteCurrentRevocation: chanState.RemoteCurrentRevocation,
				LocalChanConfig:         chanState.LocalChanCfg,

				// RemoteNextRevocation is optional, and here
				// it is not set.
				RemoteNextRevocation: nil,
			},

			// In the old format the last field written is the
			// LocalChanConfig. This indicates that the optional
			// RemoteNextRevocation field is not present. It should
			// be converted by adding boolean markers for all these
			// fields.
			oldSerialization: func(cs *ChannelCloseSummary) []byte {
				var buf bytes.Buffer
				err := WriteElements(&buf, cs.ChanPoint,
					cs.ShortChanID, cs.ChainHash,
					cs.ClosingTXID, cs.CloseHeight,
					cs.RemotePub, cs.Capacity,
					cs.SettledBalance, cs.TimeLockedBalance,
					cs.CloseType, cs.IsPending,
				)
				if err != nil {
					t.Fatal(err)
				}

				err = WriteElements(&buf, cs.RemoteCurrentRevocation)
				if err != nil {
					t.Fatal(err)
				}

				err = writeChanConfig(&buf, &cs.LocalChanConfig)
				if err != nil {
					t.Fatal(err)
				}

				// RemoteNextRevocation is not written.
				return buf.Bytes()
			},
		},
		{
			// A close summary where all fields are present.
			closeSummary: &ChannelCloseSummary{
				ChanPoint:               chanState.FundingOutpoint,
				ShortChanID:             chanState.ShortChanID(),
				ChainHash:               chanState.ChainHash,
				ClosingTXID:             testTx.TxHash(),
				CloseHeight:             100,
				RemotePub:               chanState.IdentityPub,
				Capacity:                chanState.Capacity,
				SettledBalance:          btcutil.Amount(50000),
				CloseType:               RemoteForceClose,
				IsPending:               true,
				RemoteCurrentRevocation: chanState.RemoteCurrentRevocation,
				LocalChanConfig:         chanState.LocalChanCfg,

				// RemoteNextRevocation is optional, and in
				// this case we set it.
				RemoteNextRevocation: chanState.RemoteNextRevocation,
			},

			// In the old format all the fields are written. It
			// should be converted by adding boolean markers for
			// all these fields.
			oldSerialization: func(cs *ChannelCloseSummary) []byte {
				var buf bytes.Buffer
				err := WriteElements(&buf, cs.ChanPoint,
					cs.ShortChanID, cs.ChainHash,
					cs.ClosingTXID, cs.CloseHeight,
					cs.RemotePub, cs.Capacity,
					cs.SettledBalance, cs.TimeLockedBalance,
					cs.CloseType, cs.IsPending,
				)
				if err != nil {
					t.Fatal(err)
				}

				err = WriteElements(&buf, cs.RemoteCurrentRevocation)
				if err != nil {
					t.Fatal(err)
				}

				err = writeChanConfig(&buf, &cs.LocalChanConfig)
				if err != nil {
					t.Fatal(err)
				}

				err = WriteElements(&buf, cs.RemoteNextRevocation)
				if err != nil {
					t.Fatal(err)
				}

				return buf.Bytes()
			},
		},
	}

	for _, test := range testCases {

		// Before the migration we must add the old format to the DB.
		beforeMigrationFunc := func(d *DB) {

			// Get the old serialization format for this test's
			// close summary, and it to the closed channel bucket.
			old := test.oldSerialization(test.closeSummary)
			err = d.Update(func(tx *bbolt.Tx) error {
				closedChanBucket, err := tx.CreateBucketIfNotExists(
					closedChannelBucket,
				)
				if err != nil {
					return err
				}
				return closedChanBucket.Put(chanID, old)
			})
			if err != nil {
				t.Fatalf("unable to add old serialization: %v",
					err)
			}
		}

		// After the migration it should be found in the new format.
		afterMigrationFunc := func(d *DB) {
			meta, err := d.FetchMeta(nil)
			if err != nil {
				t.Fatal(err)
			}

			if meta.DbVersionNumber != 1 {
				t.Fatal("migration wasn't applied")
			}

			// We generate the new serialized version, to check
			// against what is found in the DB.
			var b bytes.Buffer
			err = serializeChannelCloseSummary(&b, test.closeSummary)
			if err != nil {
				t.Fatalf("unable to serialize: %v", err)
			}
			newSerialization := b.Bytes()

			var dbSummary []byte
			err = d.View(func(tx *bbolt.Tx) error {
				closedChanBucket := tx.Bucket(closedChannelBucket)
				if closedChanBucket == nil {
					return errors.New("unable to find bucket")
				}

				// Get the serialized verision from the DB and
				// make sure it matches what we expected.
				dbSummary = closedChanBucket.Get(chanID)
				if !bytes.Equal(dbSummary, newSerialization) {
					return fmt.Errorf("unexpected new " +
						"serialization")
				}
				return nil
			})
			if err != nil {
				t.Fatalf("unable to view DB: %v", err)
			}

			// Finally we fetch the deserialized summary from the
			// DB and check that it is equal to our original one.
			dbChannels, err := d.FetchClosedChannels(false)
			if err != nil {
				t.Fatalf("unable to fetch closed channels: %v",
					err)
			}

			if len(dbChannels) != 1 {
				t.Fatalf("expected 1 closed channels, found %v",
					len(dbChannels))
			}

			dbChan := dbChannels[0]
			if !reflect.DeepEqual(dbChan, test.closeSummary) {
				dbChan.RemotePub.Curve = nil
				test.closeSummary.RemotePub.Curve = nil
				t.Fatalf("not equal: %v vs %v",
					spew.Sdump(dbChan),
					spew.Sdump(test.closeSummary))
			}

		}

		applyMigration(t,
			beforeMigrationFunc,
			afterMigrationFunc,
			migrateOptionalChannelCloseSummaryFields,
			false)
	}
}
