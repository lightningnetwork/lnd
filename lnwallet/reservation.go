package lnwallet

import (
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

// ChannelContribution...
type ChannelContribution struct {
	// Amount of funds contributed to the funding transaction.
	FundingAmount btcutil.Amount

	// Inputs to the funding transaction.
	Inputs []*wire.TxIn

	// Outputs to be used in the case that the total value of the fund
	// ing inputs is greather than the total potential channel capacity.
	ChangeOutputs []*wire.TxOut

	// The key to be used for the funding transaction's P2SH multi-sig
	// 2-of-2 output.
	MultiSigKey *btcec.PublicKey

	// The key to be used for this party's version of the commitment
	// transaction.
	CommitKey *btcec.PublicKey

	// Address to be used for delivery of cleared channel funds in the scenario
	// of a cooperative channel closure.
	DeliveryAddress btcutil.Address

	// Hash to be used as the revocation for the initial version of this
	// party's commitment transaction.
	RevocationHash [wire.HashSize]byte

	// The delay (in blocks) to be used for the pay-to-self output in this
	// party's version of the commitment transaction.
	CsvDelay uint32
}

// ChannelReservation...
type ChannelReservation struct {
	fundingType FundingType

	// This mutex MUST be held when either reading or modifying any of the
	// fields below.
	sync.RWMutex

	// For CLTV it is nLockTime, for CSV it's nSequence, for segwit it's
	// not needed
	fundingLockTime uint32

	// In order of sorted inputs. Sorting is done in accordance
	// to BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	ourFundingSigs   [][]byte
	theirFundingSigs [][]byte

	// Our signature for their version of the commitment transaction.
	ourCommitmentSig []byte

	ourContribution   *ChannelContribution
	theirContribution *ChannelContribution

	partialState *OpenChannelState

	// The ID of this reservation, used to uniquely track the reservation
	// throughout its lifetime.
	reservationID uint64

	// A channel which will be sent on once the channel is considered
	// 'open'. A channel is open once the funding transaction has reached
	// a sufficient number of confirmations.
	chanOpen chan *LightningChannel

	wallet *LightningWallet
}

// newChannelReservation...
func newChannelReservation(t FundingType, fundingAmt btcutil.Amount,
	minFeeRate btcutil.Amount, wallet *LightningWallet, id uint64) *ChannelReservation {
	// TODO(roasbeef): CSV here, or on delay?
	return &ChannelReservation{
		fundingType: t,
		ourContribution: &ChannelContribution{
			FundingAmount: fundingAmt,
		},
		theirContribution: &ChannelContribution{
			FundingAmount: fundingAmt,
		},
		partialState: &OpenChannelState{
			// TODO(roasbeef): assumes balanced symmetric channels.
			Capacity:     fundingAmt * 2,
			OurBalance:   fundingAmt,
			TheirBalance: fundingAmt,
			MinFeePerKb:  minFeeRate,
		},
		reservationID: id,
		wallet:        wallet,
	}
}

// OurContribution...
// NOTE: This SHOULD NOT be modified.
func (r *ChannelReservation) OurContribution() *ChannelContribution {
	r.RLock()
	defer r.RUnlock()
	return r.ourContribution
}

// ProcessContribution...
func (r *ChannelReservation) ProcessContribution(theirContribution *ChannelContribution) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addContributionMsg{
		pendingFundingID: r.reservationID,
		contribution:     theirContribution,
		err:              errChan,
	}

	return <-errChan
}

func (r *ChannelReservation) TheirContribution() *ChannelContribution {
	r.RLock()
	defer r.RUnlock()
	return r.theirContribution
}

// OurSignatures...
func (r *ChannelReservation) OurSignatures() ([][]byte, []byte) {
	r.RLock()
	defer r.RUnlock()
	return r.ourFundingSigs, r.ourCommitmentSig
}

// CompleteFundingReservation...
// TODO(roasbeef): add commit sig also
func (r *ChannelReservation) CompleteReservation(fundingSigs [][]byte, commitmentSig []byte) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addCounterPartySigsMsg{
		pendingFundingID:   r.reservationID,
		theirFundingSigs:   fundingSigs,
		theirCommitmentSig: commitmentSig,
		err:                errChan,
	}

	return <-errChan
}

// OurSignatures...
func (r *ChannelReservation) TheirSignatures() ([][]byte, []byte) {
	r.RLock()
	defer r.RUnlock()
	return r.theirFundingSigs, r.partialState.TheirCommitSig
}

// FinalFundingTransaction...
func (r *ChannelReservation) FinalFundingTx() *wire.MsgTx {
	r.RLock()
	defer r.RUnlock()
	return r.partialState.FundingTx
}

// RequestFundingReserveCancellation...
// TODO(roasbeef): also return mutated state?
func (r *ChannelReservation) Cancel() error {
	errChan := make(chan error, 1)
	r.wallet.msgChan <- &fundingReserveCancelMsg{
		pendingFundingID: r.reservationID,
		err:              errChan,
	}

	return <-errChan
}

// WaitForChannelOpen...
func (r *ChannelReservation) WaitForChannelOpen() *LightningChannel {
	return nil
}

