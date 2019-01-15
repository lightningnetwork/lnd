package routing

import (
	"bytes"
	"encoding/binary"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	payAttemptBucket = []byte("pay-attempts")
	byteOrder        = binary.BigEndian
)

type payAttemptStore struct {
	DB *channeldb.DB
}

type payAttempt struct {
	paymentID uint64
	firstHop  lnwire.ShortChannelID
	htlcAdd   *lnwire.UpdateAddHTLC
	circuit   *sphinx.Circuit
}

func (s *payAttemptStore) storePayAttempt(p *payAttempt) error {

	var b bytes.Buffer
	if err := binary.Write(&b, byteOrder, p.firstHop.ToUint64()); err != nil {
		return err
	}
	if err := p.htlcAdd.Encode(&b, 0); err != nil {
		return err
	}

	if err := p.circuit.Encode(&b); err != nil {
		return err
	}

	paymentIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(paymentIDBytes, p.paymentID)

	return s.DB.Update(func(tx *bbolt.Tx) error {

		attempts, err := tx.CreateBucketIfNotExists(payAttemptBucket)
		if err != nil {
			return err
		}
		return attempts.Put(paymentIDBytes, b.Bytes())
	})
}

func (s *payAttemptStore) deletePayAttempt(pid uint64) error {

	paymentIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(paymentIDBytes, pid)

	return s.DB.Update(func(tx *bbolt.Tx) error {

		attempts, err := tx.CreateBucketIfNotExists(payAttemptBucket)
		if err != nil {
			return err
		}
		return attempts.Delete(paymentIDBytes)
	})
}

func (s *payAttemptStore) getPayAttempts() ([]*payAttempt, error) {
	var attempts []*payAttempt
	err := s.DB.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(payAttemptBucket)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(k, v []byte) error {

			p := &payAttempt{}
			p.paymentID = binary.BigEndian.Uint64(k)

			r := bytes.NewReader(v)

			var a uint64
			if err := binary.Read(r, byteOrder, &a); err != nil {
				return err
			}
			p.firstHop = lnwire.NewShortChanIDFromInt(a)

			htlc := &lnwire.UpdateAddHTLC{}
			if err := htlc.Decode(r, 0); err != nil {
				return err
			}
			p.htlcAdd = htlc

			circuit := &sphinx.Circuit{}
			if err := circuit.Decode(r); err != nil {
				return err
			}
			p.circuit = circuit

			attempts = append(attempts, p)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return attempts, nil
}
