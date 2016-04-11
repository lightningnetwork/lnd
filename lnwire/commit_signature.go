package lnwire

import (
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"io"
)

// Multiple Clearing Requests are possible by putting this inside an array of
// clearing requests
type CommitSignature struct {
	// List of HTLC Keys which are updated from all parties
	//UpdatedHTLCKeys []uint64
	HighestPosition uint64

	// Total miners' fee that was used
	Fee btcutil.Amount

	// Signature for the new Commitment
	CommitSig *btcec.Signature // Requester's Commitment
}

func (c *CommitSignature) Decode(r io.Reader, pver uint32) error {
	// ChannelID(8)
	// CommitmentHeight(8)
	// RevocationHash(20)
	// Fee(8)
	// RequesterCommitSig(73max+2)
	err := readElements(r,
		&c.HighestPosition,
		&c.Fee,
		&c.CommitSig,
	)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new CommitSignature
func NewCommitSignature() *CommitSignature {
	return &CommitSignature{}
}

// Serializes the item from the CommitSignature struct
// Writes the data to w
func (c *CommitSignature) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.HighestPosition,
		c.Fee,
		c.CommitSig,
	)
	if err != nil {
		return err
	}

	return nil
}

func (c *CommitSignature) Command() uint32 {
	return CmdCommitSignature
}

func (c *CommitSignature) MaxPayloadLength(uint32) uint32 {
	return 8192
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *CommitSignature) Validate() error {
	if c.Fee < 0 {
		// While fees can be negative, it's too confusing to allow
		// negative payments. Maybe for some wallets, but not this one!
		return fmt.Errorf("Amount paid cannot be negative.")
	}
	// We're good!
	return nil
}

func (c *CommitSignature) String() string {
	// c.ChannelID,
	// c.CommitmentHeight,
	// c.RevocationHash,
	// c.UpdatedHTLCKeys,
	// c.Fee,
	// c.CommitSig,
	var serializedSig []byte
	if c.CommitSig != nil && c.CommitSig.R != nil {
		serializedSig = c.CommitSig.Serialize()
	}

	return fmt.Sprintf("\n--- Begin CommitSignature ---\n") +
		fmt.Sprintf("HighestPosition:\t%d\n", c.HighestPosition) +
		fmt.Sprintf("Fee:\t\t\t%s\n", c.Fee.String()) +
		fmt.Sprintf("CommitSig:\t\t%x\n", serializedSig) +
		fmt.Sprintf("--- End CommitSignature ---\n")
}
