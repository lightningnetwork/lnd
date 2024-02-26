package migration27

import (
	"bytes"
	"fmt"

	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	mig25 "github.com/lightningnetwork/lnd/channeldb/migration25"
	mig26 "github.com/lightningnetwork/lnd/channeldb/migration26"
	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// A tlv type definition used to serialize and deserialize a KeyLocator
	// from the database.
	keyLocType tlv.Type = 1

	// A tlv type used to serialize and deserialize the
	// `InitialLocalBalance` field.
	initialLocalBalanceType tlv.Type = 2

	// A tlv type used to serialize and deserialize the
	// `InitialRemoteBalance` field.
	initialRemoteBalanceType tlv.Type = 3
)

var (
	// chanInfoKey can be accessed within the bucket for a channel
	// (identified by its chanPoint). This key stores all the static
	// information for a channel which is decided at the end of  the
	// funding flow.
	chanInfoKey = []byte("chan-info-key")

	// localUpfrontShutdownKey can be accessed within the bucket for a
	// channel (identified by its chanPoint). This key stores an optional
	// upfront shutdown script for the local peer.
	localUpfrontShutdownKey = []byte("local-upfront-shutdown-key")

	// remoteUpfrontShutdownKey can be accessed within the bucket for a
	// channel (identified by its chanPoint). This key stores an optional
	// upfront shutdown script for the remote peer.
	remoteUpfrontShutdownKey = []byte("remote-upfront-shutdown-key")

	// lastWasRevokeKey is a key that stores true when the last update we
	// sent was a revocation and false when it was a commitment signature.
	// This is nil in the case of new channels with no updates exchanged.
	lastWasRevokeKey = []byte("last-was-revoke")

	// ErrNoChanInfoFound is returned when a particular channel does not
	// have any channels state.
	ErrNoChanInfoFound = fmt.Errorf("no chan info found")
)

// OpenChannel embeds a mig26.OpenChannel with the extra update-to-date
// serialization and deserialization methods.
//
// NOTE: doesn't have the Packager field as it's not used in current migration.
type OpenChannel struct {
	mig26.OpenChannel
}

