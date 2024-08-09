package migration32

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	failureIndex = 8
	testPub      = Vertex{2, 202, 4}
	testPub2     = Vertex{22, 202, 4}

	pubkeyBytes, _ = hex.DecodeString(
		"598ec453728e0ffe0ae2f5e174243cf58f2" +
			"a3f2c83d2457b43036db568b11093",
	)
	pubKeyY = new(btcec.FieldVal)
	_       = pubKeyY.SetByteSlice(pubkeyBytes)
	pubkey  = btcec.NewPublicKey(new(btcec.FieldVal).SetInt(4), pubKeyY)

	paymentResultCommon1 = paymentResultCommon{
		id:               0,
		timeFwd:          time.Unix(0, 1),
		timeReply:        time.Unix(0, 2),
		success:          false,
		failureSourceIdx: &failureIndex,
		failure:          &lnwire.FailFeeInsufficient{},
	}

	paymentResultCommon2 = paymentResultCommon{
		id:        2,
		timeFwd:   time.Unix(0, 4),
		timeReply: time.Unix(0, 7),
		success:   true,
	}
)

// TestMigrateMCRouteSerialisation tests that the MigrateMCRouteSerialisation
// migration function correctly migrates the MC store from using the old route
// encoding to using the newer, more minimal route encoding.
func TestMigrateMCRouteSerialisation(t *testing.T) {
	customRecord := map[uint64][]byte{
		65536: {4, 2, 2},
	}

	resultsOld := []*paymentResultOld{
		{
			paymentResultCommon: paymentResultCommon1,
			route: &Route{
				TotalTimeLock: 100,
				TotalAmount:   400,
				SourcePubKey:  testPub,
				Hops: []*Hop{
					// A hop with MPP, AMP
					{
						PubKeyBytes:      testPub,
						ChannelID:        100,
						OutgoingTimeLock: 300,
						AmtToForward:     500,
						MPP: &MPP{
							paymentAddr: [32]byte{
								4, 5,
							},
							totalMsat: 900,
						},
						AMP: &AMP{
							rootShare: [32]byte{
								0, 0,
							},
							setID: [32]byte{
								5, 5, 5,
							},
							childIndex: 90,
						},
						CustomRecords: customRecord,
						Metadata:      []byte{6, 7, 7},
					},
					// A legacy hop.
					{
						PubKeyBytes:      testPub,
						ChannelID:        800,
						OutgoingTimeLock: 4,
						AmtToForward:     4,
						LegacyPayload:    true,
					},
					// A hop with a blinding key.
					{
						PubKeyBytes:      testPub,
						ChannelID:        800,
						OutgoingTimeLock: 4,
						AmtToForward:     4,
						BlindingPoint:    pubkey,
						EncryptedData: []byte{
							1, 2, 3,
						},
						TotalAmtMsat: 600,
					},
				},
			},
		},
		{
			paymentResultCommon: paymentResultCommon2,
			route: &Route{
				TotalTimeLock: 101,
				TotalAmount:   401,
				SourcePubKey:  testPub2,
				Hops: []*Hop{
					{
						PubKeyBytes:      testPub,
						ChannelID:        800,
						OutgoingTimeLock: 4,
						AmtToForward:     4,
						BlindingPoint:    pubkey,
						EncryptedData: []byte{
							1, 2, 3,
						},
						TotalAmtMsat: 600,
					},
				},
			},
		},
	}

	expectedResultsNew := []*paymentResultNew{
		{
			paymentResultCommon: paymentResultCommon1,
			route: &mcRoute{
				sourcePubKey: testPub,
				totalAmount:  400,
				hops: []*mcHop{
					{
						channelID:   100,
						pubKeyBytes: testPub,
						amtToFwd:    500,
					},
					{
						channelID:   800,
						pubKeyBytes: testPub,
						amtToFwd:    4,
					},
					{
						channelID:        800,
						pubKeyBytes:      testPub,
						amtToFwd:         4,
						hasBlindingPoint: true,
					},
				},
			},
		},
		{
			paymentResultCommon: paymentResultCommon2,
			route: &mcRoute{
				sourcePubKey: testPub2,
				totalAmount:  401,
				hops: []*mcHop{
					{
						channelID:        800,
						pubKeyBytes:      testPub,
						amtToFwd:         4,
						hasBlindingPoint: true,
					},
				},
			},
		},
	}

	// Prime the database with some mission control data that uses the
	// old route encoding.
	before := func(tx kvdb.RwTx) error {
		resultBucket, err := tx.CreateTopLevelBucket(resultsKey)
		if err != nil {
			return err
		}

		for _, result := range resultsOld {
			k, v, err := serializeOldResult(result)
			if err != nil {
				return err
			}

			if err := resultBucket.Put(k, v); err != nil {
				return err
			}
		}

		return nil
	}

	// After the migration, ensure that all the relevant info was
	// maintained.
	after := func(tx kvdb.RwTx) error {
		m := make(map[string]interface{})
		for _, result := range expectedResultsNew {
			k, v, err := serializeNewResult(result)
			if err != nil {
				return err
			}

			m[string(k)] = string(v)
		}

		return migtest.VerifyDB(tx, resultsKey, m)
	}

	migtest.ApplyMigration(
		t, before, after, MigrateMCRouteSerialisation, false,
	)
}
