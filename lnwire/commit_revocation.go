package lnwire

import (
	"fmt"
	"io"
)

// Multiple Clearing Requests are possible by putting this inside an array of
// clearing requests
type CommitRevocation struct {
	// We can use a different data type for this if necessary...
	ChannelID uint64

	// Revocation to use
	RevocationProof [20]byte

	//Next revocation to use
	NextRevocationHash [20]byte
}

func (c *CommitRevocation) Decode(r io.Reader, pver uint32) error {
	// ChannelID(8)
	// CommitmentHeight(8)
	// RevocationProof(20)
	err := readElements(r,
		&c.ChannelID,
		&c.RevocationProof,
		&c.NextRevocationHash,
	)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new CommitRevocation
func NewCommitRevocation() *CommitRevocation {
	return &CommitRevocation{}
}

// Serializes the item from the CommitRevocation struct
// Writes the data to w
func (c *CommitRevocation) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.RevocationProof,
		c.NextRevocationHash,
	)
	if err != nil {
		return err
	}

	return nil
}

func (c *CommitRevocation) Command() uint32 {
	return CmdCommitRevocation
}

func (c *CommitRevocation) MaxPayloadLength(uint32) uint32 {
	return 48
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *CommitRevocation) Validate() error {
	// We're good!
	return nil
}

func (c *CommitRevocation) String() string {
	return fmt.Sprintf("\n--- Begin CommitRevocation ---\n") +
		fmt.Sprintf("ChannelID:\t\t%d\n", c.ChannelID) +
		fmt.Sprintf("RevocationProof:\t%x\n", c.RevocationProof) +
		fmt.Sprintf("NextRevocationHash:\t%x\n", c.NextRevocationHash) +
		fmt.Sprintf("--- End CommitRevocation ---\n")
}
