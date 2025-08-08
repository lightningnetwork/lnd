package zpay32

// We use package `zpay32` rather than `zpay32_test` in order to share test data
// with the internal tests.

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var (
	testMillisat24BTC    = lnwire.MilliSatoshi(2400000000000)
	testMillisat2500uBTC = lnwire.MilliSatoshi(250000000)
	testMillisat25mBTC   = lnwire.MilliSatoshi(2500000000)
	testMillisat20mBTC   = lnwire.MilliSatoshi(2000000000)
	testMillisat10mBTC   = lnwire.MilliSatoshi(1000000000)

	testPaymentHash = [32]byte{
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05,
		0x06, 0x07, 0x08, 0x09, 0x00, 0x01, 0x02, 0x03,
		0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x01, 0x02,
	}

	testPaymentAddr = [32]byte{
		0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x01, 0x02,
		0x06, 0x07, 0x08, 0x09, 0x00, 0x01, 0x02, 0x03,
		0x08, 0x09, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05,
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	}

	specPaymentAddr = [32]byte{
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
	}

	testNonUTF8Str      = "1 cup coffee\xff\xfe\xfd"
	testEmptyString     = ""
	testCupOfCoffee     = "1 cup coffee"
	testCoffeeBeans     = "coffee beans"
	testCupOfNonsense   = "ナンセンス 1杯"
	testPleaseConsider  = "Please consider supporting this project"
	testPaymentMetadata = "payment metadata inside"

	testPrivKeyBytes, _     = hex.DecodeString("e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2db734")
	testPrivKey, testPubKey = btcec.PrivKeyFromBytes(testPrivKeyBytes)

	testDescriptionHashSlice = chainhash.HashB([]byte("One piece of chocolate cake, one icecream cone, one pickle, one slice of swiss cheese, one slice of salami, one lollypop, one piece of cherry pie, one sausage, one cupcake, and one slice of watermelon"))

	testExpiry0  = time.Duration(0) * time.Second
	testExpiry60 = time.Duration(60) * time.Second

	testAddrTestnet, _       = btcutil.DecodeAddress("mk2QpYatsKicvFVuTAQLBryyccRXMUaGHP", &chaincfg.TestNet3Params)
	testRustyAddr, _         = btcutil.DecodeAddress("1RustyRX2oai4EYYDpQGWvEL62BBGqN9T", &chaincfg.MainNetParams)
	testAddrMainnetP2SH, _   = btcutil.DecodeAddress("3EktnHQD7RiAE6uzMj2ZifT9YgRrkSgzQX", &chaincfg.MainNetParams)
	testAddrMainnetP2WPKH, _ = btcutil.DecodeAddress("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", &chaincfg.MainNetParams)
	testAddrMainnetP2WSH, _  = btcutil.DecodeAddress("bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3", &chaincfg.MainNetParams)

	testHopHintPubkeyBytes1, _ = hex.DecodeString("029e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255")
	testHopHintPubkey1, _      = btcec.ParsePubKey(testHopHintPubkeyBytes1)
	testHopHintPubkeyBytes2, _ = hex.DecodeString("039e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255")
	testHopHintPubkey2, _      = btcec.ParsePubKey(testHopHintPubkeyBytes2)

	testSingleHop = []HopHint{
		{
			NodeID:                    testHopHintPubkey1,
			ChannelID:                 0x0102030405060708,
			FeeBaseMSat:               0,
			FeeProportionalMillionths: 20,
			CLTVExpiryDelta:           3,
		},
	}
	testDoubleHop = []HopHint{
		{
			NodeID:                    testHopHintPubkey1,
			ChannelID:                 0x0102030405060708,
			FeeBaseMSat:               1,
			FeeProportionalMillionths: 20,
			CLTVExpiryDelta:           3,
		},
		{
			NodeID:                    testHopHintPubkey2,
			ChannelID:                 0x030405060708090a,
			FeeBaseMSat:               2,
			FeeProportionalMillionths: 30,
			CLTVExpiryDelta:           4,
		},
	}

	testMessageSigner = MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			hash := chainhash.HashB(msg)

			return ecdsa.SignCompact(testPrivKey, hash, true), nil
		},
	}

	emptyFeatures = lnwire.NewFeatureVector(nil, lnwire.Features)

	// Must be initialized in init().
	testDescriptionHash [32]byte

	testBlindedPK1Bytes, _ = hex.DecodeString("03f3311e948feb5115242c4e39" +
		"6c81c448ab7ee5fd24c4e24e66c73533cc4f98b8")
	testBlindedHopPK1, _   = btcec.ParsePubKey(testBlindedPK1Bytes)
	testBlindedPK2Bytes, _ = hex.DecodeString("03a8c97ed5cd40d474e4ef18c8" +
		"99854b25e5070106504cb225e6d2c112d61a805e")
	testBlindedHopPK2, _   = btcec.ParsePubKey(testBlindedPK2Bytes)
	testBlindedPK3Bytes, _ = hex.DecodeString("0220293926219d8efe733336e2" +
		"b674570dd96aa763acb3564e6e367b384d861a0a")
	testBlindedHopPK3, _   = btcec.ParsePubKey(testBlindedPK3Bytes)
	testBlindedPK4Bytes, _ = hex.DecodeString("02c75eb336a038294eaaf76015" +
		"8b2e851c3c0937262e35401ae64a1bee71a2e40c")
	testBlindedHopPK4, _ = btcec.ParsePubKey(testBlindedPK4Bytes)

	blindedPath1 = &BlindedPaymentPath{
		FeeBaseMsat:                 40,
		FeeRate:                     20,
		CltvExpiryDelta:             130,
		HTLCMinMsat:                 2,
		HTLCMaxMsat:                 100,
		Features:                    lnwire.EmptyFeatureVector(),
		FirstEphemeralBlindingPoint: testBlindedHopPK1,
		Hops: []*sphinx.BlindedHopInfo{
			{
				BlindedNodePub: testBlindedHopPK2,
				CipherText:     []byte{1, 2, 3, 4, 5},
			},
			{
				BlindedNodePub: testBlindedHopPK3,
				CipherText:     []byte{5, 4, 3, 2, 1},
			},
			{
				BlindedNodePub: testBlindedHopPK4,
				CipherText: []byte{
					1, 2, 3, 4, 5, 6, 7, 8, 9, 10,
					11, 12, 13, 14,
				},
			},
		},
	}

	blindedPath2 = &BlindedPaymentPath{
		FeeBaseMsat:                 4,
		FeeRate:                     2,
		CltvExpiryDelta:             10,
		HTLCMinMsat:                 0,
		HTLCMaxMsat:                 10,
		Features:                    lnwire.EmptyFeatureVector(),
		FirstEphemeralBlindingPoint: testBlindedHopPK4,
		Hops: []*sphinx.BlindedHopInfo{
			{
				BlindedNodePub: testBlindedHopPK3,
				CipherText:     []byte{1, 2, 3, 4, 5},
			},
		},
	}
)

