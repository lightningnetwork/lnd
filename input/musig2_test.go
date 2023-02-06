package input

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/stretchr/testify/require"
)

var (
	hexDecode = func(keyStr string) []byte {
		keyBytes, _ := hex.DecodeString(keyStr)
		return keyBytes
	}
	dummyPubKey1, _ = btcec.ParsePubKey(hexDecode(
		"02ec95e4e8ad994861b95fc5986eedaac24739e5ea3d0634db4c8ccd44cd" +
			"a126ea",
	))
	dummyPubKey2, _ = btcec.ParsePubKey(hexDecode(
		"0356167ba3e54ac542e86e906d4186aba9ca0b9df45001c62b753d33fe06" +
			"f5b4e8",
	))
	dummyPubKey3, _ = btcec.ParsePubKey(hexDecode(
		"02a9b0e1777e35d4620061a9fb0e614bf0254a50dea4f872babf6d44bf4d" +
			"8ee7c6",
	))

	testVector040Key1, _ = schnorr.ParsePubKey(hexDecode(
		"F9308A019258C31049344F85F89D5229B531C845836F99B08601F113BCE0" +
			"36F9",
	))
	testVector040Key2, _ = schnorr.ParsePubKey(hexDecode(
		"DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502B" +
			"A659",
	))
	testVector040Key3, _ = schnorr.ParsePubKey(hexDecode(
		"3590A94E768F8E1815C2F24B4D80A8E3149316C3518CE7B7AD338368D038" +
			"CA66",
	))

	testVector100Key1, _ = btcec.ParsePubKey(hexDecode(
		"02F9308A019258C31049344F85F89D5229B531C845836F99B08601F113BC" +
			"E036F9",
	))
	testVector100Key2, _ = btcec.ParsePubKey(hexDecode(
		"03DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B50" +
			"2BA659",
	))
	testVector100Key3, _ = btcec.ParsePubKey(hexDecode(
		"023590A94E768F8E1815C2F24B4D80A8E3149316C3518CE7B7AD338368D0" +
			"38CA66",
	))

	bip86Tweak = &MuSig2Tweaks{TaprootBIP0086Tweak: true}
)

