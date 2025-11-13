package migration_01_to_11

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	bitcoinCfg "github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/zpay32"
	litecoinCfg "github.com/ltcsuite/ltcd/chaincfg"
)

var (
	testPrivKeyBytes = []byte{
		0x2b, 0xd8, 0x06, 0xc9, 0x7f, 0x0e, 0x00, 0xaf,
		0x1a, 0x1f, 0xc3, 0x32, 0x8f, 0xa7, 0x63, 0xa9,
		0x26, 0x97, 0x23, 0xc8, 0xdb, 0x8f, 0xac, 0x4f,
		0x93, 0xaf, 0x71, 0xdb, 0x18, 0x6d, 0x6e, 0x90,
	}

	testCltvDelta = int32(50)
)

// beforeMigrationFuncV11 insert the test invoices in the database.
func beforeMigrationFuncV11(t *testing.T, d *DB, invoices []Invoice) {
	err := kvdb.Update(d, func(tx kvdb.RwTx) error {
		invoicesBucket, err := tx.CreateTopLevelBucket(
			invoiceBucket,
		)
		if err != nil {
			return err
		}

		invoiceNum := uint32(1)
		for _, invoice := range invoices {
			var invoiceKey [4]byte
			byteOrder.PutUint32(invoiceKey[:], invoiceNum)
			invoiceNum++

			var buf bytes.Buffer
			err := serializeInvoiceLegacy(&buf, &invoice)
			if err != nil {
				return err
			}

			err = invoicesBucket.Put(
				invoiceKey[:], buf.Bytes(),
			)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMigrateInvoices checks that invoices are migrated correctly.
func TestMigrateInvoices(t *testing.T) {
	t.Parallel()

	payReqBtc, err := getPayReq(&bitcoinCfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}

	var ltcNetParams bitcoinCfg.Params
	ltcNetParams.Bech32HRPSegwit = litecoinCfg.MainNetParams.Bech32HRPSegwit
	payReqLtc, err := getPayReq(&ltcNetParams)
	if err != nil {
		t.Fatal(err)
	}

	invoices := []Invoice{
		{
			PaymentRequest: []byte(payReqBtc),
		},
		{
			PaymentRequest: []byte(payReqLtc),
		},
	}

	// Verify that all invoices were migrated.
	afterMigrationFunc := func(d *DB) {
		dbInvoices, err := d.FetchAllInvoices(false)
		if err != nil {
			t.Fatalf("unable to fetch invoices: %v", err)
		}

		if len(invoices) != len(dbInvoices) {
			t.Fatalf("expected %d invoices, got %d", len(invoices),
				len(dbInvoices))
		}

		for _, dbInvoice := range dbInvoices {
			if dbInvoice.FinalCltvDelta != testCltvDelta {
				t.Fatal("incorrect final cltv delta")
			}
			if dbInvoice.Expiry != 3600*time.Second {
				t.Fatal("incorrect expiry")
			}
			if len(dbInvoice.Htlcs) != 0 {
				t.Fatal("expected no htlcs after migration")
			}
		}
	}

	applyMigration(t,
		func(d *DB) { beforeMigrationFuncV11(t, d, invoices) },
		afterMigrationFunc,
		MigrateInvoices,
		false)
}

// TestMigrateInvoicesHodl checks that a hodl invoice in the accepted state
// fails the migration.
func TestMigrateInvoicesHodl(t *testing.T) {
	t.Parallel()

	payReqBtc, err := getPayReq(&bitcoinCfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}

	invoices := []Invoice{
		{
			PaymentRequest: []byte(payReqBtc),
			Terms: ContractTerm{
				State: ContractAccepted,
			},
		},
	}

	applyMigration(t,
		func(d *DB) { beforeMigrationFuncV11(t, d, invoices) },
		func(d *DB) {},
		MigrateInvoices,
		true)
}

// signDigestCompact generates a test signature to be used in the generation of
// test payment requests.
func signDigestCompact(hash []byte) ([]byte, error) {
	// Should the signature reference a compressed public key or not.
	isCompressedKey := true

	privKey, _ := btcec.PrivKeyFromBytes(testPrivKeyBytes)

	// ecdsa.SignCompact returns a pubkey-recoverable signature
	return ecdsa.SignCompact(privKey, hash, isCompressedKey), nil
}

// getPayReq creates a payment request for the given net.
func getPayReq(net *bitcoinCfg.Params) (string, error) {
	options := []func(*zpay32.Invoice){
		zpay32.CLTVExpiry(uint64(testCltvDelta)),
		zpay32.Description("test"),
	}

	payReq, err := zpay32.NewInvoice(
		net, [32]byte{}, time.Unix(1, 0), options...,
	)
	if err != nil {
		return "", err
	}
	return payReq.Encode(
		zpay32.MessageSigner{
			SignCompact: signDigestCompact,
		},
	)
}