func init() {
	copy(testDescriptionHash[:], testDescriptionHashSlice[:])
}

// TestDecodeEncode tests that an encoded invoice gets decoded into the expected
// Invoice object, and that reencoding the decoded invoice gets us back to the
// original encoded string.
//
//nolint:ll
func TestDecodeEncode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		encodedInvoice string
		valid          bool
		decodedInvoice func() *Invoice
		decodeOpts     []DecodeOption
		skipEncoding   bool
		beforeEncoding func(*Invoice)
	}{
		{
			encodedInvoice: "asdsaddnasdnas", // no hrp
			valid:          false,
		},
		{
			encodedInvoice: "lnbc1abcde", // too short
			valid:          false,
		},
		{
			encodedInvoice: "1asdsaddnv4wudz", // empty hrp
			valid:          false,
		},
		{
			encodedInvoice: "ln1asdsaddnv4wudz", // hrp too short
			valid:          false,
		},
		{
			encodedInvoice: "llts1dasdajtkfl6", // no "ln" prefix
			valid:          false,
		},
		{
			encodedInvoice: "lnts1dasdapukz0w", // invalid segwit prefix
			valid:          false,
		},
		{
			encodedInvoice: "lnbcm1aaamcu25m", // invalid amount
			valid:          false,
		},
		{
			encodedInvoice: "lnbc1000000000m1", // invalid amount
			valid:          false,
		},
		{
			encodedInvoice: "lnbc20m1pvjluezhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqspp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqfqqepvrhrm9s57hejg0p662ur5j5cr03890fa7k2pypgttmh4897d3raaq85a293e9jpuqwl0rnfuwzam7yr8e690nd2ypcq9hlkdwdvycqjhlqg5", // empty fallback address field
			valid:          false,
		},
		{
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3qjmp7lwpagxun9pygexvgpjdc4jdj85frqg00000000j9n4evl6mr5aj9f58zp6fyjzup6ywn3x6sk8akg5v4tgn2q8g4fhx05wf6juaxu9760yp46454gpg5mtzgerlzezqcqvjnhjh8z3g2qqsj5cgu", // invalid routing info length: not a multiple of 51
			valid:          false,
		},
		{
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqs9qrsgq7ea976txfraylvgzuxs8kgcw23ezlrszfnh8r6qtfpr6cxga50aj6txm9rxrydzd06dfeawfk6swupvz4erwnyutnjq7x39ymw6j38gp49qdkj", // Missing required s field.
			valid:          false,
		},
		{
			// no payment hash set
			encodedInvoice: "lnbc20m1pvjluezhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsjv38luh6p6s2xrv3mzvlmzaya43376h0twal5ax0k6p47498hp3hnaymzhsn424rxqjs0q7apn26yrhaxltq3vzwpqj9nc2r3kzwccsplnq470",
			valid:          false,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					Features:        emptyFeatures,
				}
			},
		},
		{
			// Both Description and DescriptionHash set.
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqs03vghs8y0kuj4ulrzls8ln7fnm9dk7sjsnqmghql6hd6jut36clkqpyuq0s5m6fhureyz0szx2qjc8hkgf4xc2hpw8jpu26jfeyvf4cpga36gt",
			valid:          false,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					PaymentAddr:     fn.Some(testPaymentAddr),
					Description:     &testPleaseConsider,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					Features:        emptyFeatures,
				}
			},
		},
		{
			// Neither Description nor DescriptionHash set.
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrs035emxws94pfuhsrusppn0yyfwvkra478rtdruqws3x32luss58kpy53jf5r28fsaf75w46pxw4k55dpzyvey5c46h4hfacrxmtcwrsphta58l",
			valid:          false,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat20mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					PaymentAddr: fn.Some(testPaymentAddr),
					Destination: testPubKey,
					Features:    emptyFeatures,
				}
			},
		},
		{
			// Has a few unknown fields, should just be ignored.
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrsvqgzxx3w2k6kjta8vpc0vgrt8qz0kcmasuj57e4ajga2alsthqhk76dmf9dpu26n3u6hpww557tef9t3wfzdv7sj67k982rkyztqsxcq66a5xd",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat20mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					PaymentAddr: fn.Some(testPaymentAddr),
					Description: &testPleaseConsider,
					Destination: testPubKey,
					Features:    emptyFeatures,
				}
			},
			skipEncoding: true, // Skip encoding since we don't have the unknown fields to encode.
		},
		{
			// Ignore unknown witness version in fallback address.
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrsxs4mt7um3uptqd8jc06vdsxzvwnqusnl2ft78083td02t03rcpvyxvs5chp2jk2v03t90yk96vsqnezt8gvdtry3jamxp2qkqzs6clsqqc90xq",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					PaymentAddr:     fn.Some(testPaymentAddr),
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					Features:        emptyFeatures,
				}
			},
			skipEncoding: true, // Skip encoding since we don't have the unknown fields to encode.
		},
		{
			// Ignore fields with unknown lengths.
			encodedInvoice: "lnbc241pveeq09pp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrst6cs035rjpreedlt6pwr9v5kve0rf9dtv9segdc0f7eq04n6edsre0cm58kdkn3qc46nug9zyn7cvf2eawg0k9vxp66kmjxfyngggmgq0f2g05",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat24BTC,
					Timestamp:       time.Unix(1503429093, 0),
					PaymentHash:     &testPaymentHash,
					PaymentAddr:     fn.Some(testPaymentAddr),
					Destination:     testPubKey,
					DescriptionHash: &testDescriptionHash,
					Features:        emptyFeatures,
				}
			},
			skipEncoding: true, // Skip encoding since we don't have the unknown fields to encode.
		},
		{
			// Invoice with no amount.
			encodedInvoice: "lnbc1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4jssp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrskc3flz5pl3lkue2eks7wg4m288mcns2xdm98a0xhruatalj2wpr4pnrdpavd7cfhfn3vlkl30s44cu2urmd0wcstqccmguqgdn3rfxcq0gjc43",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					Description: &testCupOfCoffee,
					PaymentAddr: fn.Some(testPaymentAddr),
					Destination: testPubKey,
					Features:    emptyFeatures,
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// Please make a donation of any amount using payment_hash 0001020304050607080900010203040506070809000102030405060708090102 to me @03e7156ae33b0a208d0744199163177e909e80176e55d97a2f221ede0f934dd9ad
			encodedInvoice: "lnbc1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaqsp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrsue98ndd4eucmnl835ecn29u0gmeezarwqz2kfynm97ckqnmtmx6kxhnafn66e8jrq8mav8emz9j6kcp4s669m7y5xgwarnnrwnn9e2gppzufhx",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					PaymentAddr: fn.Some(testPaymentAddr),
					Description: &testPleaseConsider,
					Destination: testPubKey,
					Features:    emptyFeatures,
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// Same as above, pubkey set in 'n' field.
			encodedInvoice: "lnbc241pveeq09pp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdqqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrsmmw6t0kplachqfzd76tvuuln6k2zqmxf2u57xcla9lcxfcrsrd48q5ekp2xul50yc6x4wu5qaen3zm3wvuukf9fespkytwqzgya5cxgpeyed0p",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat24BTC,
					Timestamp:   time.Unix(1503429093, 0),
					PaymentHash: &testPaymentHash,
					PaymentAddr: fn.Some(testPaymentAddr),
					Destination: testPubKey,
					Description: &testEmptyString,
					Features:    emptyFeatures,
				}
			},
		},
		{
			// Please send $3 for a cup of coffee to the same peer, within one minute
			encodedInvoice: "lnbc2500u1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4jsxqzpusp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygsxfj8ru680ls6z3cqpanuvtyqux2xuek3kr9xnfek7p258x4vhhns0hl5uvzryshanhlxnhl0fglhw9klpxl0rzsn6rv63uxwup0sgtspjum4ek",
			valid:          true,
			decodedInvoice: func() *Invoice {
				i, _ := NewInvoice(
					&chaincfg.MainNetParams,
					testPaymentHash,
					time.Unix(1496314658, 0),
					PaymentAddr(specPaymentAddr),
					Amount(testMillisat2500uBTC),
					Description(testCupOfCoffee),
					Destination(testPubKey),
					Expiry(testExpiry60))
				return i
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// Please send 0.0025 BTC for a cup of nonsense (ナンセンス 1杯) to the same peer, within one minute
			encodedInvoice: "lnbc2500u1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpquwpc4curk03c9wlrswe78q4eyqc7d8d0xqzpusp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygslten3f0ydjxklpy86g3sh6yz5ar7f0jxjas9dkdvggwwnvq2q93xle0gsfxmm3qmulh5y5q5gunhj3r2mqn8glden5g7d6n8mzxze7cq4mqn5r",
			valid:          true,
			decodedInvoice: func() *Invoice {
				i, _ := NewInvoice(
					&chaincfg.MainNetParams,
					testPaymentHash,
					time.Unix(1496314658, 0),
					PaymentAddr(specPaymentAddr),
					Amount(testMillisat2500uBTC),
					Description(testCupOfNonsense),
					Destination(testPubKey),
					Expiry(testExpiry60))
				return i
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// Now send $24 for an entire list of things (hashed)
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqssp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrs2twmp5m3znk6fl3z9pxxh6d3zamrkw40hrssvh6htgwehnjx68054gkahn3u2gsus27jtrzyylwn2t9fghz9ajzdlnrjl5xvgmm8xwcq6cgxv2",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					PaymentAddr:     fn.Some(testPaymentAddr),
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					Features:        emptyFeatures,
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// The same, on testnet, with a fallback address mk2QpYatsKicvFVuTAQLBryyccRXMUaGHP
			encodedInvoice: "lntb20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3x9et2e20v6pu37c5d9vax37wxq72un98sp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrsj7jhppareweywucw7cupxztnv6s2uj7ag2dpz8y8h6layltdsf3z862wwh0fy8vsgv3kc5q7vtzq0ge79rz9svjrnhlf6kp4re7kgacpu5wekx",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.TestNet3Params,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					PaymentAddr:     fn.Some(testPaymentAddr),
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrTestnet,
					Features:        emptyFeatures,
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, with fallback address 1RustyRX2oai4EYYDpQGWvEL62BBGqN9T with extra routing info to get to node 029e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3qjmp7lwpagxun9pygexvgpjdc4jdj85frzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvsp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrscghn64k30zfsuu3q769fnysxew6hs38pjv9jh98j3lygudqgs7j9tanauzgyypd9rqtfn3vg5jnvmkq8pjpftckj9wu60av737eg38gqg59t3q",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					PaymentAddr:     fn.Some(testPaymentAddr),
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testRustyAddr,
					RouteHints:      [][]HopHint{testSingleHop},
					Features:        emptyFeatures,
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, with fallback address 1RustyRX2oai4EYYDpQGWvEL62BBGqN9T with extra routing info to go via nodes 029e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255 then 039e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3qjmp7lwpagxun9pygexvgpjdc4jdj85fr9yq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqpqqqqq9qqqvpeuqafqxu92d8lr6fvg0r5gv0heeeqgcrqlnm6jhphu9y00rrhy4grqszsvpcgpy9qqqqqqgqqqqq7qqzqsp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrsdp2zsmy7777psknktem4mc46mn765z75nd68jdx7g87u0u6hh6q3pdnulk8f0k3pfnscp43ar9se540xuy098mx4rtn79h9grshy0cqq4td3hp",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					PaymentAddr:     fn.Some(testPaymentAddr),
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testRustyAddr,
					RouteHints:      [][]HopHint{testDoubleHop},
					Features:        emptyFeatures,
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, with fallback (p2sh) address 3EktnHQD7RiAE6uzMj2ZifT9YgRrkSgzQX
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfppj3a24vwu6r8ejrss3axul8rxldph2q7z9sp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrsjwltjy7ezwjl3p8ahw55rf25xgzgwqtfkkafnzhq9a2e7vv9actnhxclgsu5rtn6rsp7dmymdkaks38wrkn05w378kq0kjkhxjznvugq45ewl9",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					PaymentAddr:     fn.Some(testPaymentAddr),
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrMainnetP2SH,
					Features:        emptyFeatures,
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, please send $30 coffee beans supporting
			// features 9, 15 and 99, using secret 0x11...
			encodedInvoice: "lnbc25m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5vdhkven9v5sxyetpdeessp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs9q5sqqqqqqqqqqqqqqqpqsq67gye39hfg3zd8rgc80k32tvy9xk2xunwm5lzexnvpx6fd77en8qaq424dxgt56cag2dpt359k3ssyhetktkpqh24jqnjyw6uqd08sgptq44qu",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat25mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					PaymentAddr: fn.Some(specPaymentAddr),
					Description: &testCoffeeBeans,
					Destination: testPubKey,
					Features: lnwire.NewFeatureVector(
						lnwire.NewRawFeatureVector(9, 15, 99),
						lnwire.Features,
					),
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, please send $30 coffee beans supporting
			// features 9, 15, 99, and 100, using secret 0x11...
			encodedInvoice: "lnbc25m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5vdhkven9v5sxyetpdeessp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs9q4psqqqqqqqqqqqqqqqpqsqq40wa3khl49yue3zsgm26jrepqr2eghqlx86rttutve3ugd05em86nsefzh4pfurpd9ek9w2vp95zxqnfe2u7ckudyahsa52q66tgzcp6t2dyk",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat25mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					PaymentAddr: fn.Some(specPaymentAddr),
					Description: &testCoffeeBeans,
					Destination: testPubKey,
					Features: lnwire.NewFeatureVector(
						lnwire.NewRawFeatureVector(9, 15, 99, 100),
						lnwire.Features,
					),
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, with fallback (p2wpkh) address bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfppqw508d6qejxtdg4y5r3zarvary0c5xw7ksp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrsm8xkfajpctqqapx7qdv6ypkxcpj29dd5hgaqmk2vjf4vg4fzlsgh40cq8ykk7zk6cpu9v3r9xa2durvc04qkz648z45v6c9splv82hqptd6mq7",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					PaymentAddr:     fn.Some(testPaymentAddr),
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrMainnetP2WPKH,
					Features:        emptyFeatures,
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, with fallback (p2wsh) address bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfp4qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qsp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrsm2qp7lsn6k6fctq3lfpu4465hkup9yd8u5ja3n5yfcucg200247xdhgdpefg9lkp5remg4efcxvlnn4pt9smqmf7zj8uskqka4eg8jcqry7kel",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					PaymentAddr:     fn.Some(testPaymentAddr),
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrMainnetP2WSH,
					Features:        emptyFeatures,
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// Send 2500uBTC for a cup of coffee with a custom CLTV
			// expiry value.
			encodedInvoice: "lnbc2500u1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4jscqzysnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs4uwcaqm89zz7y0t8xlhyt80r8mc5yy2q33j0wctghfnwjf3j8fgzpe0q0reenxg92eweyxg67c32x4kqa0wt0xaq0t6s307kltapmssqglqfg3",
			valid:          true,
			decodedInvoice: func() *Invoice {
				i, _ := NewInvoice(
					&chaincfg.MainNetParams,
					testPaymentHash,
					time.Unix(1496314658, 0),
					PaymentAddr(specPaymentAddr),
					Amount(testMillisat2500uBTC),
					Description(testCupOfCoffee),
					Destination(testPubKey),
					CLTVExpiry(144),
				)

				return i
			},
		},
		{
			// Send 2500uBTC for a cup of coffee with a payment
			// address.
			encodedInvoice: "lnbc2500u1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4jsnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrsusq8nx2hdt3st3ankwz23xy9w7udvqq3f0mdlpc6ga5ew3y67u4qkx8vu72ejg5x6tqhyclm28r7r0mg6lx9x3vls9g6glp2qy3y34cpry54xp",
			valid:          true,
			decodedInvoice: func() *Invoice {
				i, _ := NewInvoice(
					&chaincfg.MainNetParams,
					testPaymentHash,
					time.Unix(1496314658, 0),
					PaymentAddr(specPaymentAddr),
					Amount(testMillisat2500uBTC),
					Description(testCupOfCoffee),
					Destination(testPubKey),
					PaymentAddr(testPaymentAddr),
				)

				return i
			},
		},
		{
			// Decode a mainnet invoice while expecting active net to be testnet
			encodedInvoice: "lnbc241pveeq09pp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdqqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66jd3m5klcwhq68vdsmx2rjgxeay5v0tkt2v5sjaky4eqahe4fx3k9sqavvce3capfuwv8rvjng57jrtfajn5dkpqv8yelsewtljwmmycq62k443",
			valid:          false,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.TestNet3Params,
					MilliSat:    &testMillisat24BTC,
					Timestamp:   time.Unix(1503429093, 0),
					PaymentHash: &testPaymentHash,
					Destination: testPubKey,
					Description: &testEmptyString,
					Features:    emptyFeatures,
				}
			},
			skipEncoding: true, // Skip encoding since we were given the wrong net
		},
		{
			// Please send 0.01 BTC with payment metadata 0x01fafaf0.
			encodedInvoice: "lnbc10m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdp9wpshjmt9de6zqmt9w3skgct5vysxjmnnd9jx2mq8q8a04uqsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs9q2gqqqqqqsgq7hf8he7ecf7n4ffphs6awl9t6676rrclv9ckg3d3ncn7fct63p6s365duk5wrk202cfy3aj5xnnp5gs3vrdvruverwwq7yzhkf5a3xqpd05wjc",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat10mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					Description: &testPaymentMetadata,
					Destination: testPubKey,
					PaymentAddr: fn.Some(specPaymentAddr),
					Features: lnwire.NewFeatureVector(
						lnwire.NewRawFeatureVector(8, 14, 48),
						lnwire.Features,
					),
					Metadata: []byte{0x01, 0xfa, 0xfa, 0xf0},
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// Invoice with blinded payment paths.
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4js5fdqqqqq2qqqqqpgqyzqqqqqqqqqqqqyqqqqqqqqqqqvsqqqqlnxy0ffrlt2y2jgtzw89kgr3zg4dlwtlfycn3yuek8x5eucnuchqps82xf0m2u6sx5wnjw7xxgnxz5kf09quqsv5zvkgj7d5kpzttp4qz7q5qsyqcyq5pzq2feycsemrh7wvendc4kw3tsmkt25a36ev6kfehrv7ecfkrp5zs9q5zqxqspqtr4avek5quzjn427asptzews5wrczfhychr2sq6ue9phmn35tjqcrspqgpsgpgxquyqjzstpsxsu59zqqqqqpqqqqqqyqq2qqqqqqqqqqqqqqqqqqqqqqqqpgqqqqk8t6endgpc99824amqzk9japgu8synwf3wx4qp4ej2r0h8rghypsqsygpf8ynzr8vwleenxdhzke69wrwed2nk8t9n2e8xudnm8pxcvxs2q5qsyqcyq5sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygslta4qyvme80a6nkwqn04ppmnexxpldzlzxhw6evm7xfw03gnqafp9qquce0g5ggv7xzj0sgpc5qylpz8m5uapfs664dq5tpwyh7vf4sqnzuuuh",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat20mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					PaymentAddr: fn.Some(specPaymentAddr),
					Description: &testCupOfCoffee,
					Destination: testPubKey,
					Features:    emptyFeatures,
					BlindedPaymentPaths: []*BlindedPaymentPath{
						blindedPath1,
						blindedPath2,
					},
				}
			},
			beforeEncoding: func(i *Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// Invoice with unknown feature bits but since the
			// WithErrorOnUnknownFeatureBit option is not provided,
			// it is not expected to error out.
			encodedInvoice: "lnbc25m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5vdhkven9v5sxyetpdeesnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs9q4psqqqqqqqqqqqqqqqpqsqahyu8p32qn33ppvs7dqmmvrlayfrnlzlx6we3fgwew8fz57mdefhw244wqmur8xyz4vyq8cyzvl0cwdc49enxxxh2sehlvzype3eqvgpw839ch",
			valid:          true,
			skipEncoding:   true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat25mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					PaymentAddr: fn.Some(specPaymentAddr),
					Description: &testCoffeeBeans,
					Destination: testPubKey,
					Features: lnwire.NewFeatureVector(
						lnwire.NewRawFeatureVector(
							9, 15, 99, 100,
						),
						lnwire.Features,
					),
				}
			},
			decodeOpts: []DecodeOption{
				WithKnownFeatureBits(map[lnwire.FeatureBit]string{
					9:  "9",
					15: "15",
					99: "99",
				}),
			},
		},
		{
			// Invoice with unknown feature bits with option set to
			// error out on unknown feature bit.
			encodedInvoice: "lnbc25m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5vdhkven9v5sxyetpdeesnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrs9q4psqqqqqqqqqqqqqqqpqsquvfjfm4l2q7clah3gye4thg6fv2xhzx8jhmavj0egz0as74sdrx8lveuyseans0f77emx9jxfjeuvhjhgm5gz6uflpm9ryxzh8scfgsp9u8njt",
			valid:          false,
			skipEncoding:   true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat25mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					PaymentAddr: fn.Some(testPaymentAddr),
					Description: &testCoffeeBeans,
					Destination: testPubKey,
					Features: lnwire.NewFeatureVector(
						lnwire.NewRawFeatureVector(
							9, 15, 99, 100,
						),
						lnwire.Features,
					),
				}
			},
			decodeOpts: []DecodeOption{
				WithKnownFeatureBits(map[lnwire.FeatureBit]string{
					9:  "9",
					15: "15",
					99: "99",
				}),
				WithErrorOnUnknownFeatureBit(),
			},
		},
	}

	for i, test := range tests {
		test := test

		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			t.Parallel()

			var decodedInvoice *Invoice
			net := &chaincfg.MainNetParams
			if test.decodedInvoice != nil {
				decodedInvoice = test.decodedInvoice()
				net = decodedInvoice.Net
			}

			invoice, err := Decode(
				test.encodedInvoice, net, test.decodeOpts...,
			)
			if !test.valid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, decodedInvoice, invoice)
			}

			if test.skipEncoding {
				return
			}

			if test.beforeEncoding != nil {
				test.beforeEncoding(decodedInvoice)
			}

			if decodedInvoice == nil {
				return
			}

			reencoded, err := decodedInvoice.Encode(
				testMessageSigner,
			)
			if !test.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.encodedInvoice, reencoded)
		})
	}
}

