package trampoline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

//nolint:ll // Official vectors remain in the line grouping published by BOLT 4.
const (
	officialFailureEve = "" +
		"8d332db2f2192d54ccf1717318e231f5fa80853036e86f7a6d6ac60a88452efe" +
		"eef9d37d69bbb954bb5868d2c364a3eec1a5647b6f1a166e90f0682882679e81" +
		"21bc2eed0d750cfef58de6e455920dbb5def6244cb8b2b47bc3f19dc903d9f2c" +
		"578733aae4e943a1c7ce9893687848d40d2cac174905e99c1b65e4d637583b93" +
		"c231c593d9752fc6b0265f647cbf708efb2de632f8df4c8253ad56dd13552ff1" +
		"704ac7f1ed7c1c6450995a7e4c820f55bd1ae4330e7e822f18b2d5cbbe578d57" +
		"08fbec0474ec2a0501c6100cf644c634c166e7501c7d70723be5defe8b0fd200" +
		"d7ed64e2b09260867864890558351af9178c867a6cb59e83185ba4e92c64e19a" +
		"6d057af2856c7b9aeb66a9c30d6f672de9a556d4020aabb388af96e644fa34d8" +
		"bef34262"

	officialFailureDave = "" +
		"59048eb5ae82a97991db9e51d94efbeb91035627d3aa9608e7079509ec7dceb2" +
		"6bfa9346c7bb4aa33b42edb0d7d29f2e962afd03c5220a2597662bceb3ce0ef4" +
		"3090ddd89dd61b51a5c35bda4b19a74939b538b45680c25430fa3f024a8b1673" +
		"6179f3f583538ba80625aafc6e363a4a4f5055ad780dfe93e0086b26206db463" +
		"eeed12c77b4df58b3df3f8e977061722f0007329aecaf7f0853bd46ee3afbbc2" +
		"6969f723f61f02a4000b8a59d572d9adfc147b6b53e19a49289714ffb8b64bd5" +
		"d8f9a57854bd775c8cc488c108031cf4a447ef3f1e011b35d16050b6e91a4de6" +
		"3b85e6167ce602363df02d10cdc29ac52f59a788f16ec2fe65c6717346f054c0" +
		"e42b1ab87547d047277d901a7e081291a040bdebbc2fa578b7aa996261c239623" +
		"7255c54"

	officialDownstreamPeeled = "" +
		"af2704cc847cc1e1bd33242f6a123a5139015910d813c18f9c7cfda520519cab" +
		"aa2bfcdb3e43e30adefd8471e095949ab5d39e1b3b875f48f8162c9740fa837c" +
		"a72404a0eccaca2b521a46cb5d6cc0d1dcc8a15670efe05d8ba8b22f08b2315b" +
		"796dbd713b55b7ed639f5fbeff3b0dcb87cd36b44b56530fcb2c752a98497241" +
		"611ff09c03b6853a7eae890723899057bf3ab77d1638a287178c370305f54ade0" +
		"0af7e5c4fbb838aed9ac86c24fd70c5f716e5cdefdf8c3037bc85b467251b064" +
		"4a01f3c428744ed09263dd07b6671960775425f9025f290af6e0a53cc1697610" +
		"7ba13200b13703fa5f09c5b4edca19853e9528feef13b4e62633c97e4f417c9a" +
		"098c2ce7125c29c3196cd92b5240d31ecf2d22197cd26aa005c88598d0ab9fd5" +
		"ea0e77e"

	officialFailureCarol = "" +
		"d3ab773d52436535f3f85a1204afc975319b28e7ab953c6ee81d61aa0fc7788" +
		"5abea0f0cbb44a6729b4916f490994dcb180d6cb0fdfe2c50ee0f96ef4d24f82" +
		"fdb488817e7d099362351df2ded93992d175d1fbcf04ffc1c3f4d1e797c81c65" +
		"2f4f648a03950ed43002b75f23c734de26a33060320a436214f7c2fe71eb3e34" +
		"c0eabe9eada713b667e882a99667bf6327353447dbd0115c5e0ca74524fb6c4eb" +
		"2839c7db82804644306f534878c163b0e7f2398592ba3b7155d1db259447c56d" +
		"e0420bb81d9a0a33150e4316818cd66b5d74fb3327e769b83c865aea6e77493b" +
		"f0a977417b8a52328b5927aec7bd14764a7a304ef15b5349ceb606ada2babc45c" +
		"1f1b5ed0b9e3b2181a61611b93a6f40f360e508d3a42dcacf690bda01da0086f" +
		"603c382"

	officialFailureBob = "" +
		"f8941a320b8fde4ad7b9b920c69cbf334114737497d93059d77e591eaa78d633" +
		"4d3e2aeefcb0cc83402eaaf91d07d695cd895d9cad1018abdaf7d2a49d7657b1" +
		"612729db7f393f0bb62b25afaaaa326d72a9214666025385033f2ec4605dcf150" +
		"7467b5726d806da180ea224a7d8631cd31b0bdd08eead8bfe14fc8c7475e1776" +
		"8b1321b54dd4294aecc96da391efe0ca5bd267a45ee085c85a60cf9a9ac152fa" +
		"4795fff8700a3ea4f848817f5e6943e855ab2e86f6929c9e885d8b20c49b14d" +
		"2512c59ed21f10bd38691110b0d82c00d9fa48a20f10c7550358724c6e8e2b9" +
		"66e56a0aadf458695b273768062fa7c6e60eb72d4cdc67bf525c194e4a17fdca" +
		"a0e9d80480b586bf113f14eea530b6728a1c53fe5cee092e24a90f21f4b7640" +
		"15e7ed5e23"

	officialOuterPeeled = "" +
		"1ecb52bae42f744559016d17fb74c65e0a252ee89e4fbcd977a6f9b89dda92d5" +
		"169dca498cde6fd2b33108433ec4243e95f90be7286e4d8ac011fd572c1a5e2e" +
		"534a374ae2cdad3906e9eafe73be334d40b4796a77b5550742e759590ad4f2af" +
		"5d32d701a1bc87101b87eed4460bc288970fc905faa3b122fb2f93e5e430f874" +
		"4da9918e93db0ae9987abb5fbe5740127d0ce48e022e85a988ba84e5390bc3f3" +
		"e4026b841109489a21d0e7050815fd069d50ff1222a48708badffa2904de8786d" +
		"00200e59c2f0f6a7f38a1ac2d11cbf1eded8414de55ba516af6c43c339ad02d3" +
		"63b60f91258d2596f6cc329666e825cc830996b4e0726ab85e69bfc52e6e7f91" +
		"b8fbfb2a3e3b69cf20677138e6aaeb262a463ca9448b27eeacafbe52bee5bca0" +
		"796ef69"

	officialTrampolinePeeled = "" +
		"328d61ad604ddb3821aa8762714badd623d38637fe2c43f9c1c844989a2ac98a" +
		"000e400f0000000005f5e100000c350000f20000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"00000000"
)

