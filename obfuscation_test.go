package sphinx

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec"
)

// TestOnionFailure checks the ability of sender of payment to decode the
// obfuscated onion error.
func TestOnionFailure(t *testing.T) {
	// Create numHops random sphinx paymentPath.
	paymentPath := make([]*btcec.PublicKey, 5)
	for i := 0; i < len(paymentPath); i++ {
		privKey, err := btcec.NewPrivateKey(btcec.S256())
		if err != nil {
			t.Fatalf("unable to generate random key for sphinx node: %v", err)
		}
		paymentPath[i] = privKey.PubKey()
	}
	sessionKey, _ := btcec.PrivKeyFromBytes(btcec.S256(),
		bytes.Repeat([]byte{'A'}, 32))

	// Reduce the error path on one node, in order to check that we are
	// able to receive the error not only from last hop.
	errorPath := paymentPath[:len(paymentPath)-1]

	failureData := []byte("some kek-error data")
	sharedSecrets := generateSharedSecrets(paymentPath, sessionKey)

	// Emulate creation of the obfuscator on node where error have occurred.
	obfuscator := &OnionErrorEncrypter{
		sharedSecret: sharedSecrets[len(errorPath)-1],
	}

	// Emulate the situation when last hop creates the onion failure
	// message and send it back.
	obfuscatedData := obfuscator.EncryptError(true, failureData)

	// Emulate that failure message is backward obfuscated on every hop.
	for i := len(errorPath) - 2; i >= 0; i-- {
		// Emulate creation of the obfuscator on forwarding node which
		// propagates the onion failure.
		obfuscator = &OnionErrorEncrypter{
			sharedSecret: sharedSecrets[i],
		}
		obfuscatedData = obfuscator.EncryptError(false, obfuscatedData)
	}

	// Emulate creation of the deobfuscator on the receiving onion error side.
	deobfuscator := NewOnionErrorDecrypter(&Circuit{
		SessionKey:  sessionKey,
		PaymentPath: paymentPath,
	})

	// Emulate that sender node receive the failure message and trying to
	// unwrap it, by applying obfuscation and checking the hmac.
	pubKey, deobfuscatedData, err := deobfuscator.DecryptError(obfuscatedData)
	if err != nil {
		t.Fatalf("unable to de-obfuscate the onion failure: %v", err)
	}

	// We should understand the node from which error have been received.
	if !bytes.Equal(pubKey.SerializeCompressed(),
		errorPath[len(errorPath)-1].SerializeCompressed()) {
		t.Fatalf("unable to properly conclude from which node in " +
			"the path we received an error")
	}

	// Check that message have been properly de-obfuscated.
	if !bytes.Equal(deobfuscatedData, failureData) {
		t.Fatalf("data not equals, expected: \"%v\", real: \"%v\"",
			string(failureData), string(deobfuscatedData))
	}
}

