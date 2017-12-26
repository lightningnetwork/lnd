package zpay32_test

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil"
)

var (
	testPrivKeyBytes, _     = hex.DecodeString("e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2db734")
	testPrivKey, testPubKey = btcec.PrivKeyFromBytes(btcec.S256(), testPrivKeyBytes)

	testRoutingInfoPubkeyBytes, _  = hex.DecodeString("029e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255")
	testRoutingInfoPubkey, _       = btcec.ParsePubKey(testRoutingInfoPubkeyBytes, btcec.S256())
	testRoutingInfoPubkeyBytes2, _ = hex.DecodeString("039e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255")
	testRoutingInfoPubkey2, _      = btcec.ParsePubKey(testRoutingInfoPubkeyBytes2, btcec.S256())

	testMillisat24BTC    = lnwire.MilliSatoshi(2400000000000)
	testMillisat2500uBTC = lnwire.MilliSatoshi(250000000)
	testMillisat20mBTC   = lnwire.MilliSatoshi(2000000000)

	testExpiry60             = 60 * time.Second
	testEmptyString          = ""
	testCupOfCoffee          = "1 cup coffee"
	testPleaseConsider       = "Please consider supporting this project"
	testRustyAddr, _         = btcutil.DecodeAddress("1RustyRX2oai4EYYDpQGWvEL62BBGqN9T", &chaincfg.MainNetParams)
	testAddrTestnet, _       = btcutil.DecodeAddress("mk2QpYatsKicvFVuTAQLBryyccRXMUaGHP", &chaincfg.TestNet3Params)
	testAddrMainnetP2SH, _   = btcutil.DecodeAddress("3EktnHQD7RiAE6uzMj2ZifT9YgRrkSgzQX", &chaincfg.MainNetParams)
	testAddrMainnetP2WPKH, _ = btcutil.DecodeAddress("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", &chaincfg.MainNetParams)
	testAddrMainnetP2WSH, _  = btcutil.DecodeAddress("bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3", &chaincfg.MainNetParams)
	testPaymentHashSlice, _  = hex.DecodeString("0001020304050607080900010203040506070809000102030405060708090102")
	testDescriptionHashSlice = chainhash.HashB([]byte("One piece of chocolate cake, one icecream cone, one pickle, one slice of swiss cheese, one slice of salami, one lollypop, one piece of cherry pie, one sausage, one cupcake, and one slice of watermelon"))

	// Must be initialized in init().
	testPaymentHash     [32]byte
	testDescriptionHash [32]byte

	testMessageSigner = zpay32.MessageSigner{
		SignCompact: func(hash []byte) ([]byte, error) {
			sig, err := btcec.SignCompact(btcec.S256(),
				testPrivKey, hash, true)
			if err != nil {
				return nil, fmt.Errorf("can't sign the "+
					"message: %v", err)
			}
			return sig, nil
		},
	}
)

func init() {
	copy(testPaymentHash[:], testPaymentHashSlice[:])
	copy(testDescriptionHash[:], testDescriptionHashSlice[:])
}

