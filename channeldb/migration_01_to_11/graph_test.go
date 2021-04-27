package migration_01_to_11

import (
	"image/color"
	"math/big"
	prand "math/rand"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
)

var (
	testAddr = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}
	anotherAddr, _ = net.ResolveTCPAddr("tcp",
		"[2001:db8:85a3:0:0:8a2e:370:7334]:80")
	testAddrs = []net.Addr{testAddr, anotherAddr}

	testSig = &btcec.Signature{
		R: new(big.Int),
		S: new(big.Int),
	}
	_, _ = testSig.R.SetString("63724406601629180062774974542967536251589935445068131219452686511677818569431", 10)
	_, _ = testSig.S.SetString("18801056069249825825291287104931333862866033135609736119018462340006816851118", 10)

	testFeatures = lnwire.NewFeatureVector(nil, nil)
)

func createLightningNode(db *DB, priv *btcec.PrivateKey) (*LightningNode, error) {
	updateTime := prand.Int63()

	pub := priv.PubKey().SerializeCompressed()
	n := &LightningNode{
		HaveNodeAnnouncement: true,
		AuthSigBytes:         testSig.Serialize(),
		LastUpdate:           time.Unix(updateTime, 0),
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "kek" + string(pub[:]),
		Features:             testFeatures,
		Addresses:            testAddrs,
		db:                   db,
	}
	copy(n.PubKeyBytes[:], priv.PubKey().SerializeCompressed())

	return n, nil
}

func createTestVertex(db *DB) (*LightningNode, error) {
	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

	return createLightningNode(db, priv)
}