// TestNewInvoice tests that providing the optional arguments to the NewInvoice
// method creates an Invoice that encodes to the expected string.
func TestNewInvoice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		newInvoice     func() (*Invoice, error)
		encodedInvoice string
		valid          bool
	}{
		{
			// Both Description and DescriptionHash set.
			newInvoice: func() (*Invoice, error) {
				return NewInvoice(&chaincfg.MainNetParams,
					testPaymentHash, time.Unix(1496314658, 0),
					PaymentAddr(specPaymentAddr),
					DescriptionHash(testDescriptionHash),
					Description(testPleaseConsider))
			},
			valid: false, // Both Description and DescriptionHash set.
		},
		{
			// Invoice with no amount.
			newInvoice: func() (*Invoice, error) {
				return NewInvoice(
					&chaincfg.MainNetParams,
					testPaymentHash,
					time.Unix(1496314658, 0),
					PaymentAddr(specPaymentAddr),
					Description(testCupOfCoffee),
				)
			},
			valid:          true,
			encodedInvoice: "lnbc1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4jssp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygsk7mxg4jd0r638awa4kd6n0pzn6wtg33jg6ptdx68vclhlwpwxz7h4kn2tlah4zn3zwsgvn4xrcszhz0qu9nhem7u8lt4tz5vmkd8a5gpe4w8ra",
		},
		{
			// 'n' field set.
			newInvoice: func() (*Invoice, error) {
				return NewInvoice(&chaincfg.MainNetParams,
					testPaymentHash, time.Unix(1503429093, 0),
					PaymentAddr(specPaymentAddr),
					Amount(testMillisat24BTC),
					Description(testEmptyString),
					Destination(testPubKey))
			},
			valid:          true,
			encodedInvoice: "lnbc241pveeq09pp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdqqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygse2tu9s2lqeved59qef46rl2rsd5tkzdc9pjr6z3299aaaskgnk85zg96ng3xlknry8d9f9ssrqan6uj9dtm9mc00wcsam7awtkdgh6sqpf7nq7",
		},
		{
			// On mainnet, with fallback address 1RustyRX2oai4EYYDpQGWvEL62BBGqN9T with extra routing info to go via nodes 029e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255 then 039e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255
			newInvoice: func() (*Invoice, error) {
				return NewInvoice(&chaincfg.MainNetParams,
					testPaymentHash, time.Unix(1496314658, 0),
					PaymentAddr(specPaymentAddr),
					Amount(testMillisat20mBTC),
					DescriptionHash(testDescriptionHash),
					FallbackAddr(testRustyAddr),
					RouteHint(testDoubleHop),
				)
			},
			valid:          true,
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3qjmp7lwpagxun9pygexvgpjdc4jdj85fr9yq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqpqqqqq9qqqvpeuqafqxu92d8lr6fvg0r5gv0heeeqgcrqlnm6jhphu9y00rrhy4grqszsvpcgpy9qqqqqqgqqqqq7qqzqsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygsa492vv7x3elyzpkck3gyex3al2yywz0sm4clf3t4ayxz49005axh7qx3hp72wflvde27h8mp5rfv4ctvcqc8fy48r4mm69gpeucpadcq9vwr6c",
		},
		{
			// On simnet
			newInvoice: func() (*Invoice, error) {
				return NewInvoice(&chaincfg.SimNetParams,
					testPaymentHash, time.Unix(1496314658, 0),
					PaymentAddr(specPaymentAddr),
					Amount(testMillisat24BTC),
					Description(testEmptyString),
					Destination(testPubKey))
			},
			valid:          true,
			encodedInvoice: "lnsb241pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdqqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs6ypz232f2e8t2u957z8p82h0gkjpzcv4e3jaggs7vje4mqhtyvspl75kv9mkt3ye35rhh904m4mtz665jwkrmu2cdkjdfcz05s6ut5qqke0kry",
		},
		{
			// On regtest
			newInvoice: func() (*Invoice, error) {
				return NewInvoice(&chaincfg.RegressionNetParams,
					testPaymentHash, time.Unix(1496314658, 0),
					PaymentAddr(specPaymentAddr),
					Amount(testMillisat24BTC),
					Description(testEmptyString),
					Destination(testPubKey))
			},
			valid:          true,
			encodedInvoice: "lnbcrt241pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdqqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygsxuyyugag372x5v5ntk8ep3ag2hnk99acwn58yjnqna7htk364n0khlrx0htkvq58d280y9yeqjxpsludequ3sl5xwf9mthkr6dr8f5qp07r5jw",
		},
		{
			// Mainnet invoice with two blinded paths.
			newInvoice: func() (*Invoice, error) {
				return NewInvoice(&chaincfg.MainNetParams,
					testPaymentHash,
					time.Unix(1496314658, 0),
					PaymentAddr(specPaymentAddr),
					Amount(testMillisat20mBTC),
					Description(testCupOfCoffee),
					WithBlindedPaymentPath(blindedPath1),
					WithBlindedPaymentPath(blindedPath2),
				)
			},
			valid: true,
			//nolint:ll
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4js5fdqqqqq2qqqqqpgqyzqqqqqqqqqqqqyqqqqqqqqqqqvsqqqqlnxy0ffrlt2y2jgtzw89kgr3zg4dlwtlfycn3yuek8x5eucnuchqps82xf0m2u6sx5wnjw7xxgnxz5kf09quqsv5zvkgj7d5kpzttp4qz7q5qsyqcyq5pzq2feycsemrh7wvendc4kw3tsmkt25a36ev6kfehrv7ecfkrp5zs9q5zqxqspqtr4avek5quzjn427asptzews5wrczfhychr2sq6ue9phmn35tjqcrspqgpsgpgxquyqjzstpsxsu59zqqqqqpqqqqqqyqq2qqqqqqqqqqqqqqqqqqqqqqqqpgqqqqk8t6endgpc99824amqzk9japgu8synwf3wx4qp4ej2r0h8rghypsqsygpf8ynzr8vwleenxdhzke69wrwed2nk8t9n2e8xudnm8pxcvxs2q5qsyqcyq5sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygslta4qyvme80a6nkwqn04ppmnexxpldzlzxhw6evm7xfw03gnqafp9qquce0g5ggv7xzj0sgpc5qylpz8m5uapfs664dq5tpwyh7vf4sqnzuuuh",
		},
	}

	for i, test := range tests {
		test := test

		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			t.Parallel()

			invoice, err := test.newInvoice()
			if !test.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			encoded, err := invoice.Encode(testMessageSigner)
			require.NoError(t, err)

			require.Equal(t, test.encodedInvoice, encoded)
		})
	}
}