// TestDecodeEncode tests that an encoded invoice gets decoded into the expected
// Invoice object, and that reencoding the decoded invoice gets us back to the
// original encoded string.
func TestDecodeEncode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		encodedInvoice string
		valid          bool
		decodedInvoice func() *zpay32.Invoice
		skipEncoding   bool
		beforeEncoding func(*zpay32.Invoice)
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
			encodedInvoice: "lnb1asdsaddnv4wudz", // hrp too short
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
			// no payment hash set
			encodedInvoice: "lnbc20m1pvjluezhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsjv38luh6p6s2xrv3mzvlmzaya43376h0twal5ax0k6p47498hp3hnaymzhsn424rxqjs0q7apn26yrhaxltq3vzwpqj9nc2r3kzwccsplnq470",
			valid:          false,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
				}
			},
		},
		{
			// Both Description and DescriptionHash set.
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqs03vghs8y0kuj4ulrzls8ln7fnm9dk7sjsnqmghql6hd6jut36clkqpyuq0s5m6fhureyz0szx2qjc8hkgf4xc2hpw8jpu26jfeyvf4cpga36gt",
			valid:          false,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					Description:     &testPleaseConsider,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
				}
			},
		},
		{
			// Neither Description nor DescriptionHash set.
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqn2rne0kagfl4e0xag0w6hqeg2dwgc54hrm9m0auw52dhwhwcu559qav309h598pyzn69wh2nqauneyyesnpmaax0g6acr8lh9559jmcquyq5a9",
			valid:          false,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat20mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					Destination: testPubKey,
				}
			},
		},
		{
			// Has a few unknown fields, should just be ignored.
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaqtq2v93xxer9vczq8v93xxeqv72xr42ca60022jqu6fu73n453tmnr0ukc0pl0t23w7eavtensjz0j2wcu7nkxhfdgp9y37welajh5kw34mq7m4xuay0a72cwec8qwgqt5vqht",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat20mBTC,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					Description: &testPleaseConsider,
					Destination: testPubKey,
				}
			},
			skipEncoding: true, // Skip encoding since we don't have the unknown fields to encode.
		},
		{
			// Ignore unknown witness version in fallback address.
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpppw508d6qejxtdg4y5r3zarvary0c5xw7k8txqv6x0a75xuzp0zsdzk5hq6tmfgweltvs6jk5nhtyd9uqksvr48zga9mw08667w8264gkspluu66jhtcmct36nx363km6cquhhv2cpc6q43r",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
				}
			},
			skipEncoding: true, // Skip encoding since we don't have the unknown fields to encode.
		},
		{
			// Ignore fields with unknown lengths.
			encodedInvoice: "lnbc241pveeq09pp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqpp3qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqshp38yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66np3q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfy8huflvs2zwkymx47cszugvzn5v64ahemzzlmm62rpn9l9rm05h35aceq00tkt296289wepws9jh4499wq2l0vk6xcxffd90dpuqchqqztyayq",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat24BTC,
					Timestamp:       time.Unix(1503429093, 0),
					PaymentHash:     &testPaymentHash,
					Destination:     testPubKey,
					DescriptionHash: &testDescriptionHash,
				}
			},
			skipEncoding: true, // Skip encoding since we don't have the unknown fields to encode.
		},
		{
			// Please make a donation of any amount using rhash 0001020304050607080900010203040506070809000102030405060708090102 to me @03e7156ae33b0a208d0744199163177e909e80176e55d97a2f221ede0f934dd9ad
			encodedInvoice: "lnbc1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kgetjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaq8rkx3yf5tcsyz3d73gafnh3cax9rn449d9p5uxz9ezhhypd0elx87sjle52x86fux2ypatgddc6k63n7erqz25le42c4u4ecky03ylcqca784w",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:         &chaincfg.MainNetParams,
					Timestamp:   time.Unix(1496314658, 0),
					PaymentHash: &testPaymentHash,
					Description: &testPleaseConsider,
					Destination: testPubKey,
				}
			},
			beforeEncoding: func(i *zpay32.Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// Same as above, pubkey set in 'n' field.
			encodedInvoice: "lnbc241pveeq09pp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdqqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66jd3m5klcwhq68vdsmx2rjgxeay5v0tkt2v5sjaky4eqahe4fx3k9sqavvce3capfuwv8rvjng57jrtfajn5dkpqv8yelsewtljwmmycq62k443",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:         &chaincfg.MainNetParams,
					MilliSat:    &testMillisat24BTC,
					Timestamp:   time.Unix(1503429093, 0),
					PaymentHash: &testPaymentHash,
					Destination: testPubKey,
					Description: &testEmptyString,
				}
			},
		},
		{
			// Please send $3 for a cup of coffee to the same peer, within 1 minute
			encodedInvoice: "lnbc2500u1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4jsxqzpuaztrnwngzn3kdzw5hydlzf03qdgm2hdq27cqv3agm2awhz5se903vruatfhq77w3ls4evs3ch9zw97j25emudupq63nyw24cg27h2rspfj9srp",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				i, _ := zpay32.NewInvoice(
					&chaincfg.MainNetParams,
					testPaymentHash,
					time.Unix(1496314658, 0),
					zpay32.Amount(testMillisat2500uBTC),
					zpay32.Description(testCupOfCoffee),
					zpay32.Destination(testPubKey),
					zpay32.Expiry(testExpiry60))
				return i
			},
			beforeEncoding: func(i *zpay32.Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// Now send $24 for an entire list of things (hashed)
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqscc6gd6ql3jrc5yzme8v4ntcewwz5cnw92tz0pc8qcuufvq7khhr8wpald05e92xw006sq94mg8v2ndf4sefvf9sygkshp5zfem29trqq2yxxz7",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
				}
			},
			beforeEncoding: func(i *zpay32.Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// The same, on testnet, with a fallback address mk2QpYatsKicvFVuTAQLBryyccRXMUaGHP
			encodedInvoice: "lntb20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3x9et2e20v6pu37c5d9vax37wxq72un98k6vcx9fz94w0qf237cm2rqv9pmn5lnexfvf5579slr4zq3u8kmczecytdx0xg9rwzngp7e6guwqpqlhssu04sucpnz4axcv2dstmknqq6jsk2l",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:             &chaincfg.TestNet3Params,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrTestnet,
				}
			},
			beforeEncoding: func(i *zpay32.Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, with fallback address 1RustyRX2oai4EYYDpQGWvEL62BBGqN9T with extra routing info to get to node 029e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3qjmp7lwpagxun9pygexvgpjdc4jdj85frzjq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqqqqqqq9qqqvncsk57n4v9ehw86wq8fzvjejhv9z3w3q5zh6qkql005x9xl240ch23jk79ujzvr4hsmmafyxghpqe79psktnjl668ntaf4ne7ucs5csqh5mnnk",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testRustyAddr,
					RoutingInfo: []zpay32.ExtraRoutingInfo{
						{
							PubKey:                    testRoutingInfoPubkey,
							ShortChanID:               0x0102030405060708,
							FeeBaseMsat:               0,
							FeeProportionalMillionths: 20,
							CltvExpDelta:              3,
						},
					},
				}
			},
			beforeEncoding: func(i *zpay32.Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, with fallback address 1RustyRX2oai4EYYDpQGWvEL62BBGqN9T with extra routing info to go via nodes 029e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255 then 039e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3qjmp7lwpagxun9pygexvgpjdc4jdj85fr9yq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqpqqqqq9qqqvpeuqafqxu92d8lr6fvg0r5gv0heeeqgcrqlnm6jhphu9y00rrhy4grqszsvpcgpy9qqqqqqgqqqqq7qqzqj9n4evl6mr5aj9f58zp6fyjzup6ywn3x6sk8akg5v4tgn2q8g4fhx05wf6juaxu9760yp46454gpg5mtzgerlzezqcqvjnhjh8z3g2qqdhhwkj",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testRustyAddr,
					RoutingInfo: []zpay32.ExtraRoutingInfo{
						{
							PubKey:                    testRoutingInfoPubkey,
							ShortChanID:               0x0102030405060708,
							FeeBaseMsat:               1,
							FeeProportionalMillionths: 20,
							CltvExpDelta:              3,
						},
						{
							PubKey:                    testRoutingInfoPubkey2,
							ShortChanID:               0x030405060708090a,
							FeeBaseMsat:               2,
							FeeProportionalMillionths: 30,
							CltvExpDelta:              4,
						},
					},
				}
			},
			beforeEncoding: func(i *zpay32.Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, with fallback (p2sh) address 3EktnHQD7RiAE6uzMj2ZifT9YgRrkSgzQX
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfppj3a24vwu6r8ejrss3axul8rxldph2q7z9kk822r8plup77n9yq5ep2dfpcydrjwzxs0la84v3tfw43t3vqhek7f05m6uf8lmfkjn7zv7enn76sq65d8u9lxav2pl6x3xnc2ww3lqpagnh0u",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrMainnetP2SH,
				}
			},
			beforeEncoding: func(i *zpay32.Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, with fallback (p2wpkh) address bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfppqw508d6qejxtdg4y5r3zarvary0c5xw7kknt6zz5vxa8yh8jrnlkl63dah48yh6eupakk87fjdcnwqfcyt7snnpuz7vp83txauq4c60sys3xyucesxjf46yqnpplj0saq36a554cp9wt865",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrMainnetP2WPKH,
				}
			},
			beforeEncoding: func(i *zpay32.Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// On mainnet, with fallback (p2wsh) address bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfp4qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qvnjha2auylmwrltv2pkp2t22uy8ura2xsdwhq5nm7s574xva47djmnj2xeycsu7u5v8929mvuux43j0cqhhf32wfyn2th0sv4t9x55sppz5we8",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				return &zpay32.Invoice{
					Net:             &chaincfg.MainNetParams,
					MilliSat:        &testMillisat20mBTC,
					Timestamp:       time.Unix(1496314658, 0),
					PaymentHash:     &testPaymentHash,
					DescriptionHash: &testDescriptionHash,
					Destination:     testPubKey,
					FallbackAddr:    testAddrMainnetP2WSH,
				}
			},
			beforeEncoding: func(i *zpay32.Invoice) {
				// Since this destination pubkey was recovered
				// from the signature, we must set it nil before
				// encoding to get back the same invoice string.
				i.Destination = nil
			},
		},
		{
			// Send 2500uBTC for a cup of coffee with a custom CLTV
			// expiry value.
			encodedInvoice: "lnbc2500u1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdq5xysxxatsyp3k7enxv4jscqzysnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66ysxkvnxhcvhz48sn72lp77h4fxcur27z0he48u5qvk3sxse9mr9jhkltt962s8arjnzk8rk59yj5nw4p495747gksj30gza0crhzwjcpgxzy00",
			valid:          true,
			decodedInvoice: func() *zpay32.Invoice {
				i, _ := zpay32.NewInvoice(
					&chaincfg.MainNetParams,
					testPaymentHash,
					time.Unix(1496314658, 0),
					zpay32.Amount(testMillisat2500uBTC),
					zpay32.Description(testCupOfCoffee),
					zpay32.Destination(testPubKey),
					zpay32.CLTVExpiry(144),
				)

				return i
			},
		},
	}

	for i, test := range tests {
		invoice, err := zpay32.Decode(test.encodedInvoice)
		if (err == nil) != test.valid {
			t.Errorf("Decoding test %d failed: %v", i, err)
			return
		}

		if test.valid {
			if err := compareInvoices(test.decodedInvoice(), invoice); err != nil {
				t.Errorf("Invoice decoding result %d not as expected: %v", i, err)
				return
			}
		}

		if test.skipEncoding {
			continue
		}

		var decodedInvoice *zpay32.Invoice
		if test.decodedInvoice != nil {
			decodedInvoice = test.decodedInvoice()
		}

		if test.beforeEncoding != nil {
			test.beforeEncoding(decodedInvoice)
		}

		if decodedInvoice != nil {
			reencoded, err := decodedInvoice.Encode(
				testMessageSigner,
			)
			if (err == nil) != test.valid {
				t.Errorf("Encoding test %d failed: %v", i, err)
				return
			}

			if test.valid && test.encodedInvoice != reencoded {
				t.Errorf("Encoding %d failed, expected %v, got %v",
					i, test.encodedInvoice, reencoded)
				return
			}
		}
	}
}

