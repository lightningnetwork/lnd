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

	bip86Tweak = &MuSig2Tweaks{TaprootBIP0086Tweak: true}
)

func TestMuSig2CombineKeys(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                string
		keys                []*btcec.PublicKey
		tweak               *MuSig2Tweaks
		expectedErr         string
		expectedFinalKey    string
		expectedPreTweakKey string
	}{{
		name:  "v0.4.0 two dummy keys BIP86",
		keys:  []*btcec.PublicKey{dummyPubKey1, dummyPubKey2},
		tweak: bip86Tweak,
		expectedFinalKey: "03b54fb320a8fc3589e86a1559c6aaa774fbab4e4d" +
			"9fbf31e2fd836b661ac6a132",
		expectedPreTweakKey: "0279c76a15dcf6786058a571e4022b78633e1bf" +
			"8a7a4ca440bcbbeeaea772228a2",
	}, {
		name: "v0.4.0 three dummy keys BIP86",
		keys: []*btcec.PublicKey{
			dummyPubKey1, dummyPubKey2, dummyPubKey3,
		},
		tweak: bip86Tweak,
		expectedFinalKey: "03fa8195d584b195476f20e2fe978fd7312f4b08f2" +
			"777f080bcdfc9350603cd6e7",
		expectedPreTweakKey: "03e615b8aad4ed10544537bc48b1d6600e15773" +
			"476a675c6cbba6808f21b1988e5",
	}, {
		name: "v0.4.0 three test vector keys BIP86",
		keys: []*btcec.PublicKey{
			testVector040Key1, testVector040Key2, testVector040Key3,
		},
		tweak: bip86Tweak,
		expectedFinalKey: "025b257b4e785d61157ef5303051f45184bd5cb47b" +
			"c4b4069ed4dd4536459cb83b",
		expectedPreTweakKey: "02d70cd69a2647f7390973df48cbfa2ccc407b8" +
			"b2d60b08c5f1641185c7998a290",
	}, {
		name: "v0.4.0 three test vector keys BIP86 reverse order",
		keys: []*btcec.PublicKey{
			testVector040Key3, testVector040Key2, testVector040Key1,
		},
		tweak: bip86Tweak,
		expectedFinalKey: "025b257b4e785d61157ef5303051f45184bd5cb47b" +
			"c4b4069ed4dd4536459cb83b",
		expectedPreTweakKey: "02d70cd69a2647f7390973df48cbfa2ccc407b8" +
			"b2d60b08c5f1641185c7998a290",
	}}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(tt *testing.T) {
			tt.Parallel()

			res, err := MuSig2CombineKeys(tc.keys, tc.tweak)

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