// TestMaxInvoiceLength tests that attempting to decode an invoice greater than
// maxInvoiceLength fails with ErrInvoiceTooLarge.
func TestMaxInvoiceLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		encodedInvoice string
		expectedError  error
	}{
		{
			// Valid since it is less than maxInvoiceLength.
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaqr84q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66y352euf40x77qqqqqlgqqqqqeqqpgp7w9t2uvas5gydqazpnytrzalfp85qzah9tkt69u3pahs0jdxantfzx3t83x4ummcqqqp8zqqqqqryqq9q8ec4dt3nkz3q35r5gxv3vvthayy7sqtku4we0ghjy8k7p7f5mkddxg69v7y6hn00qqqqyugqqqqqvsqq5r84q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66y35tfuf40x77qqqqqlgqqqqqeqqpgp7w9t2uvas5gydqazpnytrzalfp85qzah9tkt69u3pahs0jdxantfzx3t83k4ummcqqqp8zqqqqqryqq9q8ec4dt3nkz3q35r5gxv3vvthayy7sqtku4we0ghjy8k7p7f5mkddxg69z7y6hn00qqqqyugqqqqqqqqq5r84q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66y352euf40x77qqqqqlgqqqqqeqqpgp7w9t2uvas5gydqazpnytrzalfp85qzah9tkt69u3pahs0jdxantfzx3t83x4ummcqqqp8zqqqqqryqq9q8ec4dt3nkz3q35r5gxv3vvthayy7sqtku4we0ghjy8k7p7f5mkddxg69v7y6hn00qqqqyugqqqqqvsqq5r84q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66y35tfuf40x77qqqqqlgqqqqqeqqpgp7w9t2uvas5gydqazpnytrzalfp85qzah9tkt69u3pahs0jdxantfzx3t83k4ummcqqqp8zqqqqqryqq9q8ec4dt3nkz3q35r5gxv3vvthayy7sqtku4we0ghjy8k7p7f5mkddxg69z7y6hn00qqqqyugqqqqqqqqq5r84q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66y352euf40x77qqqqqlgqqqqqeqqpgp7w9t2uvas5gydqazpnytrzalfp85qzah9tkt69u3pahs0jdxantfzx3t83x4ummcqqqp8zqqqqqryqq9q8ec4dt3nkz3q35r5gxv3vvthayy7sqtku4we0ghjy8k7p7f5mkddxg69v7y6hn00qqqqyugqqqqqvsqq5r84q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66y35tfuf40x77qqqqqlgqqqqqeqqpgp7w9t2uvas5gydqazpnytrzalfp85qzah9tkt69u3pahs0jdxantfzx3t83k4ummcqqqp8zqqqqqryqq9q8ec4dt3nkz3q35r5gxv3vvthayy7sqtku4we0ghjy8k7p7f5mkddxg69z7y6hn00qqqqyugqqqqqqqqq5np4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5qszsvpcgpyqsyps8pqysqqgzqvyqjqqpqgpsgpgqqypqxpq9qcrslt5c3kg43xljqrxfded5j2d2ralscq5qjtn70vjhkz2vlw7vgzp8rwv5s0qn2wa3g2d3knz64cez09c55jsm70fhk8yh2g8ytffhy8gpfsn2p6",
		},
		{
			// Invalid since it is greater than maxInvoiceLength.
			encodedInvoice: "lnbc20m1pvjluezsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygspp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66fxq08u2ye04k6d2v2hgd8naeu0mfszlz2h6ze5mnufsc7y07s2z5v7vswj7jjcqumqchd646hnrx9pznxfj98565uc84zpc82x3nr8squ3hp9w",
			expectedError:  ErrInvoiceTooLarge,
		},
	}

	net := &chaincfg.MainNetParams

	for i, test := range tests {
		_, err := Decode(test.encodedInvoice, net)
		if err != test.expectedError {
			t.Errorf("Expected test %d to have error: %v, instead have: %v",
				i, test.expectedError, err)
			return
		}
	}
}