// FetchChanInfo deserializes the channel info based on the legacy boolean.
func FetchChanInfo(chanBucket kvdb.RBucket, c *OpenChannel, legacy bool) error {
	infoBytes := chanBucket.Get(chanInfoKey)
	if infoBytes == nil {
		return ErrNoChanInfoFound
	}
	r := bytes.NewReader(infoBytes)

	var (
		chanType   mig.ChannelType
		chanStatus mig.ChannelStatus
	)

	if err := mig.ReadElements(r,
		&chanType, &c.ChainHash, &c.FundingOutpoint,
		&c.ShortChannelID, &c.IsPending, &c.IsInitiator,
		&chanStatus, &c.FundingBroadcastHeight,
		&c.NumConfsRequired, &c.ChannelFlags,
		&c.IdentityPub, &c.Capacity, &c.TotalMSatSent,
		&c.TotalMSatReceived,
	); err != nil {
		return fmt.Errorf("ReadElements got: %w", err)
	}

	c.ChanType = mig25.ChannelType(chanType)
	c.ChanStatus = mig25.ChannelStatus(chanStatus)

	// For single funder channels that we initiated and have the funding
	// transaction to, read the funding txn.
	if c.FundingTxPresent() {
		if err := mig.ReadElement(r, &c.FundingTxn); err != nil {
			return fmt.Errorf("read FundingTxn got: %w", err)
		}
	}

	if err := mig.ReadChanConfig(r, &c.LocalChanCfg); err != nil {
		return fmt.Errorf("read LocalChanCfg got: %w", err)
	}
	if err := mig.ReadChanConfig(r, &c.RemoteChanCfg); err != nil {
		return fmt.Errorf("read RemoteChanCfg got: %w", err)
	}

	// Retrieve the boolean stored under lastWasRevokeKey.
	lastWasRevokeBytes := chanBucket.Get(lastWasRevokeKey)
	if lastWasRevokeBytes == nil {
		// If nothing has been stored under this key, we store false in
		// the OpenChannel struct.
		c.LastWasRevoke = false
	} else {
		// Otherwise, read the value into the LastWasRevoke field.
		revokeReader := bytes.NewReader(lastWasRevokeBytes)
		err := mig.ReadElements(revokeReader, &c.LastWasRevoke)
		if err != nil {
			return fmt.Errorf("read LastWasRevoke got: %w", err)
		}
	}

	// Make the tlv stream based on the legacy param.
	var (
		ts            *tlv.Stream
		err           error
		localBalance  uint64
		remoteBalance uint64
	)

	keyLocRecord := mig25.MakeKeyLocRecord(
		keyLocType, &c.RevocationKeyLocator,
	)

	// If it's legacy, create the stream with a single tlv record.
	if legacy {
		ts, err = tlv.NewStream(keyLocRecord)
	} else {
		// Otherwise, for the new format, we will encode the balance
		// fields in the tlv stream too.
		ts, err = tlv.NewStream(
			keyLocRecord,
			tlv.MakePrimitiveRecord(
				initialLocalBalanceType, &localBalance,
			),
			tlv.MakePrimitiveRecord(
				initialRemoteBalanceType, &remoteBalance,
			),
		)
	}
	if err != nil {
		return fmt.Errorf("create tlv stream got: %w", err)
	}

	if err := ts.Decode(r); err != nil {
		return fmt.Errorf("decode tlv stream got: %w", err)
	}

	// For the new format, attach the balance fields.
	if !legacy {
		c.InitialLocalBalance = lnwire.MilliSatoshi(localBalance)
		c.InitialRemoteBalance = lnwire.MilliSatoshi(remoteBalance)
	}

	// Finally, read the optional shutdown scripts.
	if err := mig25.GetOptionalUpfrontShutdownScript(
		chanBucket, localUpfrontShutdownKey, &c.LocalShutdownScript,
	); err != nil {
		return fmt.Errorf("local shutdown script got: %w", err)
	}

	return mig25.GetOptionalUpfrontShutdownScript(
		chanBucket, remoteUpfrontShutdownKey, &c.RemoteShutdownScript,
	)
}

// PutChanInfo serializes the channel info based on the legacy boolean.
func PutChanInfo(chanBucket kvdb.RwBucket, c *OpenChannel, legacy bool) error {
	var w bytes.Buffer
	if err := mig.WriteElements(&w,
		mig.ChannelType(c.ChanType), c.ChainHash, c.FundingOutpoint,
		c.ShortChannelID, c.IsPending, c.IsInitiator,
		mig.ChannelStatus(c.ChanStatus), c.FundingBroadcastHeight,
		c.NumConfsRequired, c.ChannelFlags,
		c.IdentityPub, c.Capacity, c.TotalMSatSent,
		c.TotalMSatReceived,
	); err != nil {
		return err
	}

	// For single funder channels that we initiated, and we have the
	// funding transaction, then write the funding txn.
	if c.FundingTxPresent() {
		if err := mig.WriteElement(&w, c.FundingTxn); err != nil {
			return err
		}
	}

	if err := mig.WriteChanConfig(&w, &c.LocalChanCfg); err != nil {
		return err
	}
	if err := mig.WriteChanConfig(&w, &c.RemoteChanCfg); err != nil {
		return err
	}

	// Make the tlv stream based on the legacy param.
	tlvStream, err := mig26.MakeTlvStream(&c.OpenChannel, legacy)
	if err != nil {
		return err
	}

	if err := tlvStream.Encode(&w); err != nil {
		return err
	}

	if err := chanBucket.Put(chanInfoKey, w.Bytes()); err != nil {
		return err
	}

	// Finally, add optional shutdown scripts for the local and remote peer
	// if they are present.
	if err := mig25.PutOptionalUpfrontShutdownScript(
		chanBucket, localUpfrontShutdownKey, c.LocalShutdownScript,
	); err != nil {
		return err
	}

	return mig25.PutOptionalUpfrontShutdownScript(
		chanBucket, remoteUpfrontShutdownKey, c.RemoteShutdownScript,
	)
}