// TestGenerateOfficialSharedSecrets verifies the session-key derivation used
// by the #836 error vector for every outer and trampoline circuit.
//
//nolint:ll // Official vector keys remain intact for direct comparison.
func TestGenerateOfficialSharedSecrets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		session  byte
		path     []string
		expected []string
	}{
		{
			name:    "alice outer",
			session: 0x04,
			path: []string{
				"0324653eac434488002cc06bbfb7f10fe18991e35f9fe4302dbea6d2353dc0ab1c",
				"027f31ebc5462c1fdce1b737ecff52d37d75dea43ce11c74d25aa297165faa2007",
			},
			expected: []string{
				"4bb46ded4fb0cae53a11db2e3ed5239ad083e2df6b9a98122268414ad525a868",
				"4546a66fb391bab8f165ffe444bf5d90192d1fad950434033183c64f08b6b091",
			},
		},
		{
			name:    "alice trampoline",
			session: 0x03,
			path: []string{
				"027f31ebc5462c1fdce1b737ecff52d37d75dea43ce11c74d25aa297165faa2007",
				"02edabbd16b41c8371b92ef2f04c1185b4f03b6dcd52ba9b78d9d7c89c8f221145",
			},
			expected: []string{
				"cf4ca0186dc2c2ea3f8e5b0999418151a6c61339ee09fef4c4804cd2c60fb359",
				"7bd32e41e242e1bb33e9bbfbff62b51249332a7c86d814dd4c8945b9c3bc9950",
			},
		},
		{
			name:    "carol downstream",
			session: 0x05,
			path: []string{
				"032c0b7cf95324a07d05398b240174dc0c2be444d96b159aa6c7f7b1e668680991",
				"02edabbd16b41c8371b92ef2f04c1185b4f03b6dcd52ba9b78d9d7c89c8f221145",
			},
			expected: []string{
				"ba32b1206434f6ded793257f34e497d4ba67f66e026a108860cc65dd9a6c8989",
				"e79cf33c343ca26b7048f029ad3c70f0dae0e3061eef960b5677696ffd297f54",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			session, _ := btcec.PrivKeyFromBytes(
				bytes.Repeat([]byte{test.session}, 32),
			)
			path := make([]*btcec.PublicKey, len(test.path))
			for i := range test.path {
				path[i] = mustPubKey(t, test.path[i])
			}

			secrets, err := GenerateSharedSecrets(session, path)
			require.NoError(t, err)
			require.Len(t, secrets, len(test.expected))
			for i := range secrets {
				require.Equal(
					t, test.expected[i],
					hex.EncodeToString(secrets[i][:]),
				)
			}
		})
	}
}