// TestInvoiceChecksumMalleability ensures that the malleability of the
// checksum in bech32 strings cannot cause a signature to become valid and
// therefore cause a wrong destination to be decoded for invoices where the
// destination is extracted from the signature.
func TestInvoiceChecksumMalleability(t *testing.T) {
	privKeyHex := "a50f3bdf9b6c4b1fdd7c51a8bbf4b5855cf381f413545ed155c0282f4412a1b1"
	privKeyBytes, _ := hex.DecodeString(privKeyHex)
	var paymentAddr [32]byte
	chain := &chaincfg.SimNetParams
	var payHash [32]byte
	ts := time.Unix(0, 0)

	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)
	msgSigner := MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			hash := chainhash.HashB(msg)
			return ecdsa.SignCompact(privKey, hash, true), nil
		},
	}
	opts := []func(*Invoice){Description("test83"), PaymentAddr(paymentAddr)}
	invoice, err := NewInvoice(chain, payHash, ts, opts...)
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := invoice.Encode(msgSigner)
	if err != nil {
		t.Fatal(err)
	}

	// Changing a bech32 string which checksum ends in "p" to "(q*)p" can
	// cause the checksum to return as a valid bech32 string _but_ the
	// signature field immediately preceding it would be mutaded.  In rare
	// cases (about 3%) it is still seen as a valid signature and public
	// key recovery causes a different node than the originally intended
	// one to be derived.
	//
	// We thus modify the checksum here and verify the invoice gets broken
	// enough that it fails to decode.
	if !strings.HasSuffix(encoded, "p") {
		t.Logf("Invoice: %s", encoded)
		t.Fatalf("Generated invoice checksum does not end in 'p'")
	}
	encoded = encoded[:len(encoded)-1] + "qp"

	_, err = Decode(encoded, chain)
	if err == nil {
		t.Fatalf("Did not get expected error when decoding invoice")
	}
}

