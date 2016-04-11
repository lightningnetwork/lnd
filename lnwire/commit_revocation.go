package lnwire

import (
	"fmt"
	"io"
)

// Multiple Clearing Requests are possible by putting this inside an array of
// clearing requests
type CommitRevocation struct {
	//Next revocation to use
	NextRevocationHash [20]byte
	Revocation         [20]byte
}

func (c *CommitRevocation) Decode(r io.Reader, pver uint32) error {
	// NextRevocationHash(20)
	// Revocation(20)
	err := readElements(r,
		&c.NextRevocationHash,
		&c.Revocation,
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
		c.NextRevocationHash,
		c.Revocation,
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
		fmt.Sprintf("NextRevocationHash:\t%x\n", c.NextRevocationHash) +
		fmt.Sprintf("Revocation:\t%x\n", c.Revocation) +
		fmt.Sprintf("--- End CommitRevocation ---\n")
}