// TestOfficialNestedErrorVector executes the first #836 error vector from the
// final recipient through both relay layers and back to the origin.
func TestOfficialNestedErrorVector(t *testing.T) {
	t.Parallel()

	trampolineCarol := mustSecret(
		t, "cf4ca0186dc2c2ea3f8e5b0999418151"+
			"a6c61339ee09fef4c4804cd2c60fb359",
	)
	trampolineEve := mustSecret(
		t, "7bd32e41e242e1bb33e9bbfbff62b512"+
			"49332a7c86d814dd4c8945b9c3bc9950",
	)
	outerBob := mustSecret(
		t, "4bb46ded4fb0cae53a11db2e3ed5239a"+
			"d083e2df6b9a98122268414ad525a868",
	)
	outerCarol := mustSecret(
		t, "4546a66fb391bab8f165ffe444bf5d90"+
			"192d1fad950434033183c64f08b6b091",
	)
	outerDave := mustSecret(
		t, "ba32b1206434f6ded793257f34e497d4"+
			"ba67f66e026a108860cc65dd9a6c8989",
	)
	outerEve := mustSecret(
		t, "e79cf33c343ca26b7048f029ad3c70f0"+
			"dae0e3061eef960b5677696ffd297f54",
	)

	failure := lnwire.NewFailIncorrectDetails(100_000_000, 800_000)
	var failureMessage bytes.Buffer
	require.NoError(t, lnwire.EncodeFailure(&failureMessage, failure, 0))

	failureEve, err := EncryptNestedFailure(
		failureMessage.Bytes(), trampolineEve, outerEve,
	)
	require.NoError(t, err)
	require.Equal(t, mustHex(t, officialFailureEve), failureEve)

	failureDave, err := WrapOuterFailure(failureEve, outerDave)
	require.NoError(t, err)
	require.Equal(t, mustHex(t, officialFailureDave), failureDave)

	downstreamPeeled, authenticated, err := PeelErrorPath(
		failureDave, [][sha256.Size]byte{outerDave, outerEve},
	)
	require.NoError(t, err)
	require.Nil(t, authenticated)
	require.Equal(
		t, mustHex(t, officialDownstreamPeeled),
		downstreamPeeled,
	)

	failureCarol, err := WrapNestedFailure(
		downstreamPeeled, trampolineCarol, outerCarol,
	)
	require.NoError(t, err)
	require.Equal(t, mustHex(t, officialFailureCarol), failureCarol)

	failureBob, err := WrapOuterFailure(failureCarol, outerBob)
	require.NoError(t, err)
	require.Equal(t, mustHex(t, officialFailureBob), failureBob)

	outerPeeled, authenticated, err := PeelErrorPath(
		failureBob, [][sha256.Size]byte{outerBob, outerCarol},
	)
	require.NoError(t, err)
	require.Nil(t, authenticated)
	require.Equal(t, mustHex(t, officialOuterPeeled), outerPeeled)

	trampolinePeeled, authenticated, err := PeelErrorPath(
		outerPeeled,
		[][sha256.Size]byte{trampolineCarol, trampolineEve},
	)
	require.NoError(t, err)
	require.NotNil(t, authenticated)
	require.Equal(t, 1, authenticated.SenderIndex)
	require.Equal(
		t, mustHex(t, officialTrampolinePeeled),
		trampolinePeeled,
	)

	decrypted, err := DecryptNestedError(
		failureBob,
		[][sha256.Size]byte{outerBob, outerCarol},
		[][sha256.Size]byte{trampolineCarol, trampolineEve},
	)
	require.NoError(t, err)
	require.Equal(t, ErrorDomainTrampoline, decrypted.Domain)
	require.Equal(t, 1, decrypted.SenderIndex)

	decoded, err := lnwire.DecodeFailure(
		bytes.NewReader(decrypted.Message), 0,
	)
	require.NoError(t, err)
	require.Equal(t, failure, decoded)
}