func TestMuSig2CombineKeys(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                string
		version             MuSig2Version
		keys                []*btcec.PublicKey
		sortKeys            bool
		tweak               *MuSig2Tweaks
		expectedErr         string
		expectedFinalKey    string
		expectedPreTweakKey string
	}{{
		name:        "invalid version",
		version:     7,
		expectedErr: "unknown MuSig2 version: <7>",
	}, {
		name:     "v0.4.0 two dummy keys BIP86",
		version:  MuSig2Version040,
		keys:     []*btcec.PublicKey{dummyPubKey1, dummyPubKey2},
		sortKeys: true, tweak: bip86Tweak,
		expectedFinalKey: "03b54fb320a8fc3589e86a1559c6aaa774fbab4e4d" +
			"9fbf31e2fd836b661ac6a132",
		expectedPreTweakKey: "0279c76a15dcf6786058a571e4022b78633e1bf" +
			"8a7a4ca440bcbbeeaea772228a2",
	}, {
		name:    "v0.4.0 three dummy keys BIP86",
		version: MuSig2Version040,
		keys: []*btcec.PublicKey{
			dummyPubKey1, dummyPubKey2, dummyPubKey3,
		},
		sortKeys: true,
		tweak:    bip86Tweak,
		expectedFinalKey: "03fa8195d584b195476f20e2fe978fd7312f4b08f2" +
			"777f080bcdfc9350603cd6e7",
		expectedPreTweakKey: "03e615b8aad4ed10544537bc48b1d6600e15773" +
			"476a675c6cbba6808f21b1988e5",
	}, {
		name:    "v0.4.0 three test vector keys BIP86",
		version: MuSig2Version040,
		keys: []*btcec.PublicKey{
			testVector040Key1, testVector040Key2, testVector040Key3,
		},
		tweak:    bip86Tweak,
		sortKeys: true,
		expectedFinalKey: "025b257b4e785d61157ef5303051f45184bd5cb47b" +
			"c4b4069ed4dd4536459cb83b",
		expectedPreTweakKey: "02d70cd69a2647f7390973df48cbfa2ccc407b8" +
			"b2d60b08c5f1641185c7998a290",
	}, {
		name: "v0.4.0 three test vector keys BIP86 reverse order",
		keys: []*btcec.PublicKey{
			testVector040Key3, testVector040Key2, testVector040Key1,
		},
		sortKeys: true,
		tweak:    bip86Tweak,
		expectedFinalKey: "025b257b4e785d61157ef5303051f45184bd5cb47b" +
			"c4b4069ed4dd4536459cb83b",
		expectedPreTweakKey: "02d70cd69a2647f7390973df48cbfa2ccc407b8" +
			"b2d60b08c5f1641185c7998a290",
	}, {
		name: "v0.4.0 three test vector keys BIP86 no sort",
		keys: []*btcec.PublicKey{
			testVector040Key1, testVector040Key2, testVector040Key3,
		},
		sortKeys: false,
		tweak:    bip86Tweak,
		expectedFinalKey: "0223e0c640e96000e8e92699ec3802e5c39edf47db" +
			"dfdb788b3735b76a55538179",
		expectedPreTweakKey: "03e5830140512195d74c8307e39637cbe5fb730" +
			"ebeab80ec514cf88a877ceeee0b",
	}, {
		name:    "v1.0.0rc2 three dummy keys BIP86",
		version: MuSig2Version100RC2,
		keys: []*btcec.PublicKey{
			dummyPubKey1, dummyPubKey2, dummyPubKey3,
		},
		sortKeys: true,
		tweak:    bip86Tweak,
		expectedFinalKey: "029d11a433e446276c88ee099cecfda1e4234dc486" +
			"49e2b07f9b159176731eb68a",
		expectedPreTweakKey: "03f0bacfef76086c519b6db216a252f1435336f" +
			"d23fc86cfb51f67056439c256d4",
	}, {
		name:    "v1.0.0rc2 three test vector keys BIP86",
		version: MuSig2Version100RC2,
		keys: []*btcec.PublicKey{
			testVector100Key1, testVector100Key2, testVector100Key3,
		},
		sortKeys: false,
		tweak:    bip86Tweak,
		expectedFinalKey: "03f79d14149ecd4bb74921865906a8e4f1333439a9" +
			"1b96610d72caa7495dcf2376",
		expectedPreTweakKey: "0290539eede565f5d054f32cc0c220126889ed1" +
			"e5d193baf15aef344fe59d4610c",
	}, {
		name:    "v1.0.0rc2 three test vector keys BIP86 sorted",
		version: MuSig2Version100RC2,
		keys: []*btcec.PublicKey{
			testVector100Key1, testVector100Key2, testVector100Key3,
		},
		sortKeys: true,
		tweak:    bip86Tweak,
		expectedFinalKey: "0379e6c3e628c9bfbce91de6b7fb28e2aec7713d37" +
			"7cf260ab599dcbc40e542312",
		expectedPreTweakKey: "03789d937bade6673538f3e28d8368dda4d0512" +
			"f94da44cf477a505716d26a1575",
	}, {
		name:    "v1.0.0rc2 three test vector keys BIP86 reverse order",
		version: MuSig2Version100RC2,
		keys: []*btcec.PublicKey{
			testVector100Key3, testVector100Key2, testVector100Key1,
		},
		sortKeys: false,
		tweak:    bip86Tweak,
		expectedFinalKey: "03d61d333ab8c53c330290c144f406ce0c0dc3564b" +
			"8e3dee6d1daa6288609bfc75",
		expectedPreTweakKey: "036204de8b083426dc6eaf9502d27024d53fc82" +
			"6bf7d2012148a0575435df54b2b",
	}}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(tt *testing.T) {
			tt.Parallel()

			res, err := MuSig2CombineKeys(
				tc.version, tc.keys, tc.sortKeys, tc.tweak,
			)

			if tc.expectedErr != "" {
				require.ErrorContains(tt, err, tc.expectedErr)
				return
			}

			require.NoError(tt, err)

			finalKey := res.FinalKey.SerializeCompressed()
			preTweakKey := res.PreTweakedKey.SerializeCompressed()
			require.Equal(
				tt, tc.expectedFinalKey,
				hex.EncodeToString(finalKey),
			)
			require.Equal(
				tt, tc.expectedPreTweakKey,
				hex.EncodeToString(preTweakKey),
			)
		})
	}
}
