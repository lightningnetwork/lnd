package lnstate

import (
	"fmt"
	//"github.com/roasbeef/btcd/btcec"
	//"atomic"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

//This is a state machine which allows for simultaneous high-volume
//bidirectional payments. The design goal is to for all steps to be
//non-blocking over-the-wire on signing/revoking with the counterparty.
//
//A *single* Lightning Network node on one reasonably high-speced computer
//should be able to conduct transactions at Visa-scale (one Lightning Network
//node can process thousands of transactions per second total), enabling
//incredible volume for the entire network. This state machine design should
//enable transaction volume for the entire network greater than current Visa
//card transaction load by many orders of magnitude.
//
//More info on the design is in lnwire/README.md

//NOTE: WORK IN PROGRESS (current working code in wallet is blocking -- it is
//implemented, but this version will be merged and replacing it soon)
//TODO: Will make channel-friendly & add in mutex stuff

//DUMMY FunCTIONS FOR UPDATING LATER
func disk() {
}
func net(msg lnwire.Message) {
	fmt.Println(msg)
}
func (l *LNChannel) sendErrorPkt() {
}

//Will copy in code from channel.go...

const (
	MAX_STAGED_HTLCS                 = 1000
	MAX_UNREVOKED_COMMITMENTS        = 16
	MAX_UPDATED_HTLCS_PER_COMMITMENT = 1000
)

//Currently, the mutex locks across the entire channel, it's possible to update
//it to be more granular and to have each PaymentDescriptor have its own locks
//(and lock both simultaneously for minimal parts). However, this is
//complicated and will be updated in the future. Right now, since there is a
//per-channel global-lock, this should work fine and be fairly performant.
//
//Before calling another function which does a lock, make sure to l.Unlock()
//first, e.g. if a packet received triggers a packet to be sent.

//PaymentDescriptor states
//PRESTAGE: We're not sure if the other guy wants to follow through
//STAGED: Both parties informally agree to add to their commitments
//SIGNING_AND_REVOKING: In the process of activating the HTLC
//
//Only one state is allowed at a time. timeout/settle can only begin on an
//ADD_COMPLETE state. If it times out before it hits completion, channel closes
//out uncooperatively.
//
//NOTE: Current design assumes if value%1000 == 200 as partially
//signed/revoked, this means that the code is reliant upon this as knowing when
//to force close out channels, etc.
const (
	//HTLC Add
	ADD_PRESTAGE             = 1000
	ADD_STAGED               = 1100
	ADD_SIGNING_AND_REVOKING = 1200
	ADD_COMPLETE             = 1300 //Most HTLCs should be this
	ADD_REJECTED             = 1999 //Staging request rejected

	//HTLC Timeout
	TIMEOUT_PRESTAGE             = 2000
	TIMEOUT_STAGED               = 2100
	TIMEOUT_SIGNING_AND_REVOKING = 2200
	TIMEOUT_COMPLETE             = 2300

	//HTLC Settle
	SETTLE_PRESTAGE             = 3000
	SETTLE_STAGED               = 3100
	SETTLE_SIGNING_AND_REVOKING = 3200
	SETTLE_COMPLETE             = 3300

	//TODO: Commitment states
)

//Example, Add Flow:
//1. Create the HTLC
//2. (you+they) Sign the commit
//3. (you+they) Send revocation when you get a new commit
//4. Update the state to account for revocation
//5. Both sides committed and revoked prior states, mark state as finished

type LNChannel struct {
	fundingTxIn *wire.TxIn
	channelDB   *channeldb.DB

	channelID uint64

	//The person who set up the channel is even, the person who responded
	//is odd. All HTLCKeys use even/odd numbering.
	//NOTE: should we change this to be a int64 and use positive/negative?
	isEven bool

	sync.RWMutex

	//Should update with new Commitments when fees are renegotiated
	feePerKb btcutil.Amount

	//HTLCs
	//Even/odd numbering in effect
	//Will just take the next number
	HTLCs map[lnwire.HTLCKey]*PaymentDescriptor
	//Last key in the map for incrementing
	//If the other party wants to use some stupid high number, then
	//they're basically wanting the channel to close out early...
	ourLastKey   lnwire.HTLCKey
	theirLastKey lnwire.HTLCKey

	//ourCommit/theirCommit is the most recent *signed* Commitment
	ourCommit                 *wire.MsgTx
	theirCommit               *wire.MsgTx
	ourCommitHeight           lnwire.CommitHeight
	ourUnrevokedCommitments   []lnwire.CommitHeight
	theirCommitHeight         lnwire.CommitHeight
	theirUnrevokedCommitments []lnwire.CommitHeight
	//UnrevoedStates indexed by CommitHeight, slice of HTLCKeys to update
	//as revoked/complete

	//NOTE: I think it's EASIER to reconstruct the Commitment than
	//to update because finding indicies and blah blah blah,
	//validation/verification screw that, the state is in the HTLCs
	//and the sig. We can optimize later.

	//ShaChain Height
	//Difference between ourCommitHeight and ourShaChain index is the
	//Commitments not yet revoked. If the difference is 1, then all prior
	//Commitments are revoked.
	ourShaChain   *shachain.HyperShaChain
	theirShaChain *shachain.HyperShaChain
}

type PaymentDescriptor struct {
	RHashes       []*[20]byte
	Timeout       uint32
	CreditsAmount lnwire.CreditsAmount
	Revocation    []*[20]byte
	Blob          []byte //next hop data
	PayToUs       bool

	State uint32 //Current state

	Index uint32   //Position in txout
	p2sh  [20]byte //cached p2sh script to use

	//Used during the SIGNING_AND_REVOKING process
	weSigned               bool
	theyRevoked            bool
	theySignedAndWeRevoked bool
	//This is the height which is revoked, anything before that
	//is assumed to still be valid.
	//This data point is useful for ...UnrevokedCommitments
	//if there's unrevoked commitments below these values,
	//then the state is not locked in
	//Used for add/settle/timeout
	ourRevokedHeight   lnwire.CommitHeight
	theirRevokedHeight lnwire.CommitHeight
}

//addHTLC will take in a PaymentDescriptor, adds to HTLCs and writes to disk
//Assumes the data has already been validated!
//NOTE: **MUST** HAVE THE MUTEX LOCKED ALREADY WHEN CALLED
func (l *LNChannel) addHTLC(h *PaymentDescriptor) (lnwire.HTLCKey, error) {
	//Sanity check
	if h.State != ADD_PRESTAGE {
		return 0, fmt.Errorf("addHTLC can only add PRESTAGE")
	}

	//Make sure we're not overfull
	//We should *never* hit this...
	//we subtract 3 in case we need to add one to correct for even/odd
	if l.ourLastKey >= ^lnwire.HTLCKey(0)-3 {
		return 0, fmt.Errorf("Channel full!!!")
	}

	//Assign a new value
	//We add 2 due to even/odd assignments
	l.ourLastKey += 2

	//Check whether the even/odd is invalid..
	//If it is, we iterate to the next one
	if l.ourLastKey%1 == 1 {
		if l.isEven {
			l.ourLastKey += 1
		}
	} else {
		if !l.isEven {
			l.ourLastKey += 1
		}
	}

	//Write the new HTLC
	l.HTLCs[l.ourLastKey] = h
	disk() //save l.ourLastKey and the htlcs

	return l.ourLastKey, nil
}

func (l *LNChannel) CreateHTLC(h *PaymentDescriptor) error {
	l.Lock()
	var err error
	//if h.State == ADD_PRESTAGE {
	//	//We already have it created, but let's re-send!
	//	//Send a payment request LNWire
	//}
	if h.State > ADD_PRESTAGE {
		l.Unlock()
		return fmt.Errorf("HTLC is already created!")
	}

	if !h.PayToUs { //We created the payment
		//Validate the data
		err = l.validateHTLC(h, false)
		if err != nil {
			l.Unlock()
			return err
		}
		//Update state as pre-commit
		h.State = ADD_PRESTAGE
		if _, err := l.addHTLC(h); err != nil {
			return err
		}
		//Send a ADDHTLC LNWire
		l.Unlock()
		// net() //TODO
	} else {
		//Future version may be able to do this..
		l.Unlock()
		return fmt.Errorf("Cannot pull money")
	}

	return nil
}

//Receives an HTLCAddRequest message
//Adds to the HTLC staging list
func (l *LNChannel) recvHTLCAddRequest(p *lnwire.HTLCAddRequest) error {
	l.Lock()
	var err error
	//Check if we already have a PaymentDescriptor
	if l.HTLCs[p.HTLCKey] != nil {
		//Don't do anything because we already have one
		l.Unlock()
		return fmt.Errorf("Counterparty attempted to re-send AddRequest")
	}
	//Make sure they aren't reusing
	if p.HTLCKey <= l.theirLastKey {
		//They're reusing.... uhoh
		l.Unlock()
		l.sendAddReject(p.HTLCKey) //This may be a dupe!
		return fmt.Errorf("Counterparty cannot re-use HTLCKeys")
	}

	//Update newest id
	l.theirLastKey = p.HTLCKey

	//Create a new HTLCKey entry
	htlc := new(PaymentDescriptor)
	//Populate the entries
	htlc.RHashes = p.RedemptionHashes
	htlc.Timeout = p.Expiry
	htlc.CreditsAmount = p.Amount
	htlc.Blob = p.Blob
	htlc.State = ADD_STAGED //mark as staged by both parties
	htlc.PayToUs = true     //assume this is paid to us, may change in the future

	//Validate the HTLC
	err = l.validateHTLC(htlc, true)
	if err != nil {
		//Update state just in case (not used but y'know..)
		htlc.State = ADD_REJECTED

		//currently not yet added to staging
		//so we don't need to worry about the above htlc
		//we also don't add to disk

		//However, we do need to send a AddReject packet
		l.Unlock()
		l.sendAddReject(p.HTLCKey)

		return err
	}

	//Validation passed, so we continue
	if _, err := l.addHTLC(htlc); err != nil {
		return err
	}

	//Send add accept packet, and we're done
	l.Unlock()
	l.sendAddAccept(p.HTLCKey)
	return nil
}

func (l *LNChannel) sendAddReject(htlckey lnwire.HTLCKey) error {
	l.Lock()
	defer l.Unlock()
	msg := new(lnwire.HTLCAddReject)
	msg.ChannelID = l.channelID
	msg.HTLCKey = htlckey
	net(msg)

	return nil
}

func (l *LNChannel) recvAddReject(htlckey lnwire.HTLCKey) error {
	l.Lock()
	defer l.Unlock()
	htlc := l.HTLCs[htlckey]
	if htlc == nil {
		return fmt.Errorf("Counterparty rejected non-existent HTLC")
	}
	if htlc.State != ADD_PRESTAGE {
		return fmt.Errorf("Counterparty atttempted to reject invalid state")
	}
	htlc.State = ADD_REJECTED
	disk()

	return nil
}

//Notifies the other party that it is now staged on our end
func (l *LNChannel) sendAddAccept(htlckey lnwire.HTLCKey) error {
	htlc := l.HTLCs[htlckey]
	msg := new(lnwire.HTLCAddAccept)
	msg.ChannelID = l.channelID
	msg.HTLCKey = htlckey
	htlc.State = ADD_STAGED

	disk()
	net(msg)

	return nil
}

//The other party has accepted the staging request, so we are staging now
func (l *LNChannel) recvAddAccept(p *lnwire.HTLCAddAccept) error {
	htlc := l.HTLCs[p.HTLCKey]
	//Make sure it's in the list
	if htlc == nil {
		return fmt.Errorf("Counterparty accepted non-existent HTLC")
	}

	//Update pre-stage to staged
	//Everything else it won't do anything
	if htlc.State == ADD_PRESTAGE {
		//Update to staged
		htlc.State = ADD_STAGED
		disk()
	}
	return nil
}

func (l *LNChannel) timeoutHTLC(htlcKey lnwire.HTLCKey) error {
	return nil
}

func (l *LNChannel) settleHTLC(htlcKey lnwire.HTLCKey) error {
	return nil
}

//receive AddAcceptHTLC: Find the HTLC and call createHTLC
func (l *LNChannel) addAccept(h *PaymentDescriptor) error {
	if h.State == ADD_PRESTAGE {
		//Mark stage as accepted
		h.State = ADD_SIGNING_AND_REVOKING
		//Write to disk
		disk()
	}
	return nil
}

//Timeout, Settle

//Create a commitment
//	Update the markings on the HTLCs
func (l *LNChannel) createCommitment() error {
	//Take all staging marked as SIGNING_AND_REVOKING *and* we have not signed
	//	Mark each as weSigned
	return nil
}

//They revoke prior
//After we send a Commitment, they should send a revocation
//	Update shaChain and HTLCs
func (l *LNChannel) receiveRevocation() error {
	//Revoke the prior Commitment
	//Update HTLCs with theyRevoked
	//Check each HTLC for being complete and mark as complete if so
	return nil
}

//Receive a commitment & send out a revocation
//	Update the markings on the HTLCs
func (l *LNChannel) receiveCommitment() error {
	//Validate new Commitment (sigs, even/odd for commitment height, incremental commitment height, etc)
	//	Reconstruct Commitment
	//Update with new Commitment
	//Send revocation
	//Mark as theySignedAndWeRevoked
	//Check each HTLC for being complete and mark as complete if so
	return nil
}

//Mark the HTLC as revoked if it is fully signed and revoked by both parties
func (l *LNChannel) addCompleteHTLC(h *PaymentDescriptor) error {
	//Check/validate values
	//Mark as ADD_COMPLETE
	return nil
}

//Validate whether we want to add the HTLC
func (l *LNChannel) validateHTLC(h *PaymentDescriptor, toUs bool) error {
	//Make sure we have available spots; not too many outputs
	//Make sure there is available funds
	//Make sure there is sufficient reserve for fees
	//Make sure the money is going in the right direction
	return nil
}