/*//FundRequest serialize
//(reading from ChannelReservation directly to reduce the amount of copies)
//We can move this stuff to another file too if it's too big...
func (r *ChannelReservation) SerializeFundRequest() ([]byte, error) {
	var err error

	//Buffer to dump in the serialized data
	b := new(bytes.Buffer)

	//Fund Request
	err = b.WriteByte(0x30)
	if err != nil {
		return nil, err
	}

	//ChannelType (1)
	//Default to current type
	err = b.WriteByte(uint8(0))
	if err != nil {
		return nil, err
	}

	//RequesterFundingAmount - The amount we are going to fund (8)
	//check for positive values
	err = binary.Write(b, binary.BigEndian, r.FundingAmount)
	if err != nil {
		return nil, err
	}

	//RequesterChannelMinCapacity (8)
	//The amount needed to accept and sign the channel commit later
	err = binary.Write(b, binary.BigEndian, r.MinTotalFundingAmount)
	if err != nil {
		return nil, err
	}

	//RevocationHash (20)
	//Our revocation hash being contributed (for CLTV/CSV)
	_, err = b.Write(btcutil.Hash160(r.ourRevocation))
	if err != nil {
		return nil, err
	}

	//CommitPubkey (33)
	//Our public key being used for the commitment
	ourPubKey := r.ourKey.PubKey().SerializeCompressed()
	if len(ourPubKey) != 33 { //validation, can remove later? (NO UNCOMPRESSED KEYS!)
		return nil, fmt.Errorf("Serialize FundReq: our Pubkey length incorrect")
	}
	_, err = b.Write(ourPubKey)
	if err != nil {
		return nil, err
	}

	//DeliveryPkHash (20)
	//For now it's a P2PKH, but we will add an extra byte later for the
	//option for P2SH
	//This is the address to send funds to when complete or refunded
	_, err = b.Write(r.ourDeliveryAddress)
	if err != nil {
		return nil, err
	}

	//ReserveAmount (8)
	//Our own reserve amount
	err = binary.Write(b, binary.BigEndian, r.ReserveAmount)
	if err != nil {
		return nil, err
	}

	//Minimum transaction fee per kb (8)
	err = binary.Write(b, binary.BigEndian, r.MinFeePerKb)
	if err != nil {
		return nil, err
	}

	//LockTime (4)
	err = binary.Write(b, binary.BigEndian, r.FundingLockTime)
	if err != nil {
		return nil, err
	}

	//Fee payer (default to split currently) (1)
	err = binary.Write(b, binary.BigEndian, 0)
	if err != nil {
		return nil, err
	}

	//ChangePkScript
	//Length (1)
	changeScriptLength := len(r.ourChange[0].PkScript)
	if changeScriptLength > 255 {
		return nil, fmt.Errorf("Your changeScriptLength is too long!")
	}
	err = binary.Write(b, binary.BigEndian, uint8(changeScriptLength))
	if err != nil {
		return nil, err
	}
	//For now it's a P2PKH, but we will add an extra byte later for the
	//option for P2SH
	//This is the address to send change to (only allow one)
	//ChangePkScript (length of script)
	_, err = b.Write(r.ourChange[0].PkScript)
	if err != nil {
		return nil, err
	}

	//Append the unsigned(!!) txins
	//First one byte for the amount of txins (1)
	if len(r.ourInputs) > 127 {
		return nil, fmt.Errorf("Too many txins")
	}
	err = b.WriteByte(uint8(len(r.ourInputs)))
	if err != nil {
		return nil, err
	}
	//Append the actual Txins (NumOfTxins * 36)
	//Do not include the sequence number to eliminate funny business
	for _, in := range r.ourInputs {
		//Hash
		_, err = b.Write(in.PreviousOutPoint.Hash.Bytes())
		if err != nil {
			return nil, err
		}
		//Index
		err = binary.Write(b, binary.BigEndian, in.PreviousOutPoint.Index)
		if err != nil {
			return nil, err
		}
	}

	return b.Bytes(), err
}

func (r *ChannelReservation) DeserializeFundRequest(wireMsg []byte) error {
	//Make sure we're not overwriting stuff...
	//Update the channelState to 1 before progressing if you want to re-do it.
	//Assumes only one thread is writing at a time
	if r.channelState > 1 {
		return fmt.Errorf("FundRequest: Channel State Mismatch")
	}

	var err error

	b := bytes.NewBuffer(wireMsg)
	msgid, _ := b.ReadByte()
	if msgid != 0x30 {
		return fmt.Errorf("Cannot deserialize: not a funding request")
	}

	if err != nil {
		return err
	}
	//Update the channel state as complete
	r.channelState = 2

	return nil
}

//Validation on the data being supplied from the fund request
func (r *ChannelReservation) ValidateFundRequest(wireMsg []byte) error {
	return nil
}

//Serialize CSV Revocation
//After the Commitment Transaction has been created, send a message to revoke this tx
func (r *ChannelReservation) SerializeCSVRefundRevocation() ([]byte, error) {
	return nil, nil
}

//Deserialize CSV Revocation
//Validate the revocation, after this step, the channel is fully set up
func (r *ChannelReservation) DeserializeCSVRefundRevocation() error {
	return nil
}*/

// * finish reset of tests
// * comment out stuff that'll need a node.
// * start on commitment side
//   * implement rusty's shachain
//   * set up logic to get notification from node when funding tx gets 6 deep.
//     * prob spawn into ChainNotifier struct
//   * create builder for initial funding transaction
//     * fascade through the wallet, for signing and such.
//   * channel should have active namespace to it's bucket, query at that point fo past commits etc