// TestOfficialBlindedNestedErrorVector executes the second #836 error vector,
// where a malformed failure inside a blinded path is replaced by the
// introduction trampoline node.
//
//nolint:ll // Official failure packets remain in their published grouping.
func TestOfficialBlindedNestedErrorVector(t *testing.T) {
	t.Parallel()

	officialCarol := "" +
		"7b5d085fc545742096fabdcc1d37a1c62b88080f3c6ab8f4abd4f0fbd35639e3" +
		"6625db968c12816c072f374d79e5c642a6419aa13c12abb69537581ad0afe96f" +
		"41cc2789d91b25ce04d46fffbeeb6ef639f44f9a2188f21a64cb836167267b54" +
		"b5b3611ab77efd5f052f76295966c41d0b6d8a1c0fce72eece41e67156e649de" +
		"0c849d96be186f5d57790409f918b45cd052d3bd3972cd4f0edc990ac13d5d54" +
		"ecf2667888e989bf5a034b32a0693d01a0d9465007d5395343cc1a61129bcf0c" +
		"bb67eff959f73350853c039219a9e356999389c1604f7728e3d66a239ddf1cf1" +
		"2b12f6a6d6810065955c8128c43e85d8290f499f420f308cc1a8ae3acb4a1aa2" +
		"68dbfeb3b17c17ca50b8fadce5f38e1519881bd29ac2823043f35891de23e156" +
		"e40ac0b2"
	officialBob := "" +
		"c572fa6d2b2ab9dd05f361b15acc20327b7067758155ee3361b71ae52dbd2b81" +
		"9b54a39e339a4138998d07ed2d84967df06eeb9283b617694eaba71f934036b6" +
		"8a826f3d1a7b47e01a34828f34717959dd2c8aaff91cfcdbb46f7c6485279580" +
		"c3229bb4c73bcde667850620ec09443540a4edc9ab126bb17de6ccc85c9e0d04" +
		"979e65ecc9c84a4bb8b301c21159882699aa8d50ca420286b6977f467567d161" +
		"4f5172e6a591a26aa7cf2e29f929b7b31303b1c75036a91710fd5499f487829d" +
		"8d4912d1dd103a3d25b5cfeca0902bdc6c2cc58283fad5066192b52e3300c88d" +
		"741cda42108f886f61644a84b21d3a06b9a5c7b493096a8095d67fecc9b48a32" +
		"f989a31771a8d1cbd36cbe7d2e3f1e33b15aa68ca6eae03895094aa27d1549d8" +
		"ed8e31b2"

	trampolineCarol := mustSecret(
		t, "10d608eee2dcc6a00f1c08fa14659bd8"+
			"8531a608be3d68c8cf5988874a2e589b",
	)
	trampolineDave := mustSecret(
		t, "746a293d921da6db1a5564b204a5a9f"+
			"06c4ca0cf5540593319ffa511e6b411f0",
	)
	trampolineEve := mustSecret(
		t, "b98befc45213d74357027e292510d9ba"+
			"8000ba07d659c2af58854a818e3d436f",
	)
	outerBob := mustSecret(
		t, "21940511853dd5bd14ffc350149b04ba"+
			"88df752e31ebe5b2ad039bdc1ce160e4",
	)
	outerCarol := mustSecret(
		t, "21ea283167d589794e1bb1702ec62546"+
			"47a043cd1a535559f0ba2e2e280e5a9f",
	)

	var failureMessage bytes.Buffer
	failure := &lnwire.FailTemporaryTrampolineFailure{}
	require.NoError(t, lnwire.EncodeFailure(&failureMessage, failure, 0))

	failureCarol, err := EncryptNestedFailure(
		failureMessage.Bytes(), trampolineCarol, outerCarol,
	)
	require.NoError(t, err)
	require.Equal(t, mustHex(t, officialCarol), failureCarol)

	failureBob, err := WrapOuterFailure(failureCarol, outerBob)
	require.NoError(t, err)
	require.Equal(t, mustHex(t, officialBob), failureBob)

	decrypted, err := DecryptNestedError(
		failureBob,
		[][sha256.Size]byte{outerBob, outerCarol},
		[][sha256.Size]byte{
			trampolineCarol, trampolineDave, trampolineEve,
		},
	)
	require.NoError(t, err)
	require.Equal(t, ErrorDomainTrampoline, decrypted.Domain)
	require.Zero(t, decrypted.SenderIndex)

	decoded, err := lnwire.DecodeFailure(
		bytes.NewReader(decrypted.Message), 0,
	)
	require.NoError(t, err)
	require.Equal(t, failure, decoded)
}

