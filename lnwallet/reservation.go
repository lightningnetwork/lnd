package lnwallet

import (
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

// ChannelReservation...
type ChannelReservation struct {
	sync.RWMutex // All fields below owned by the lnwallet.

	//for CLTV it is nLockTime, for CSV it's nSequence, for segwit it's not needed
	fundingLockTime int64
	fundingAmount   btcutil.Amount

	//Current state of the channel, progesses through until complete
	//Makes sure we can't go backwards and only accept messages once
	channelState uint8

	theirInputs []*wire.TxIn
	ourInputs   []*wire.TxIn

	// NOTE(j): FundRequest assumes there is only one change (see ChangePkScript)
	theirChange []*wire.TxOut
	ourChange   []*wire.TxOut

	theirMultiSigKey *btcec.PublicKey

	// In order of sorted inputs. Sorting is done in accordance
	// to BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	ourFundingSigs   [][]byte
	theirFundingSigs [][]byte

	ourRevokeHash    [wire.HashSize]byte
	ourCommitmentSig []byte

	partialState *OpenChannelState

	reservationID uint64
	wallet        *LightningWallet

	chanOpen chan *LightningChannel
}

// newChannelReservation...
func newChannelReservation(t FundingType, fundingAmt btcutil.Amount,
	minFeeRate btcutil.Amount, wallet *LightningWallet, id uint64) *ChannelReservation {
	return &ChannelReservation{
		fundingAmount: fundingAmt,
		// TODO(roasbeef): assumes balanced symmetric channels.
		partialState: &OpenChannelState{
			capacity:    fundingAmt * 2,
			fundingType: t,
		},
		wallet:        wallet,
		reservationID: id,
	}
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

// OurFunds...
func (r *ChannelReservation) OurFunds() ([]*wire.TxIn, []*wire.TxOut) {
	r.RLock()
	defer r.RUnlock()
	return r.ourInputs, r.ourChange
}

func (r *ChannelReservation) OurKeys() (*btcec.PrivateKey, *btcec.PrivateKey) {
	r.RLock()
	defer r.RUnlock()
	return r.partialState.multiSigKey, r.partialState.ourCommitKey
}

// AddFunds...
// TODO(roasbeef): add commitment txns, etc.
func (r *ChannelReservation) AddFunds(theirInputs []*wire.TxIn, theirChangeOutputs []*wire.TxOut, multiSigKey *btcec.PublicKey) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addCounterPartyFundsMsg{
		pendingFundingID:   r.reservationID,
		theirInputs:        theirInputs,
		theirChangeOutputs: theirChangeOutputs,
		theirKey:           multiSigKey,
		err:                errChan,
	}

	return <-errChan
}

// OurFundingSigs...
func (r *ChannelReservation) OurFundingSigs() [][]byte {
	r.RLock()
	defer r.RUnlock()
	return r.ourFundingSigs
}

// OurCommitmentSig
func (r *ChannelReservation) OurCommitmentSig() []byte {
	r.RLock()
	defer r.RUnlock()
	return r.ourCommitmentSig
}

// TheirFunds...
// TODO(roasbeef): return error if accessors not yet populated?
func (r *ChannelReservation) TheirFunds() ([]*wire.TxIn, []*wire.TxOut) {
	r.RLock()
	defer r.RUnlock()
	return r.theirInputs, r.theirChange
}

func (r *ChannelReservation) TheirKeys() (*btcec.PublicKey, *btcec.PublicKey) {
	r.RLock()
	defer r.RUnlock()
	return r.theirMultiSigKey, r.partialState.theirCommitKey
}

// CompleteFundingReservation...
// TODO(roasbeef): add commit sig also
func (r *ChannelReservation) CompleteReservation(theirSigs [][]byte) error {
	errChan := make(chan error, 1)

	r.wallet.msgChan <- &addCounterPartySigsMsg{
		pendingFundingID: r.reservationID,
		theirSigs:        theirSigs,
		err:              errChan,
	}

	return <-errChan
}

// FinalFundingTransaction...
func (r *ChannelReservation) FundingTx() *wire.MsgTx {
	r.RLock()
	defer r.RUnlock()
	return r.partialState.fundingTx
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

// * finish reset of tests
// * comment out stuff that'll need a node.
// * start on commitment side
//   * implement rusty's shachain
//   * set up logic to get notification from node when funding tx gets 6 deep.
//     * prob spawn into ChainNotifier struct
//   * create builder for initial funding transaction
//     * fascade through the wallet, for signing and such.
//   * channel should have active namespace to it's bucket, query at that point fo past commits etc
