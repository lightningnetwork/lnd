package migration32

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
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

	customRecord = map[uint64][]byte{
		65536: {4, 2, 2},
	}

	resultOld1 = paymentResultOld{
		id:               0,
		timeFwd:          time.Unix(0, 1),
		timeReply:        time.Unix(0, 2),
		success:          false,
		failureSourceIdx: &failureIndex,
		failure:          &lnwire.FailFeeInsufficient{},
		route: &Route{
			TotalTimeLock: 100,
			TotalAmount:   400,
			SourcePubKey:  testPub,
			Hops: []*Hop{
				// A hop with MPP, AMP and custom
				// records.
				{
					PubKeyBytes:      testPub,
					ChannelID:        100,
					OutgoingTimeLock: 300,
					AmtToForward:     500,
					MPP: &MPP{
						paymentAddr: [32]byte{4, 5},
						totalMsat:   900,
					},
					AMP: &AMP{
						rootShare:  [32]byte{0, 0},
						setID:      [32]byte{5, 5, 5},
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
					EncryptedData:    []byte{1, 2, 3},
					TotalAmtMsat:     600,
				},
				// A hop with a blinding key and custom
				// records.
				{
					PubKeyBytes:      testPub,
					ChannelID:        800,
					OutgoingTimeLock: 4,
					AmtToForward:     4,
					CustomRecords:    customRecord,
					BlindingPoint:    pubkey,
					EncryptedData:    []byte{1, 2, 3},
					TotalAmtMsat:     600,
				},
			},
		},
	}

	resultOld2 = paymentResultOld{
		id:        2,
		timeFwd:   time.Unix(0, 4),
		timeReply: time.Unix(0, 7),
		success:   true,
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
					EncryptedData:    []byte{1, 2, 3},
					CustomRecords:    customRecord,
					TotalAmtMsat:     600,
				},
			},
		},
	}

	resultOld3 = paymentResultOld{
		id:               3,
		timeFwd:          time.Unix(0, 4),
		timeReply:        time.Unix(0, 7),
		success:          false,
		failure:          nil,
		failureSourceIdx: &failureIndex,
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
					EncryptedData:    []byte{1, 2, 3},
					CustomRecords:    customRecord,
					TotalAmtMsat:     600,
				},
			},
		},
	}

	//nolint:ll
	resultNew1Hop1 = &mcHop{
		channelID:   tlv.NewPrimitiveRecord[tlv.TlvType0, uint64](100),
		pubKeyBytes: tlv.NewRecordT[tlv.TlvType1](testPub),
		amtToFwd:    tlv.NewPrimitiveRecord[tlv.TlvType2, lnwire.MilliSatoshi](500),
		hasCustomRecords: tlv.SomeRecordT(
			tlv.ZeroRecordT[tlv.TlvType4, lnwire.TrueBoolean](),
		),
	}

	//nolint:ll
	resultNew1Hop2 = &mcHop{
		channelID:   tlv.NewPrimitiveRecord[tlv.TlvType0, uint64](800),
		pubKeyBytes: tlv.NewRecordT[tlv.TlvType1](testPub),
		amtToFwd:    tlv.NewPrimitiveRecord[tlv.TlvType2, lnwire.MilliSatoshi](4),
	}

	//nolint:ll
	resultNew1Hop3 = &mcHop{
		channelID:   tlv.NewPrimitiveRecord[tlv.TlvType0, uint64](800),
		pubKeyBytes: tlv.NewRecordT[tlv.TlvType1](testPub),
		amtToFwd:    tlv.NewPrimitiveRecord[tlv.TlvType2, lnwire.MilliSatoshi](4),
		hasBlindingPoint: tlv.SomeRecordT(
			tlv.ZeroRecordT[tlv.TlvType3, lnwire.TrueBoolean](),
		),
	}

	//nolint:ll
	resultNew1Hop4 = &mcHop{
		channelID:   tlv.NewPrimitiveRecord[tlv.TlvType0, uint64](800),
		pubKeyBytes: tlv.NewRecordT[tlv.TlvType1](testPub),
		amtToFwd:    tlv.NewPrimitiveRecord[tlv.TlvType2, lnwire.MilliSatoshi](4),
		hasCustomRecords: tlv.SomeRecordT(
			tlv.ZeroRecordT[tlv.TlvType4, lnwire.TrueBoolean](),
		),
		hasBlindingPoint: tlv.SomeRecordT(
			tlv.ZeroRecordT[tlv.TlvType3, lnwire.TrueBoolean](),
		),
	}

	//nolint:ll
	resultNew2Hop1 = &mcHop{
		channelID:   tlv.NewPrimitiveRecord[tlv.TlvType0, uint64](800),
		pubKeyBytes: tlv.NewRecordT[tlv.TlvType1](testPub),
		amtToFwd:    tlv.NewPrimitiveRecord[tlv.TlvType2, lnwire.MilliSatoshi](4),
		hasCustomRecords: tlv.SomeRecordT(
			tlv.ZeroRecordT[tlv.TlvType4, lnwire.TrueBoolean](),
		),
		hasBlindingPoint: tlv.SomeRecordT(
			tlv.ZeroRecordT[tlv.TlvType3, lnwire.TrueBoolean](),
		),
	}

	//nolint:ll
	resultNew1 = paymentResultNew{
		id: 0,
		timeFwd: tlv.NewPrimitiveRecord[tlv.TlvType0](
			uint64(time.Unix(0, 1).UnixNano()),
		),
		timeReply: tlv.NewPrimitiveRecord[tlv.TlvType1](
			uint64(time.Unix(0, 2).UnixNano()),
		),
		failure: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType3](
				newPaymentFailure(
					&failureIndex,
					&lnwire.FailFeeInsufficient{},
				),
			),
		),
		route: tlv.NewRecordT[tlv.TlvType2](mcRoute{
			sourcePubKey: tlv.NewRecordT[tlv.TlvType0](testPub),
			totalAmount:  tlv.NewRecordT[tlv.TlvType1, lnwire.MilliSatoshi](400),
			hops: tlv.NewRecordT[tlv.TlvType2, mcHops](mcHops{
				resultNew1Hop1,
				resultNew1Hop2,
				resultNew1Hop3,
				resultNew1Hop4,
			}),
		}),
	}

	//nolint:ll
	resultNew2 = paymentResultNew{
		id: 2,
		timeFwd: tlv.NewPrimitiveRecord[tlv.TlvType0, uint64](
			uint64(time.Unix(0, 4).UnixNano()),
		),
		timeReply: tlv.NewPrimitiveRecord[tlv.TlvType1, uint64](
			uint64(time.Unix(0, 7).UnixNano()),
		),
		route: tlv.NewRecordT[tlv.TlvType2](mcRoute{
			sourcePubKey: tlv.NewRecordT[tlv.TlvType0](testPub2),
			totalAmount:  tlv.NewRecordT[tlv.TlvType1, lnwire.MilliSatoshi](401),
			hops: tlv.NewRecordT[tlv.TlvType2](mcHops{
				resultNew2Hop1,
			}),
		}),
	}

	//nolint:ll
	resultNew3 = paymentResultNew{
		id: 3,
		timeFwd: tlv.NewPrimitiveRecord[tlv.TlvType0, uint64](
			uint64(time.Unix(0, 4).UnixNano()),
		),
		timeReply: tlv.NewPrimitiveRecord[tlv.TlvType1, uint64](
			uint64(time.Unix(0, 7).UnixNano()),
		),
		failure: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType3](
				newPaymentFailure(
					&failureIndex, nil,
				),
			),
		),
		route: tlv.NewRecordT[tlv.TlvType2](mcRoute{
			sourcePubKey: tlv.NewRecordT[tlv.TlvType0](testPub2),
			totalAmount:  tlv.NewRecordT[tlv.TlvType1, lnwire.MilliSatoshi](401),
			hops: tlv.NewRecordT[tlv.TlvType2](mcHops{
				resultNew2Hop1,
			}),
		}),
	}
)

// TestMigrateMCRouteSerialisation tests that the MigrateMCRouteSerialisation
// migration function correctly migrates the MC store from using the old route
// encoding to using the newer, more minimal route encoding.
func TestMigrateMCRouteSerialisation(t *testing.T) {
	var (
		resultsOld = []*paymentResultOld{
			&resultOld1, &resultOld2, &resultOld3,
		}
		expectedResultsNew = []*paymentResultNew{
			&resultNew1, &resultNew2, &resultNew3,
		}
	)

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
