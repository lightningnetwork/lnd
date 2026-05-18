package chanstate

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// forceCloseTxKey points to a the unilateral closing tx that we
	// broadcasted when moving the channel to state CommitBroadcasted.
	forceCloseTxKey = []byte("closing-tx-key")

	// coopCloseTxKey points to a the cooperative closing tx that we
	// broadcasted when moving the channel to state CoopBroadcasted.
	coopCloseTxKey = []byte("coop-closing-tx-key")
)

// ForceCloseTxKey returns the key used to store the unilateral closing
// transaction in a channel bucket.
func ForceCloseTxKey() []byte {
	return forceCloseTxKey
}

// CoopCloseTxKey returns the key used to store the cooperative closing
// transaction in a channel bucket.
func CoopCloseTxKey() []byte {
	return coopCloseTxKey
}

// PutChannelCloseTx stores the closing transaction under the requested key in
// the target channel bucket.
func PutChannelCloseTx(chanBucket kvdb.RwBucket, key []byte,
	closeTx *wire.MsgTx) error {

	var b bytes.Buffer
	if err := closeTx.Serialize(&b); err != nil {
		return err
	}

	return chanBucket.Put(key, b.Bytes())
}

// FetchChannelCloseTx retrieves the closing transaction stored under the
// requested key in the target channel bucket.
func FetchChannelCloseTx(chanBucket kvdb.RBucket,
	key []byte) (*wire.MsgTx, error) {

	bs := chanBucket.Get(key)
	if bs == nil {
		return nil, ErrNoCloseTx
	}

	closeTx := wire.NewMsgTx(2)
	r := bytes.NewReader(bs)
	if err := closeTx.Deserialize(r); err != nil {
		return nil, err
	}

	return closeTx, nil
}

// MarkChannelCommitmentBroadcasted marks the channel as having a commitment
// transaction broadcast.
func (s *KVStore) MarkChannelCommitmentBroadcasted(
	channel *OpenChannel, closeTx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	return s.markBroadcasted(
		channel, ChanStatusCommitBroadcasted, forceCloseTxKey,
		closeTx, closer,
	)
}

// MarkChannelCoopBroadcasted marks the channel as having a cooperative close
// transaction broadcast.
func (s *KVStore) MarkChannelCoopBroadcasted(
	channel *OpenChannel, closeTx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	return s.markBroadcasted(
		channel, ChanStatusCoopBroadcasted, coopCloseTxKey,
		closeTx, closer,
	)
}

// markBroadcasted modifies the channel status and inserts a close transaction
// under the requested key, which should specify either a coop or force close.
// It adds a status which indicates the party that initiated the channel close.
func (s *KVStore) markBroadcasted(channel *OpenChannel,
	status ChannelStatus, key []byte, closeTx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	if closeTx == nil {
		return fmt.Errorf("closeTx must be non-nil")
	}

	channel.Lock()
	defer channel.Unlock()

	putClosingTx := func(chanBucket kvdb.RwBucket) error {
		return PutChannelCloseTx(chanBucket, key, closeTx)
	}

	// Add the initiator status to the status provided. These statuses are
	// set in addition to the broadcast status so that we do not need to
	// migrate the original logic which does not store initiator.
	if closer.IsLocal() {
		status |= ChanStatusLocalCloseInitiator
	} else {
		status |= ChanStatusRemoteCloseInitiator
	}

	return PutChanStatus(s.backend, channel, status, putClosingTx)
}

// FetchChannelBroadcastedCommitment fetches the stored unilateral closing
// transaction.
func (s *KVStore) FetchChannelBroadcastedCommitment(
	channel *OpenChannel) (*wire.MsgTx, error) {

	return s.fetchClosingTx(channel, forceCloseTxKey)
}

// FetchChannelBroadcastedCooperative fetches the stored cooperative closing
// transaction.
func (s *KVStore) FetchChannelBroadcastedCooperative(
	channel *OpenChannel) (*wire.MsgTx, error) {

	return s.fetchClosingTx(channel, coopCloseTxKey)
}

// fetchClosingTx returns the stored closing transaction for key. The caller
// should use either the force or coop closing keys.
func (s *KVStore) fetchClosingTx(channel *OpenChannel,
	key []byte) (*wire.MsgTx, error) {

	var closeTx *wire.MsgTx

	err := kvdb.View(s.backend, func(tx kvdb.RTx) error {
		chanBucket, err := FetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return ErrNoCloseTx
		default:
			return err
		}

		closeTx, err = FetchChannelCloseTx(chanBucket, key)

		return err
	}, func() {
		closeTx = nil
	})
	if err != nil {
		return nil, err
	}

	return closeTx, nil
}