// onionErrorData is a specification onion error obfuscation data which is
// produces by another lightning network node.
var onionErrorData = []struct {
	sharedSecret   string
	stream         string
	ammagKey       string
	obfuscatedData string
}{
	{
		sharedSecret: "b5756b9b542727dbafc6765a49488b023a725d631af688" +
			"fc031217e90770c328",
		ammagKey: "2f36bb8822e1f0d04c27b7d8bb7d7dd586e032a3218b8d414a" +
			"fbba6f169a4d68",
		stream:         "e9c975b07c9a374ba64fd9be3aae955e917d34d1fa33f2e90f53bbf4394713c6a8c9b16ab5f12fd45edd73c1b0c8b33002df376801ff58aaa94000bf8a86f92620f343baef38a580102395ae3abf9128d1047a0736ff9b83d456740ebbb4aeb3aa9737f18fb4afb4aa074fb26c4d702f42968888550a3bded8c05247e045b866baef0499f079fdaeef6538f31d44deafffdfd3afa2fb4ca9082b8f1c465371a9894dd8c243fb4847e004f5256b3e90e2edde4c9fb3082ddfe4d1e734cacd96ef0706bf63c9984e22dc98851bcccd1c3494351feb458c9c6af41c0044bea3c47552b1d992ae542b17a2d0bba1a096c78d169034ecb55b6e3a7263c26017f033031228833c1daefc0dedb8cf7c3e37c9c37ebfe42f3225c326e8bcfd338804c145b16e34e4",
		obfuscatedData: "a5e6bd0c74cb347f10cce367f949098f2457d14c046fd8a22cb96efb30b0fdcda8cb9168b50f2fd45edd73c1b0c8b33002df376801ff58aaa94000bf8a86f92620f343baef38a580102395ae3abf9128d1047a0736ff9b83d456740ebbb4aeb3aa9737f18fb4afb4aa074fb26c4d702f42968888550a3bded8c05247e045b866baef0499f079fdaeef6538f31d44deafffdfd3afa2fb4ca9082b8f1c465371a9894dd8c243fb4847e004f5256b3e90e2edde4c9fb3082ddfe4d1e734cacd96ef0706bf63c9984e22dc98851bcccd1c3494351feb458c9c6af41c0044bea3c47552b1d992ae542b17a2d0bba1a096c78d169034ecb55b6e3a7263c26017f033031228833c1daefc0dedb8cf7c3e37c9c37ebfe42f3225c326e8bcfd338804c145b16e34e4",
	},
	{
		sharedSecret: "21e13c2d7cfe7e18836df50872466117a295783ab8aab0e" +
			"7ecc8c725503ad02d",
		ammagKey: "cd9ac0e09064f039fa43a31dea05f5fe5f6443d40a98be4071" +
			"af4a9d704be5ad",
		stream:         "617ca1e4624bc3f04fece3aa5a2b615110f421ec62408d16c48ea6c1b7c33fe7084a2bd9d4652fc5068e5052bf6d0acae2176018a3d8c75f37842712913900263cff92f39f3c18aa1f4b20a93e70fc429af7b2b1967ca81a761d40582daf0eb49cef66e3d6fbca0218d3022d32e994b41c884a27c28685ef1eb14603ea80a204b2f2f474b6ad5e71c6389843e3611ebeafc62390b717ca53b3670a33c517ef28a659c251d648bf4c966a4ef187113ec9848bf110816061ca4f2f68e76ceb88bd6208376460b916fb2ddeb77a65e8f88b2e71a2cbf4ea4958041d71c17d05680c051c3676fb0dc8108e5d78fb1e2c44d79a202e9d14071d536371ad47c39a05159e8d6c41d17a1e858faaaf572623aa23a38ffc73a4114cb1ab1cd7f906c6bd4e21b29694",
		obfuscatedData: "c49a1ce81680f78f5f2000cda36268de34a3f0a0662f55b4e837c83a8773c22aa081bab1616a0011585323930fa5b9fae0c85770a2279ff59ec427ad1bbff9001c0cd1497004bd2a0f68b50704cf6d6a4bf3c8b6a0833399a24b3456961ba00736785112594f65b6b2d44d9f5ea4e49b5e1ec2af978cbe31c67114440ac51a62081df0ed46d4a3df295da0b0fe25c0115019f03f15ec86fabb4c852f83449e812f141a9395b3f70b766ebbd4ec2fae2b6955bd8f32684c15abfe8fd3a6261e52650e8807a92158d9f1463261a925e4bfba44bd20b166d532f0017185c3a6ac7957adefe45559e3072c8dc35abeba835a8cb01a71a15c736911126f27d46a36168ca5ef7dccd4e2886212602b181463e0dd30185c96348f9743a02aca8ec27c0b90dca270",
	},
	{
		sharedSecret: "3a6b412548762f0dbccce5c7ae7bb8147d1caf9b5471c3" +
			"4120b30bc9c04891cc",
		ammagKey: "1bf08df8628d452141d56adfd1b25c1530d7921c23cecfc749" +
			"ac03a9b694b0d3",
		stream:         "6149f48b5a7e8f3d6f5d870b7a698e204cf64452aab4484ff1dee671fe63fd4b5f1b78ee2047dfa61e3d576b149bedaf83058f85f06a3172a3223ad6c4732d96b32955da7d2feb4140e58d86fc0f2eb5d9d1878e6f8a7f65ab9212030e8e915573ebbd7f35e1a430890be7e67c3fb4bbf2def662fa625421e7b411c29ebe81ec67b77355596b05cc155755664e59c16e21410aabe53e80404a615f44ebb31b365ca77a6e91241667b26c6cad24fb2324cf64e8b9dd6e2ce65f1f098cfd1ef41ba2d4c7def0ff165a0e7c84e7597c40e3dffe97d417c144545a0e38ee33ebaae12cc0c14650e453d46bfc48c0514f354773435ee89b7b2810606eb73262c77a1d67f3633705178d79a1078c3a01b5fadc9651feb63603d19decd3a00c1f69af2dab259593",
		obfuscatedData: "a5d3e8634cfe78b2307d87c6d90be6fe7855b4f2cc9b1dfb19e92e4b79103f61ff9ac25f412ddfb7466e74f81b3e545563cdd8f5524dae873de61d7bdfccd496af2584930d2b566b4f8d3881f8c043df92224f38cf094cfc09d92655989531524593ec6d6caec1863bdfaa79229b5020acc034cd6deeea1021c50586947b9b8e6faa83b81fbfa6133c0af5d6b07c017f7158fa94f0d206baf12dda6b68f785b773b360fd0497e16cc402d779c8d48d0fa6315536ef0660f3f4e1865f5b38ea49c7da4fd959de4e83ff3ab686f059a45c65ba2af4a6a79166aa0f496bf04d06987b6d2ea205bdb0d347718b9aeff5b61dfff344993a275b79717cd815b6ad4c0beb568c4ac9c36ff1c315ec1119a1993c4b61e6eaa0375e0aaf738ac691abd3263bf937e3",
	},
	{
		sharedSecret: "a6519e98832a0b179f62123b3567c106db99ee37bef036" +
			"e783263602f3488fae",
		ammagKey: "59ee5867c5c151daa31e36ee42530f429c433836286e63744f" +
			"2020b980302564",
		stream:         "0f10c86f05968dd91188b998ee45dcddfbf89fe9a99aa6375c42ed5520a257e048456fe417c15219ce39d921555956ae2ff795177c63c819233f3bcb9b8b28e5ac6e33a3f9b87ca62dff43f4cc4a2755830a3b7e98c326b278e2bd31f4a9973ee99121c62873f5bfb2d159d3d48c5851e3b341f9f6634f51939188c3b9ff45feeb11160bb39ce3332168b8e744a92107db575ace7866e4b8f390f1edc4acd726ed106555900a0832575c3a7ad11bb1fe388ff32b99bcf2a0d0767a83cf293a220a983ad014d404bfa20022d8b369fe06f7ecc9c74751dcda0ff39d8bca74bf9956745ba4e5d299e0da8f68a9f660040beac03e795a046640cf8271307a8b64780b0588422f5a60ed7e36d60417562938b400802dac5f87f267204b6d5bcfd8a05b221ec2",
		obfuscatedData: "aac3200c4968f56b21f53e5e374e3a2383ad2b1b6501bbcc45abc31e59b26881b7dfadbb56ec8dae8857add94e6702fb4c3a4de22e2e669e1ed926b04447fc73034bb730f4932acd62727b75348a648a1128744657ca6a4e713b9b646c3ca66cac02cdab44dd3439890ef3aaf61708714f7375349b8da541b2548d452d84de7084bb95b3ac2345201d624d31f4d52078aa0fa05a88b4e20202bd2b86ac5b52919ea305a8949de95e935eed0319cf3cf19ebea61d76ba92532497fcdc9411d06bcd4275094d0a4a3c5d3a945e43305a5a9256e333e1f64dbca5fcd4e03a39b9012d197506e06f29339dfee3331995b21615337ae060233d39befea925cc262873e0530408e6990f1cbd233a150ef7b004ff6166c70c68d9f8c853c1abca640b8660db2921",
	},
	{
		sharedSecret: "53eb63ea8a3fec3b3cd433b85cd62a4b145e1dda09391b" +
			"348c4e1cd36a03ea66",
		ammagKey: "3761ba4d3e726d8abb16cba5950ee976b84937b61b7ad09e74" +
			"1724d7dee12eb5",
		stream:         "3699fd352a948a05f604763c0bca2968d5eaca2b0118602e52e59121f050936c8dd90c24df7dc8cf8f1665e39a6c75e9e2c0900ea245c9ed3b0008148e0ae18bbfaea0c711d67eade980c6f5452e91a06b070bbde68b5494a92575c114660fb53cf04bf686e67ffa4a0f5ae41a59a39a8515cb686db553d25e71e7a97cc2febcac55df2711b6209c502b2f8827b13d3ad2f491c45a0cafe7b4d8d8810e805dee25d676ce92e0619b9c206f922132d806138713a8f69589c18c3fdc5acee41c1234b17ecab96b8c56a46787bba2c062468a13919afc18513835b472a79b2c35f9a91f38eb3b9e998b1000cc4a0dbd62ac1a5cc8102e373526d7e8f3c3a1b4bfb2f8a3947fe350cb89f73aa1bb054edfa9895c0fc971c2b5056dc8665902b51fced6dff80c",
		obfuscatedData: "9c5add3963fc7f6ed7f148623c84134b5647e1306419dbe2174e523fa9e2fbed3a06a19f899145610741c83ad40b7712aefaddec8c6baf7325d92ea4ca4d1df8bce517f7e54554608bf2bd8071a4f52a7a2f7ffbb1413edad81eeea5785aa9d990f2865dc23b4bc3c301a94eec4eabebca66be5cf638f693ec256aec514620cc28ee4a94bd9565bc4d4962b9d3641d4278fb319ed2b84de5b665f307a2db0f7fbb757366067d88c50f7e829138fde4f78d39b5b5802f1b92a8a820865af5cc79f9f30bc3f461c66af95d13e5e1f0381c184572a91dee1c849048a647a1158cf884064deddbf1b0b88dfe2f791428d0ba0f6fb2f04e14081f69165ae66d9297c118f0907705c9c4954a199bae0bb96fad763d690e7daa6cfda59ba7f2c8d11448b604d12d",
	},
}