func comparePubkeys(a, b *btcec.PublicKey) bool {
	if a == b {
		return true
	}
	if a == nil && b != nil {
		return false
	}
	if b == nil && a != nil {
		return false
	}
	return a.IsEqual(b)
}

func compareHashes(a, b *[32]byte) bool {
	if a == b {
		return true
	}
	if a == nil && b != nil {
		return false
	}
	if b == nil && a != nil {
		return false
	}
	return bytes.Equal(a[:], b[:])
}

func compareRouteHints(a, b []HopHint) error {
	if len(a) != len(b) {
		return fmt.Errorf("expected len routingInfo %d, got %d",
			len(a), len(b))
	}

	for i := 0; i < len(a); i++ {
		if !comparePubkeys(a[i].NodeID, b[i].NodeID) {
			return fmt.Errorf("expected routeHint nodeID %x, "+
				"got %x", a[i].NodeID.SerializeCompressed(),
				b[i].NodeID.SerializeCompressed())
		}

		if a[i].ChannelID != b[i].ChannelID {
			return fmt.Errorf("expected routeHint channelID "+
				"%d, got %d", a[i].ChannelID, b[i].ChannelID)
		}

		if a[i].FeeBaseMSat != b[i].FeeBaseMSat {
			return fmt.Errorf("expected routeHint feeBaseMsat %d, got %d",
				a[i].FeeBaseMSat, b[i].FeeBaseMSat)
		}

		if a[i].FeeProportionalMillionths != b[i].FeeProportionalMillionths {
			return fmt.Errorf("expected routeHint feeProportionalMillionths %d, got %d",
				a[i].FeeProportionalMillionths, b[i].FeeProportionalMillionths)
		}

		if a[i].CLTVExpiryDelta != b[i].CLTVExpiryDelta {
			return fmt.Errorf("expected routeHint cltvExpiryDelta "+
				"%d, got %d", a[i].CLTVExpiryDelta, b[i].CLTVExpiryDelta)
		}
	}

	return nil
}
