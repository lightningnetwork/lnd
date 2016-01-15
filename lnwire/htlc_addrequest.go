package lnwire

import (
	"fmt"
	"io"
)

//Multiple Clearing Requests are possible by putting this inside an array of
//clearing requests
type HTLCAddRequest struct {
	//We can use a different data type for this if necessary...
	ChannelID uint64

	//ID of this request
	HTLCKey HTLCKey

	//When the HTLC expires
	Expiry uint32

	//Amount to pay in the hop
	//Difference between hop and first item in blob is the fee to complete
	Amount CreditsAmount

	//RefundContext is for payment cancellation
	//TODO (j): not currently in use, add later
	RefundContext HTLCKey

	//Contract Type
	//first 4 bits is n, second for is m, in n-of-m "multisig"
	ContractType uint8

	//Redemption Hashes
	RedemptionHashes []*[20]byte

	//Data to parse&pass on to the next node
	//Eventually, we need to make this into a group of 2 nested structs?
	Blob []byte
}

func (c *HTLCAddRequest) Decode(r io.Reader, pver uint32) error {
	//ChannelID(8)
	//HTLCKey(8)
	//Expiry(4)
	//Amount(4)
	//ContractType(1)
	//RedemptionHashes (numOfHashes * 20 + numOfHashes)
	//Blob(2+blobsize)
	err := readElements(r,
		&c.ChannelID,
		&c.HTLCKey,
		&c.Expiry,
		&c.Amount,
		&c.ContractType,
		&c.RedemptionHashes,
		&c.Blob,
	)
	if err != nil {
		return err
	}

	return nil
}

//Creates a new HTLCAddRequest
func NewHTLCAddRequest() *HTLCAddRequest {
	return &HTLCAddRequest{}
}

//Serializes the item from the HTLCAddRequest struct
//Writes the data to w
func (c *HTLCAddRequest) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.HTLCKey,
		c.Expiry,
		c.Amount,
		c.ContractType,
		c.RedemptionHashes,
		c.Blob,
	)
	if err != nil {
		return err
	}

	return nil
}

func (c *HTLCAddRequest) Command() uint32 {
	return CmdHTLCAddRequest
}

func (c *HTLCAddRequest) MaxPayloadLength(uint32) uint32 {
	//base size ~110, but blob can be variable.
	//shouldn't be bigger than 8K though...
	return 8192
}

//Makes sure the struct data is valid (e.g. no negatives or invalid pkscripts)
func (c *HTLCAddRequest) Validate() error {
	if c.Amount < 0 {
		//While fees can be negative, it's too confusing to allow
		//negative payments. Maybe for some wallets, but not this one!
		return fmt.Errorf("Amount paid cannot be negative.")
	}
	//We're good!
	return nil
}

func (c *HTLCAddRequest) String() string {
	var redemptionHashes string
	for i, rh := range c.RedemptionHashes {
		redemptionHashes += fmt.Sprintf("\n\tSlice\t%d\n", i)
		redemptionHashes += fmt.Sprintf("\t\tRedemption Hash: %x\n", *rh)
	}

	return fmt.Sprintf("\n--- Begin HTLCAddRequest ---\n") +
		fmt.Sprintf("ChannelID:\t%d\n", c.ChannelID) +
		fmt.Sprintf("HTLCKey:\t%d\n", c.HTLCKey) +
		fmt.Sprintf("Expiry:\t\t%d\n", c.Expiry) +
		fmt.Sprintf("Amount\t\t%d\n", c.Amount) +
		fmt.Sprintf("ContractType:\t%d (%b)\n", c.ContractType, c.ContractType) +
		fmt.Sprintf("RedemptionHashes:") +
		redemptionHashes +
		fmt.Sprintf("Blob:\t\t\t\t%x\n", c.Blob) +
		fmt.Sprintf("--- End HTLCAddRequest ---\n")
}