func getSpecPubKeys() ([]*btcec.PublicKey, error) {
	specPubKeys := []string{
		"02eec7245d6b7d2ccb30380bfbe2a3648cd7a942653f5aa340edcea1f283686619",
		"0324653eac434488002cc06bbfb7f10fe18991e35f9fe4302dbea6d2353dc0ab1c",
		"027f31ebc5462c1fdce1b737ecff52d37d75dea43ce11c74d25aa297165faa2007",
		"032c0b7cf95324a07d05398b240174dc0c2be444d96b159aa6c7f7b1e668680991",
		"02edabbd16b41c8371b92ef2f04c1185b4f03b6dcd52ba9b78d9d7c89c8f221145",
	}

	keys := make([]*btcec.PublicKey, len(specPubKeys))
	for i, sKey := range specPubKeys {
		bKey, err := hex.DecodeString(sKey)
		if err != nil {
			return nil, err
		}

		key, err := btcec.ParsePubKey(bKey, btcec.S256())
		if err != nil {
			return nil, err
		}
		keys[i] = key
	}
	return keys, nil
}

func getSpecSessionKey() (*btcec.PrivateKey, error) {
	bKey, err := hex.DecodeString("4141414141414141414141414141414141414" +
		"141414141414141414141414141")
	if err != nil {
		return nil, err
	}

	privKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), bKey)
	return privKey, nil
}

