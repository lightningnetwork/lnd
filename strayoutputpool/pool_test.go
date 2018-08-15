package strayoutputpool

import (
	"io/ioutil"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/test"
)

func TestGenSweepScript(t *testing.T) {
	db, err := initDB()
	if err != nil {
		t.Fatal(err)
	}

	localFundingPrivKey, err := test.PrivkeyFromHex(
		"30ff4956bbdd3222d44cc5e8a1261dab1e07957bdac5ae88fe3261ef321f3749",
	)
	if err != nil {
		t.Fatalf("Failed to parse serialized privkey: %v", err)
	}

	localPaymentPrivKey, err := test.PrivkeyFromHex(
		"bb13b121cdc357cd2e608b0aea294afca36e2b34cf958e2e6451a2f274694491",
	)
	if err != nil {
		t.Fatalf("Failed to parse serialized privkey: %v", err)
	}

	pool := NewDBStrayOutputsPool(&PoolConfig{
		DB:             db,
		Estimator:      &lnwallet.StaticFeeEstimator{FeePerKW: 50},
		GenSweepScript: func() ([]byte, error) { return nil, nil },
		Signer: &test.MockMultSigner{
			Privkeys: []*btcec.PrivateKey{
				localFundingPrivKey, localPaymentPrivKey,
			},
			NetParams: &chaincfg.RegressionNetParams,
		},
	})

	_, err = pool.GenSweepTx()
	if err != nil {
		t.Fatal("Couldn't generate sweep transaction: ", err)
	}
}

func initDB() (*channeldb.DB, error) {
	tempPath, err := ioutil.TempDir("", "switchdb")
	if err != nil {
		return nil, err
	}

	db, err := channeldb.Open(tempPath)
	if err != nil {
		return nil, err
	}

	return db, nil
}
