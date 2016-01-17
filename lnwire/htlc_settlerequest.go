package lnwire

import (
	"fmt"
	"io"
)

// Multiple Clearing Requests are possible by putting this inside an array of
// clearing requests
type HTLCSettleRequest struct {
	// We can use a different data type for this if necessary...
	ChannelID uint64

	// ID of this request
	HTLCKey HTLCKey

	// Redemption Proofs (R-Values)
	RedemptionProofs []*[20]byte
}

func (c *HTLCSettleRequest) Decode(r io.Reader, pver uint32) error {
	// ChannelID(8)
	// HTLCKey(8)
	// Expiry(4)
	// Amount(4)
	// NextHop(20)
	// ContractType(1)
	// RedemptionHashes (numOfHashes * 20 + numOfHashes)
	// Blob(2+blobsize)
	err := readElements(r,
		&c.ChannelID,
		&c.HTLCKey,
		&c.RedemptionProofs,
	)
	if err != nil {
		return err
	}

	return nil
}

// Creates a new HTLCSettleRequest
func NewHTLCSettleRequest() *HTLCSettleRequest {
	return &HTLCSettleRequest{}
}

// Serializes the item from the HTLCSettleRequest struct
// Writes the data to w
func (c *HTLCSettleRequest) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.HTLCKey,
		c.RedemptionProofs,
	)
	if err != nil {
		return err
	}

	return nil
}

func (c *HTLCSettleRequest) Command() uint32 {
	return CmdHTLCSettleRequest
}

func (c *HTLCSettleRequest) MaxPayloadLength(uint32) uint32 {
	// 21*15+16
	return 331
}

// Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *HTLCSettleRequest) Validate() error {
	// We're good!
	return nil
}

func (c *HTLCSettleRequest) String() string {
	var redemptionProofs string
	for i, rh := range c.RedemptionProofs {
		redemptionProofs += fmt.Sprintf("\n\tSlice\t%d\n", i)
		redemptionProofs += fmt.Sprintf("\t\tRedemption Proof: %x\n", *rh)
	}

	return fmt.Sprintf("\n--- Begin HTLCSettleRequest ---\n") +
		fmt.Sprintf("ChannelID:\t%d\n", c.ChannelID) +
		fmt.Sprintf("HTLCKey:\t%d\n", c.HTLCKey) +
		fmt.Sprintf("RedemptionHashes:") +
		redemptionProofs +
		fmt.Sprintf("--- End HTLCSettleRequest ---\n")
}
