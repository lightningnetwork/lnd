package bolt12

import (
	"errors"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// ErrTooManyChains is returned when offer_chains declares more entries than
// maxOfferChains.
var ErrTooManyChains = errors.New("offer_chains exceeds maxOfferChains")

const (
	// chainHashLen is the length of a chain hash (32 bytes).
	chainHashLen = 32

	// maxOfferChains caps decoded offer_chains entries. This is a sanity
	// check to prevent excessive memory allocation and is not a protocol
	// limit but a local implementation choice.
	maxOfferChains = 32
)

// ChainsRecord holds one or more chain hashes for the offer_chains field.
type ChainsRecord struct {
	Chains [][chainHashLen]byte
}

var _ tlv.RecordProducer = (*ChainsRecord)(nil)

// Record returns a TLV record for ChainsRecord.
func (c *ChainsRecord) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, c,
		func() uint64 {
			return uint64(len(c.Chains)) * chainHashLen
		},
		encodeChainsRecord,
		decodeChainsRecord,
	)
}

// encodeChainsRecord writes the chain hashes in sequence, without a count
// prefix.
func encodeChainsRecord(w io.Writer, val any, _ *[8]byte) error {
	c, ok := val.(*ChainsRecord)
	if !ok {
		return fmt.Errorf("expected *ChainsRecord, got %T", val)
	}

	for _, chain := range c.Chains {
		if _, err := w.Write(chain[:]); err != nil {
			return err
		}
	}

	return nil
}

// decodeChainsRecord caps the count at maxOfferChains to bound allocation.
func decodeChainsRecord(r io.Reader, val any, _ *[8]byte, l uint64) error {
	c, ok := val.(*ChainsRecord)
	if !ok {
		return fmt.Errorf("expected *ChainsRecord, got %T", val)
	}

	if l%chainHashLen != 0 {
		return fmt.Errorf("chains length %d not a multiple of %d", l,
			chainHashLen)
	}

	numChains := l / chainHashLen
	if numChains > maxOfferChains {
		return fmt.Errorf("%w: %d > %d", ErrTooManyChains, numChains,
			maxOfferChains)
	}

	c.Chains = make([][chainHashLen]byte, numChains)
	for i := range c.Chains {
		if _, err := io.ReadFull(r, c.Chains[i][:]); err != nil {
			return err
		}
	}

	return nil
}