// TestNewInvoice tests that providing the optional arguments to the NewInvoice
// method creates an Invoice that encodes to the expected string.
func TestNewInvoice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		newInvoice     func() (*zpay32.Invoice, error)
		encodedInvoice string
		valid          bool
	}{
		{
			// Both Description and DescriptionHash set.
			newInvoice: func() (*zpay32.Invoice, error) {
				return zpay32.NewInvoice(&chaincfg.MainNetParams,
					testPaymentHash, time.Unix(1496314658, 0),
					zpay32.DescriptionHash(testDescriptionHash),
					zpay32.Description(testPleaseConsider))
			},
			valid: false, // Both Description and DescriptionHash set.
		},
		{
			// 'n' field set.
			newInvoice: func() (*zpay32.Invoice, error) {
				return zpay32.NewInvoice(&chaincfg.MainNetParams,
					testPaymentHash, time.Unix(1503429093, 0),
					zpay32.Amount(testMillisat24BTC),
					zpay32.Description(testEmptyString),
					zpay32.Destination(testPubKey))
			},
			valid:          true,
			encodedInvoice: "lnbc241pveeq09pp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdqqnp4q0n326hr8v9zprg8gsvezcch06gfaqqhde2aj730yg0durunfhv66jd3m5klcwhq68vdsmx2rjgxeay5v0tkt2v5sjaky4eqahe4fx3k9sqavvce3capfuwv8rvjng57jrtfajn5dkpqv8yelsewtljwmmycq62k443",
		},
		{
			// On mainnet, with fallback address 1RustyRX2oai4EYYDpQGWvEL62BBGqN9T with extra routing info to go via nodes 029e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255 then 039e03a901b85534ff1e92c43c74431f7ce72046060fcf7a95c37e148f78c77255
			newInvoice: func() (*zpay32.Invoice, error) {
				return zpay32.NewInvoice(&chaincfg.MainNetParams,
					testPaymentHash, time.Unix(1496314658, 0),
					zpay32.Amount(testMillisat20mBTC),
					zpay32.DescriptionHash(testDescriptionHash),
					zpay32.FallbackAddr(testRustyAddr),
					zpay32.RoutingInfo(
						[]zpay32.ExtraRoutingInfo{
							{
								PubKey:                    testRoutingInfoPubkey,
								ShortChanID:               0x0102030405060708,
								FeeBaseMsat:               1,
								FeeProportionalMillionths: 20,
								CltvExpDelta:              3,
							},
							{
								PubKey:                    testRoutingInfoPubkey2,
								ShortChanID:               0x030405060708090a,
								FeeBaseMsat:               2,
								FeeProportionalMillionths: 30,
								CltvExpDelta:              4,
							},
						},
					),
				)
			},
			valid:          true,
			encodedInvoice: "lnbc20m1pvjluezpp5qqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqhp58yjmdan79s6qqdhdzgynm4zwqd5d7xmw5fk98klysy043l2ahrqsfpp3qjmp7lwpagxun9pygexvgpjdc4jdj85fr9yq20q82gphp2nflc7jtzrcazrra7wwgzxqc8u7754cdlpfrmccae92qgzqvzq2ps8pqqqqqqpqqqqq9qqqvpeuqafqxu92d8lr6fvg0r5gv0heeeqgcrqlnm6jhphu9y00rrhy4grqszsvpcgpy9qqqqqqgqqqqq7qqzqj9n4evl6mr5aj9f58zp6fyjzup6ywn3x6sk8akg5v4tgn2q8g4fhx05wf6juaxu9760yp46454gpg5mtzgerlzezqcqvjnhjh8z3g2qqdhhwkj",
		},
	}

	for i, test := range tests {

		invoice, err := test.newInvoice()
		if err != nil && !test.valid {
			continue
		}
		encoded, err := invoice.Encode(testMessageSigner)
		if (err == nil) != test.valid {
			t.Errorf("NewInvoice test %d failed: %v", i, err)
			return
		}

		if test.valid && test.encodedInvoice != encoded {
			t.Errorf("Encoding %d failed, expected %v, got %v",
				i, test.encodedInvoice, encoded)
			return
		}
	}
}

