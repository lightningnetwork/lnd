package channeldb

import (
	"github.com/roasbeef/btcutil"
	"io"
	"github.com/roasbeef/btcd/wire"
	"github.com/boltdb/bolt"
	"encoding/binary"
	"bytes"
)

var (
	// invoiceBucket is the name of the bucket within the database that
	// stores all data related to payments.
	// Within the payments bucket, each invoice is keyed by its invoice ID
	// which is a monotonically increasing uint64.
	// BoltDB sequence feature is used for generating monotonically increasing
	// id.
	paymentBucket = []byte("payments")
)

type OutgoingPayment struct {
	Invoice
	// Total fee paid
	Fee btcutil.Amount
	// Path including starting and ending nodes
	Path [][]byte
	TimeLockLength uint64
	// We probably need both RHash and Preimage
	// because we start knowing only RHash
	RHash [32]byte
}

func validatePayment(p *OutgoingPayment) error {
	err := validateInvoice(&p.Invoice)
	if err != nil {
		return err
	}
	return nil
}

// AddPayment adds payment to DB.
// There is no checking that payment with the same hash already exist.
func (db *DB) AddPayment(p *OutgoingPayment) error {
	err := validatePayment(p)
	if err != nil {
		return err
	}
	// We serialize before writing to database
	// so no db access in the case of serialization errors
	b := new(bytes.Buffer)
	err = serializeOutgoingPayment(b, p)
	if err != nil {
		return err
	}
	paymentBytes := b.Bytes()
	return db.Update(func (tx *bolt.Tx) error {
		payments, err := tx.CreateBucketIfNotExists(paymentBucket)
		if err != nil {
			return err
		}
		paymentId, err := payments.NextSequence()
		if err != nil {
			return err
		}
		// We use BigEndian for keys because
		// it orders keys in ascending order
		paymentIdBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(paymentIdBytes, paymentId)
		err = payments.Put(paymentIdBytes, paymentBytes)
		if err != nil {
			return err
		}
		return nil
	})
}

// FetchAllPayments returns all outgoing payments in DB.
func (db *DB) FetchAllPayments() ([]*OutgoingPayment, error) {
	var payments []*OutgoingPayment
	err := db.View(func (tx *bolt.Tx) error {
		bucket := tx.Bucket(paymentBucket)
		if bucket == nil {
			return nil
		}
		err := bucket.ForEach(func (k, v []byte) error {
			// Value can be nil if it is a sub-backet
			// so simply ignore it.
			if v == nil {
				return nil
			}
			r := bytes.NewReader(v)
			payment, err := deserializeOutgoingPayment(r)
			if err != nil {
				return err
			}
			payments = append(payments, payment)
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return payments, nil
}

// DeleteAllPayments deletes all payments from DB.
// If payments bucket does not exist it will create
// new bucket without error.
func (db *DB) DeleteAllPayments() error {
	return db.Update(func (tx *bolt.Tx) error {
		err := tx.DeleteBucket(paymentBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}
		_, err = tx.CreateBucket(paymentBucket)
		if err != nil {
			return err
		}
		return err
	})
}

func serializeOutgoingPayment(w io.Writer, p *OutgoingPayment) error {
	err := serializeInvoice(w, &p.Invoice)
	if err != nil {
		return err
	}
	// Serialize fee.
	feeBytes := make([]byte, 8)
	byteOrder.PutUint64(feeBytes, uint64(p.Fee))
	_, err = w.Write(feeBytes)
	if err != nil {
		return err
	}
	// Serialize path.
	pathLen := uint32(len(p.Path))
	pathLenBytes := make([]byte, 4)
	byteOrder.PutUint32(pathLenBytes, pathLen)
	_, err = w.Write(pathLenBytes)
	if err != nil {
		return err
	}
	for i := uint32(0); i < pathLen; i++ {
		err := wire.WriteVarBytes(w, 0, p.Path[i])
		if err != nil {
			return err
		}
	}
	// Serialize TimeLockLength
	timeLockLengthBytes := make([]byte, 8)
	byteOrder.PutUint64(timeLockLengthBytes, p.TimeLockLength)
	_, err = w.Write(timeLockLengthBytes)
	if err != nil {
		return err
	}
	// Serialize RHash
	_, err = w.Write(p.RHash[:])
	if err != nil {
		return err
	}
	return nil
}

func deserializeOutgoingPayment(r io.Reader) (*OutgoingPayment, error) {
	p := &OutgoingPayment{}
	// Deserialize invoice
	inv, err := deserializeInvoice(r)
	if err != nil {
		return nil, err
	}
	p.Invoice = *inv
	// Deserialize fee
	feeBytes := make([]byte, 8)
	_, err = r.Read(feeBytes)
	if err != nil {
		return nil, err
	}
	p.Fee = btcutil.Amount(byteOrder.Uint64(feeBytes))
	// Deserialize path
	pathLenBytes := make([]byte, 4)
	_, err = r.Read(pathLenBytes)
	if err != nil {
		return nil, err
	}
	pathLen := byteOrder.Uint32(pathLenBytes)
	path := make([][]byte, pathLen)
	for i := uint32(0); i<pathLen; i++ {
		// Each node in path have 33 bytes. It may be changed in future.
		// So put 100 here.
		path[i], err = wire.ReadVarBytes(r, 0, 100, "Node id")
		if err != nil {
			return nil, err
		}
	}
	p.Path = path
	// Deserialize TimeLockLength
	timeLockLengthBytes := make([]byte, 8)
	_, err = r.Read(timeLockLengthBytes)
	if err != nil {
		return nil, err
	}
	p.TimeLockLength = byteOrder.Uint64(timeLockLengthBytes)
	// Deserialize RHash
	_, err = r.Read(p.RHash[:])
	if err != nil {
		return nil, err
	}
	return p, nil
}