package migration26

import (
	"bytes"
	"fmt"

	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	mig25 "github.com/lightningnetwork/lnd/channeldb/migration25"
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

	// ErrNoPastDeltas is returned when the channel delta bucket hasn't been
	// created.
	ErrNoPastDeltas = fmt.Errorf("channel has no recorded deltas")

	// ErrLogEntryNotFound is returned when we cannot find a log entry at
	// the height requested in the revocation log.
	ErrLogEntryNotFound = fmt.Errorf("log entry not found")

	// ErrNoCommitmentsFound is returned when a channel has not set
	// commitment states.
	ErrNoCommitmentsFound = fmt.Errorf("no commitments found")
)

// OpenChannel embeds a mig25.OpenChannel with the extra update-to-date
// serialization and deserialization methods.
//
// NOTE: doesn't have the Packager field as it's not used in current migration.
type OpenChannel struct {
	mig25.OpenChannel
}

// FetchChanInfo deserializes the channel info based on the legacy boolean.
// After migration25, the legacy format would have the fields
// `InitialLocalBalance` and `InitialRemoteBalance` directly encoded as bytes.
// For the new format, they will be put inside a tlv stream.
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
		return err
	}

	c.ChanType = mig25.ChannelType(chanType)
	c.ChanStatus = mig25.ChannelStatus(chanStatus)

	// If this is the legacy format, we need to read the extra two new
	// fields.
	if legacy {
		if err := mig.ReadElements(r,
			&c.InitialLocalBalance, &c.InitialRemoteBalance,
		); err != nil {
			return err
		}
	}

	// For single funder channels that we initiated and have the funding
	// transaction to, read the funding txn.
	if c.FundingTxPresent() {
		if err := mig.ReadElement(r, &c.FundingTxn); err != nil {
			return err
		}
	}

	if err := mig.ReadChanConfig(r, &c.LocalChanCfg); err != nil {
		return err
	}
	if err := mig.ReadChanConfig(r, &c.RemoteChanCfg); err != nil {
		return err
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
			return err
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
		return err
	}

	if err := ts.Decode(r); err != nil {
		return err
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
		return err
	}

	return mig25.GetOptionalUpfrontShutdownScript(
		chanBucket, remoteUpfrontShutdownKey, &c.RemoteShutdownScript,
	)
}

// MakeTlvStream creates a tlv stream based on whether we are deadling with
// legacy format or not. For the legacy format, we have a single record in the
// stream. For the new format, we have the extra balance records.
func MakeTlvStream(c *OpenChannel, legacy bool) (*tlv.Stream, error) {
	keyLocRecord := mig25.MakeKeyLocRecord(
		keyLocType, &c.RevocationKeyLocator,
	)

	// If it's legacy, return the stream with a single tlv record.
	if legacy {
		return tlv.NewStream(keyLocRecord)
	}

	// Otherwise, for the new format, we will encode the balance fields in
	// the tlv stream too.
	localBalance := uint64(c.InitialLocalBalance)
	remoteBalance := uint64(c.InitialRemoteBalance)

	// Create the tlv stream.
	return tlv.NewStream(
		keyLocRecord,
		tlv.MakePrimitiveRecord(
			initialLocalBalanceType, &localBalance,
		),
		tlv.MakePrimitiveRecord(
			initialRemoteBalanceType, &remoteBalance,
		),
	)
}

// PutChanInfo serializes the channel info based on the legacy boolean. After
// migration25, the legacy format would have the fields `InitialLocalBalance`
// and `InitialRemoteBalance` directly encoded as bytes. For the new format,
// they will be put inside a tlv stream.
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

	// If this is legacy format, we need to write the extra two fields.
	if legacy {
		if err := mig.WriteElements(&w,
			c.InitialLocalBalance, c.InitialRemoteBalance,
		); err != nil {
			return err
		}
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
	tlvStream, err := MakeTlvStream(c, legacy)
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