func compareInvoices(expected, actual *zpay32.Invoice) error {
	if !reflect.DeepEqual(expected.Net, actual.Net) {
		return fmt.Errorf("expected net %v, got %v",
			expected.Net, actual.Net)
	}

	if !reflect.DeepEqual(expected.MilliSat, actual.MilliSat) {
		return fmt.Errorf("expected milli sat %d, got %d",
			*expected.MilliSat, *actual.MilliSat)
	}
	if expected.Timestamp != actual.Timestamp {
		return fmt.Errorf("expected Timestamp %d, got %d",
			expected.Timestamp, actual.Timestamp)
	}

	if !compareHashes(expected.PaymentHash, actual.PaymentHash) {
		return fmt.Errorf("expected payment hash %x, got %x",
			*expected.PaymentHash, *actual.PaymentHash)
	}

	if !reflect.DeepEqual(expected.Description, actual.Description) {
		return fmt.Errorf("expected description \"%s\", got \"%s\"",
			*expected.Description, *actual.Description)
	}

	if !comparePubkeys(expected.Destination, actual.Destination) {
		return fmt.Errorf("expected destination pubkey %x, got %x",
			expected.Destination, actual.Destination)
	}

	if !compareHashes(expected.DescriptionHash, actual.DescriptionHash) {
		return fmt.Errorf("expected description hash %x, got %x",
			*expected.DescriptionHash, *actual.DescriptionHash)
	}

	if expected.Expiry() != actual.Expiry() {
		return fmt.Errorf("expected expiry %d, got %d",
			expected.Expiry(), actual.Expiry())
	}

	if !reflect.DeepEqual(expected.FallbackAddr, actual.FallbackAddr) {
		return fmt.Errorf("expected FallbackAddr %v, got %v",
			expected.FallbackAddr, actual.FallbackAddr)
	}

	if len(expected.RoutingInfo) != len(actual.RoutingInfo) {
		return fmt.Errorf("expected len routingInfo %d, got %d",
			len(expected.RoutingInfo), len(actual.RoutingInfo))
	}

	for i := 0; i < len(expected.RoutingInfo); i++ {
		a := expected.RoutingInfo[i]
		b := actual.RoutingInfo[i]

		if !comparePubkeys(a.PubKey, b.PubKey) {
			return fmt.Errorf("expected routingInfo pubkey %x, "+
				"got %x", a.PubKey, b.PubKey)
		}

		if a.ShortChanID != b.ShortChanID {
			return fmt.Errorf("expected routingInfo shortChanID "+
				"%d, got %d", a.ShortChanID, b.ShortChanID)
		}

		if a.FeeBaseMsat != b.FeeBaseMsat {
			return fmt.Errorf("expected routingInfo feeBaseMsat %d, got %d",
				a.FeeBaseMsat, b.FeeBaseMsat)
		}

		if a.FeeProportionalMillionths != b.FeeProportionalMillionths {
			return fmt.Errorf("expected routingInfo feeProportionalMillionths %d, got %d",
				a.FeeProportionalMillionths, b.FeeProportionalMillionths)
		}

		if a.CltvExpDelta != b.CltvExpDelta {
			return fmt.Errorf("expected routingInfo cltvExpDelta "+
				"%d, got %d", a.CltvExpDelta, b.CltvExpDelta)
		}
	}
	return nil
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
