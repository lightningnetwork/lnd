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
	testAddrMainnetP2TR, _   = btcutil.DecodeAddress("bc1pptdvg0d2nj99568"+
		"qn6ssdy4cygnwuxgw2ukmnwgwz7jpqjz2kszse2s3lm",
		&chaincfg.MainNetParams)

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
			// no payment hash set
			encodedInvoice: "lnbc20m1pvjluezhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsjv38luh6p6s2xrv3mzvlmzaya43376h0twal5ax0k6p47498hp3hnaymzhsn424rxqjs0q7apn26yrhaxltq3vzwpqj9nc2r3kzwccsplnq470",
			valid:          false,
		},
		{
			// Both Description and DescriptionHash set.
			encodedInvoice: "lnbc20m1pvjluezsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygspp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsu6zmmjhtn0uhrqje9x4y2c05mjvvg0ftwg32cnjkzs2vwmuf7ltysjlvkvh2pkgg20ssp4muprn93ezasdgn6aezu5ec5g54nju9kkgqtf6fht",
			valid:          false,
		},
		{
			// Neither Description nor DescriptionHash set.
			encodedInvoice: "lnbc20m1pvjluezsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygspp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqwul6apumndhf3t4v50sdx0vc9jma3cjfq49reu9a6rsadhs933nsau8vwumegq0scs492xx5s6zp6rmr50gd2pdkv285kzsr7zt5xjsp69w7kw",
			valid:          false,
		},
		{
			// Has a few unknown fields, should just be ignored.
			encodedInvoice: "lnbc20m1pvjluezsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygspp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaqtq0w4hxkmn0wahrqvgzq0w4hxkmn0wahrqvselxnzkx7sdr4jjeqz8c022yhhkwjskhg32kcprhc08atq06megxyy8xclrsuc2tpfnayhskjj6dxmy34330rxd32cp3evjcdaqqkmdqprtqpuh",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat20mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					Description: &testPleaseConsider,
					Destination: testPubKey,
					Features:    emptyFeatures,
					PaymentAddr: fn.Some(specPaymentAddr),
				}
			},
			skipEncoding: true, // Skip encoding since we don't have the unknown fields to encode.
		},
		{
			// Ignore unknown witness version in fallback address.
			encodedInvoice: "lnbc20m1pvjluezsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygspp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfp4z6yn92zrp97a6q5hhh8swys7uf4hm9tr8a0xylnk26fvkg3jx0sdsvy8e63r5vpmxxzfh7jm87h0unhsssqfara08llrvz2zphyw2hreqsa89482kprux7pcgvsuxa8rd4vqj6duzvgat40jk8ke7uyed3asqqqa2fg",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					Features:        emptyFeatures,
					PaymentAddr:     fn.Some(specPaymentAddr),
				}
			},
			skipEncoding: true, // Skip encoding since we don't have the unknown fields to encode.
		},
		{
			// Ignore fields with unknown lengths.
			encodedInvoice: "lnbc241pveeq09pp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqppsqgpsgpgxquyqjqqpqgpsgpgxquyqjqqpqgpsgpgxquyqjqgznp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66npjz44wxwc2yzxsw3qej933wl5sn6qpwmj4m9az7gs7mc8exnwe45hp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqshpskmm8utp5qqmw6ysf8h2yuqmgmudkagnv20d7fqgltr74mwxpsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygsspszyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3hgvfdj2zty30gcyl6flpqrdztw56n8klxwrg2yqw3tjwkqy7k3s9av2rf2s4u5wm4k4e44rd2cvjk9cutlaqwv7uklgnd3zvrt79zgqqutvndk",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat24BTC,
					Timestamp:       time.Unix(1503429093, 0),
					PaymentHash:     &testPaymentHash,
					Destination:     testPubKey,
					DescriptionHash: &testDescriptionHash,
					Features:        emptyFeatures,
					PaymentAddr:     fn.Some(specPaymentAddr),
				}
			},
			skipEncoding: true, // Skip encoding since we don't have the unknown fields to encode.
		},
		{
			// Invoice with no amount.
			encodedInvoice: "lnbc1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4jssp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygsk7mxg4jd0r638awa4kd6n0pzn6wtg33jg6ptdx68vclhlwpwxz7h4kn2tlah4zn3zwsgvn4xrcszhz0qu9nhem7u8lt4tz5vmkd8a5gpe4w8ra",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					Description: &testCupOfCoffee,
					Destination: testPubKey,
					Features:    emptyFeatures,
					PaymentAddr: fn.Some(specPaymentAddr),
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
			// Please make a donation of any amount using rhash 0001020304050607080900010203040506070809000102030405060708090102 to me @03e7156ae33b0a208d0744199163177e909e80176e55d97a2f221ede0f934dd9ad
			encodedInvoice: "lnbc1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaqsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs3u3lp8kstkqc5h5u2s960n5qjkzzsqasr9rle9z3rw08frm5j62x8qhj66t83qccmkx7zh29puy7zpq0nf7qws0dkq5q6mav7n87rhgp5skgmx",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					Description: &testPleaseConsider,
					Destination: testPubKey,
					Features:    emptyFeatures,
					PaymentAddr: fn.Some(specPaymentAddr),
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
			encodedInvoice: "lnbc241pveeq09pp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdqqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygse2tu9s2lqeved59qef46rl2rsd5tkzdc9pjr6z3299aaaskgnk85zg96ng3xlknry8d9f9ssrqan6uj9dtm9mc00wcsam7awtkdgh6sqpf7nq7",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat24BTC,
					Timestamp:   time.Unix(1503429093, 0),
					PaymentHash: &testPaymentHash,
					Destination: testPubKey,
					Description: &testEmptyString,
					Features:    emptyFeatures,
					PaymentAddr: fn.Some(specPaymentAddr),
				}
			},
		},
		{
			// Please send $3 for a cup of coffee to the same peer, within 1 minute
			encodedInvoice: "lnbc2500u1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4jsxqzpusp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygsxfj8ru680ls6z3cqpanuvtyqux2xuek3kr9xnfek7p258x4vhhns0hl5uvzryshanhlxnhl0fglhw9klpxl0rzsn6rv63uxwup0sgtspjum4ek",
			valid:          true,
			decodedInvoice: func() *Invoice {
				i, _ := NewInvoice(
					&chaincfg.MainNetParams,
					testPaymentHash,
					time.Unix(1496314658, 0),
					Amount(testMillisat2500uBTC),
					Description(testCupOfCoffee),
					Destination(testPubKey),
					Expiry(testExpiry60),
					PaymentAddr(specPaymentAddr))
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
			// Please send 0.0025 BTC for a cup of nonsense (ナンセンス 1杯) to the same peer, within 1 minute
			encodedInvoice: "lnbc2500u1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpquwpc4curk03c9wlrswe78q4eyqc7d8d0xqzpusp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygslten3f0ydjxklpy86g3sh6yz5ar7f0jxjas9dkdvggwwnvq2q93xle0gsfxmm3qmulh5y5q5gunhj3r2mqn8glden5g7d6n8mzxze7cq4mqn5r",
			valid:          true,
			decodedInvoice: func() *Invoice {
				i, _ := NewInvoice(
					&chaincfg.MainNetParams,
					testPaymentHash,
					time.Unix(1496314658, 0),
					Amount(testMillisat2500uBTC),
					Description(testCupOfNonsense),
					Destination(testPubKey),
					Expiry(testExpiry60),
					PaymentAddr(specPaymentAddr))
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
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqssp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygsz7q6sc79jzdwtegpf58988jufqr7qy0n59me7pc7wk4pl8ym6k94kjzyxccs6cc9kk88djqeac3lprndgtn2whk25xzw4xenr95ldhsq5k7v5a",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					Features:        emptyFeatures,
					PaymentAddr:     fn.Some(specPaymentAddr),
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
			encodedInvoice: "lntb20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3x9et2e20v6pu37c5d9vax37wxq72un98sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs30kjz30tq32mcqcl0scp9dydfk647cfapf9xcnc2e8ea53j6ya5ne96gylvnrldlnq3x9x6vfqzqmpeuyepzsrgdwevtwd74xppnnxcphq9hqz",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.TestNet3Params,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrTestnet,
					Features:        emptyFeatures,
					PaymentAddr:     fn.Some(specPaymentAddr),
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
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3qjmp7lwpagxun9pygexvgpjdc4jdj85frzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs6jh3xwjhyldezxr7wx6myldtttk34k2q3jaqc4lyxcsfevjeh09kkv8d23pkdpeuf83wmftrwklazsftuwx8zgghn4jxafd3gq0k6vsqvh5ejq",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testRustyAddr,
					RouteHints:      [][]HopHint{testSingleHop},
					Features:        emptyFeatures,
					PaymentAddr:     fn.Some(specPaymentAddr),
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
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3qjmp7lwpagxun9pygexvgpjdc4jdj85fr9yq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqpqqqqq9qqqvpeuqafqxu92d8lr6fvg0r5gv0heeeqgcrqlnm6jhphu9y00rrhy4grqszsvpcgpy9qqqqqqgqqqqq7qqzqsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygsa492vv7x3elyzpkck3gyex3al2yywz0sm4clf3t4ayxz49005axh7qx3hp72wflvde27h8mp5rfv4ctvcqc8fy48r4mm69gpeucpadcq9vwr6c",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testRustyAddr,
					RouteHints:      [][]HopHint{testDoubleHop},
					Features:        emptyFeatures,
					PaymentAddr:     fn.Some(specPaymentAddr),
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
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfppj3a24vwu6r8ejrss3axul8rxldph2q7z9sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygslp9th3hnp54ppyeh3cef40gv34rm63xdc6cvgn2wjdmxj860453zjjmm0h55spgl78vwjfnd9x8cjfwd0kckgct5r00d7ufm8t3lc0sp5e70g5",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrMainnetP2SH,
					Features:        emptyFeatures,
					PaymentAddr:     fn.Some(specPaymentAddr),
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
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfppqw508d6qejxtdg4y5r3zarvary0c5xw7ksp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs6dc3xx6alc55ypd3fqh6m4u53ksg72dz0grj0l2rzkeap48xt3u9q2m9mwp5gfu60f84zqnnky0rp9etwqkd43ra83mjxxyqnl0r6kcpru52gx",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrMainnetP2WPKH,
					Features:        emptyFeatures,
					PaymentAddr:     fn.Some(specPaymentAddr),
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
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfp4qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qsp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygsl8ea2vka9sd0vue9qp5tzqzgl4sj982pcpzrrgzdh5hlhdf44dayxy9fudw25kz36t5hgm0q8w4xsavsz28mhtxk567qhmmy6x3eprsp0zwaz6",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrMainnetP2WSH,
					Features:        emptyFeatures,
					PaymentAddr:     fn.Some(specPaymentAddr),
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
			// On mainnet, with fallback (p2tr) address "bc1pptdvg0d
			// 2nj99568qn6ssdy4cygnwuxgw2ukmnwgwz7jpqjz2kszse2s3lm"
			// using the test vector payment from BOLT 11
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfp4pptdvg0d2nj99568qn6ssdy4cygnwuxgw2ukmnwgwz7jpqjz2kszssp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs9qrsgqcresgm59s2yvvuc67ep5wah53rv6a22uwx2u3hm70gx7r334g5qsxy30063452qv2nsfuj88khr8xxdhe79p0ukxqh8lhkagwalshwgqwalarg",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrMainnetP2TR,
					Features: lnwire.NewFeatureVector(
						lnwire.NewRawFeatureVector(
							8, 14,
						),
						lnwire.Features,
					),
					PaymentAddr: fn.Some(specPaymentAddr),
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
					Amount(testMillisat2500uBTC),
					Description(testCupOfCoffee),
					Destination(testPubKey),
					CLTVExpiry(144),
					PaymentAddr(specPaymentAddr),
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
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4js5fdqqqqq2qqqqqpgqyzqqqqqqqqqqqqyqqqqqqqqqqqvsqqqqlnxy0ffrlt2y2jgtzw89kgr3zg4dlwtlfycn3yuek8x5eucnuchqps82xf0m2u6sx5wnjw7xxgnxz5kf09quqsv5zvkgj7d5kpzttp4qz7q5qsyqcyq5pzq2feycsemrh7wvendc4kw3tsmkt25a36ev6kfehrv7ecfkrp5zs9q5zqxqspqtr4avek5quzjn427asptzews5wrczfhychr2sq6ue9phmn35tjqcrspqgpsgpgxquyqjzstpsxsu59zqqqqqpqqqqqqyqq2qqqqqqqqqqqqqqqqqqqqqqqqpgqqqqk8t6endgpc99824amqzk9japgu8synwf3wx4qp4ej2r0h8rghypsqsygpf8ynzr8vwleenxdhzke69wrwed2nk8t9n2e8xudnm8pxcvxs2q5qsyqcyq5y4rdlhtf84f8rgdj34275juwls2ftxtcfh035863q3p9k6s94hpxhdmzfn5gxpsazdznxs56j4vt3fdhe00g9v2l3szher50hp4xlggqkxf77f",
			valid:          true,
			decodedInvoice: func() *Invoice {
				return &Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat20mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
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
			encodedInvoice: "lnbc25m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5vdhkven9v5sxyetpdeessp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs9q4psqqqqqqqqqqqqqqqpqsqq40wa3khl49yue3zsgm26jrepqr2eghqlx86rttutve3ugd05em86nsefzh4pfurpd9ek9w2vp95zxqnfe2u7ckudyahsa52q66tgzcp6t2dyk",
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
			encodedInvoice: "lnbc25m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5vdhkven9v5sxyetpdeessp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs9q4psqqqqqqqqqqqqqqqpqsqq40wa3khl49yue3zsgm26jrepqr2eghqlx86rttutve3ugd05em86nsefzh4pfurpd9ek9w2vp95zxqnfe2u7ckudyahsa52q66tgzcp6t2dyk",
			valid:          false,
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
//
//nolint:ll
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
					DescriptionHash(testDescriptionHash),
					Description(testPleaseConsider),
					PaymentAddr(specPaymentAddr),
				)
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
					Description(testCupOfCoffee),
					PaymentAddr(specPaymentAddr),
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
					Amount(testMillisat24BTC),
					Description(testEmptyString),
					Destination(testPubKey),
					PaymentAddr(specPaymentAddr),
				)
			},
			valid:          true,
			encodedInvoice: "lnbc241pveeq09pp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdqqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygse2tu9s2lqeved59qef46rl2rsd5tkzdc9pjr6z3299aaaskgnk85zg96ng3xlknry8d9f9ssrqan6uj9dtm9mc00wcsam7awtkdgh6sqpf7nq7",
		},
		{
			// On mainnet, with fallback address 1RustyRX2oai4EYYDpQGWvEL62BBGqN9T with extra routing info to go via nodes 029e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255 then 039e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255
			newInvoice: func() (*Invoice, error) {
				return NewInvoice(&chaincfg.MainNetParams,
					testPaymentHash, time.Unix(1496314658, 0),
					Amount(testMillisat20mBTC),
					DescriptionHash(testDescriptionHash),
					FallbackAddr(testRustyAddr),
					RouteHint(testDoubleHop),
					PaymentAddr(specPaymentAddr),
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
					Amount(testMillisat24BTC),
					Description(testEmptyString),
					Destination(testPubKey),
					PaymentAddr(specPaymentAddr),
				)
			},
			valid:          true,
			encodedInvoice: "lnsb241pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdqqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66sp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygs6ypz232f2e8t2u957z8p82h0gkjpzcv4e3jaggs7vje4mqhtyvspl75kv9mkt3ye35rhh904m4mtz665jwkrmu2cdkjdfcz05s6ut5qqke0kry",
		},
		{
			// On regtest
			newInvoice: func() (*Invoice, error) {
				return NewInvoice(&chaincfg.RegressionNetParams,
					testPaymentHash, time.Unix(1496314658, 0),
					Amount(testMillisat24BTC),
					Description(testEmptyString),
					Destination(testPubKey),
					PaymentAddr(specPaymentAddr),
				)
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
					Amount(testMillisat20mBTC),
					Description(testCupOfCoffee),
					WithBlindedPaymentPath(blindedPath1),
					WithBlindedPaymentPath(blindedPath2),
				)
			},
			valid:          true,
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4js5fdqqqqq2qqqqqpgqyzqqqqqqqqqqqqyqqqqqqqqqqqvsqqqqlnxy0ffrlt2y2jgtzw89kgr3zg4dlwtlfycn3yuek8x5eucnuchqps82xf0m2u6sx5wnjw7xxgnxz5kf09quqsv5zvkgj7d5kpzttp4qz7q5qsyqcyq5pzq2feycsemrh7wvendc4kw3tsmkt25a36ev6kfehrv7ecfkrp5zs9q5zqxqspqtr4avek5quzjn427asptzews5wrczfhychr2sq6ue9phmn35tjqcrspqgpsgpgxquyqjzstpsxsu59zqqqqqpqqqqqqyqq2qqqqqqqqqqqqqqqqqqqqqqqqpgqqqqk8t6endgpc99824amqzk9japgu8synwf3wx4qp4ej2r0h8rghypsqsygpf8ynzr8vwleenxdhzke69wrwed2nk8t9n2e8xudnm8pxcvxs2q5qsyqcyq5y4rdlhtf84f8rgdj34275juwls2ftxtcfh035863q3p9k6s94hpxhdmzfn5gxpsazdznxs56j4vt3fdhe00g9v2l3szher50hp4xlggqkxf77f",
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
//
//nolint:ll
func TestMaxInvoiceLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		encodedInvoice string
		expectedError  error
	}{
		{
			// Valid since it is less than maxInvoiceLength.
			encodedInvoice: "lnbc25m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5vdhkven9v5sxyetpdeesr75q20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqpqqqqq9qqqvpfuqafqxu92d8lr6fvg0r5gv0heeeqgcrqlnm6jhphu9y00rrhy4gpqgpsgpgxquyqqqqqqyqqqqq5qqps98sr4yqms4f5lu0f93puw3p37l88yprqvr70022uxls53auvwuj4qypqxpq9qcrssqqqqqqsqqqqzsqqxq57qw5srwz4xnl3ayky836yx8muuusyvps0eaaftsm7zj8h33mj25qsyqcyq5rqwzqqqqqqzqqqqq2qqqczncp6jqdc2560785jcs78gscl0nnjq3sxpl8h49wr0c2g77x8wf2szqsrqszsvpcgqqqqqqgqqqqpgqqrq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqpqqqqq9qqqvpfuqafqxu92d8lr6fvg0r5gv0heeeqgcrqlnm6jhphu9y00rrhy4gpqgpsgpgxquyqqqqqqyqqqqq5qqps98sr4yqms4f5lu0f93puw3p37l88yprqvr70022uxls53auvwuj4qypqxpq9qcrssqqqqqqsqqqqzsqqxq57qw5srwz4xnl3ayky836yx8muuusyvps0eaaftsm7zj8h33mj25qsyqcyq5rqwzqqqqqqzqqqqq2qqqczncp6jqdc2560785jcs78gscl0nnjq3sxpl8h49wr0c2g77x8wf2szqsrqszsvpcgqqqqqqgqqqqpgqqrq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqpqqqqq9qqqvpfuqafqxu92d8lr6fvg0r5gv0heeeqgcrqlnm6jhphu9y00rrhy4gpqgpsgpgxquyqqqqqqyqqqqq5qqpssp5zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3zygshsu4qvmajk64zafgqprkqpk9tj5ljf9n8l4pkqpzunrs4sy7mpy8vsf2lk8532vdzz4sl0kdf07ccp7uz5mq7zxg5ylhvr45w25q9xsqea9kfg",
		},
		{
			// Invalid since it is greater than maxInvoiceLength.
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvrzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66fxq08u2ye04k6d2v2hgd8naeu0mfszlz2h6ze5mnufsc7y07s2z5v7vswj7jjcqumqchd646hnrx9pznxfj98565uc84zpc82x3nr8sqqn2zzu",
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
//
//nolint:ll
func TestInvoiceChecksumMalleability(t *testing.T) {
	privKeyHex := "a50f3bdf9b6c4b1fdd7c51a8bbf4b5855cf381f413545ed155c0282f4412b1b1"
	privKeyBytes, _ := hex.DecodeString(privKeyHex)
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
	opts := []func(*Invoice){
		Description("test"), PaymentAddr((specPaymentAddr)),
	}
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