// FuzzDecryptNestedError checks that malformed nested error packets cannot
// panic and that every authenticated result is a valid fixed-size failure.
func FuzzDecryptNestedError(f *testing.F) {
	outer := [][sha256.Size]byte{
		mustSecret(f, "4bb46ded4fb0cae53a11db2e3ed5239a"+
			"d083e2df6b9a98122268414ad525a868"),
		mustSecret(f, "4546a66fb391bab8f165ffe444bf5d90"+
			"192d1fad950434033183c64f08b6b091"),
	}
	inner := [][sha256.Size]byte{
		mustSecret(f, "cf4ca0186dc2c2ea3f8e5b0999418151"+
			"a6c61339ee09fef4c4804cd2c60fb359"),
		mustSecret(f, "7bd32e41e242e1bb33e9bbfbff62b512"+
			"49332a7c86d814dd4c8945b9c3bc9950"),
	}

	f.Add(mustHex(f, officialFailureBob))
	f.Add([]byte{})
	f.Add(make([]byte, FailurePacketSize))

	f.Fuzz(func(t *testing.T, data []byte) {
		decrypted, err := DecryptNestedError(data, outer, inner)
		if err != nil {
			return
		}

		require.Len(t, decrypted.Message, lnwire.FailureMessageLength+4)
		_, err = lnwire.DecodeFailure(
			bytes.NewReader(decrypted.Message), 0,
		)
		require.NoError(t, err)
	})
}

func mustPubKey(t testing.TB, value string) *btcec.PublicKey {
	t.Helper()

	key, err := btcec.ParsePubKey(mustHex(t, value))
	require.NoError(t, err)

	return key
}

func mustSecret(t testing.TB, value string) [sha256.Size]byte {
	t.Helper()

	var secret [sha256.Size]byte
	decoded := mustHex(t, value)
	require.Len(t, decoded, sha256.Size)
	copy(secret[:], decoded)

	return secret
}