func getSpecOnionErrorData() ([]byte, error) {
	sData := "0002200200fe0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
	return hex.DecodeString(sData)
}

// TestOnionFailureSpecVector checks that onion error corresponds to the
// specification.
func TestOnionFailureSpecVector(t *testing.T) {
	failureData, err := getSpecOnionErrorData()
	if err != nil {
		t.Fatalf("unable to get specification onion failure "+
			"data: %v", err)
	}

	paymentPath, err := getSpecPubKeys()
	if err != nil {
		t.Fatalf("unable to get specification public keys: %v", err)
	}

	sessionKey, err := getSpecSessionKey()
	if err != nil {
		t.Fatalf("unable to get specification session key: %v", err)
	}

	var obfuscatedData []byte
	sharedSecrets := generateSharedSecrets(paymentPath, sessionKey)
	for i, test := range onionErrorData {

		// Decode the shared secret and check that it matchs with
		// specification.
		expectedSharedSecret, err := hex.DecodeString(test.sharedSecret)
		if err != nil {
			t.Fatalf("unable to decode spec shared secret: %v",
				err)
		}
		obfuscator := &OnionErrorEncrypter{
			sharedSecret: sharedSecrets[len(sharedSecrets)-1-i],
		}

		var b bytes.Buffer
		if err := obfuscator.Encode(&b); err != nil {
			t.Fatalf("unable to encode obfuscator: %v", err)
		}

		obfuscator2 := &OnionErrorEncrypter{}
		obfuscatorReader := bytes.NewReader(b.Bytes())
		if err := obfuscator2.Decode(obfuscatorReader); err != nil {
			t.Fatalf("unable to decode obfuscator: %v", err)
		}

		if !reflect.DeepEqual(obfuscator, obfuscator2) {
			t.Fatalf("unable to reconstruct obfuscator: %v", err)
		}

		if !bytes.Equal(expectedSharedSecret, obfuscator.sharedSecret[:]) {
			t.Fatalf("shared secret not match with spec: expected "+
				"%x, got %x", expectedSharedSecret,
				obfuscator.sharedSecret[:])
		}

		if i == 0 {
			// Emulate the situation when last hop creates the onion failure
			// message and send it back.
			obfuscatedData = obfuscator.EncryptError(true, failureData)
		} else {
			// Emulate the situation when forward node obfuscates
			// the onion failure.
			obfuscatedData = obfuscator.EncryptError(false, obfuscatedData)
		}

		// Decode the obfuscated data and check that it matches the
		// specification.
		expectedEncryptErrordData, err := hex.DecodeString(test.obfuscatedData)
		if err != nil {
			t.Fatalf("unable to decode spec obfusacted "+
				"data: %v", err)
		}
		if !bytes.Equal(expectedEncryptErrordData, obfuscatedData) {
			t.Fatalf("obfuscated data not match spec: expected %x, "+
				"got %x", expectedEncryptErrordData[:],
				obfuscatedData[:])
		}
	}

	deobfuscator := NewOnionErrorDecrypter(&Circuit{
		SessionKey:  sessionKey,
		PaymentPath: paymentPath,
	})

	// Emulate that sender node receives the failure message and trying to
	// unwrap it, by applying obfuscation and checking the hmac.
	pubKey, deobfuscatedData, err := deobfuscator.DecryptError(obfuscatedData)
	if err != nil {
		t.Fatalf("unable to de-obfuscate the onion failure: %v", err)
	}

	// Check that message have been properly de-obfuscated.
	if !bytes.Equal(deobfuscatedData, failureData) {
		t.Fatalf("data not equals, expected: \"%v\", real: \"%v\"",
			string(failureData), string(deobfuscatedData))
	}

	// We should understand the node from which error have been received.
	if !bytes.Equal(pubKey.SerializeCompressed(),
		paymentPath[len(paymentPath)-1].SerializeCompressed()) {
		t.Fatalf("unable to properly conclude from which node in " +
			"the path we received an error")
	}
}
