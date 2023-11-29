package wtclient

import (
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtserver"
)

// DB abstracts the required database operations required by the watchtower
// client.
type DB interface {
	// CreateTower initialize an address record used to communicate with a
	// watchtower. Each Tower is assigned a unique ID, that is used to
	// amortize storage costs of the public key when used by multiple
	// sessions. If the tower already exists, the address is appended to the
	// list of all addresses used to that tower previously and its
	// corresponding sessions are marked as active.
	CreateTower(*lnwire.NetAddress) (*wtdb.Tower, error)

	// RemoveTower modifies a tower's record within the database. If an
	// address is provided, then _only_ the address record should be removed
	// from the tower's persisted state. Otherwise, we'll attempt to mark
	// the tower as inactive. If any of its sessions have unacked updates,
	// then ErrTowerUnackedUpdates is returned. If the tower doesn't have
	// any sessions at all, it'll be completely removed from the database.
	//
	// NOTE: An error is not returned if the tower doesn't exist.
	RemoveTower(*btcec.PublicKey, net.Addr) error

	// TerminateSession sets the given session's status to CSessionTerminal
	// meaning that it will not be usable again.
	TerminateSession(id wtdb.SessionID) error

	// LoadTower retrieves a tower by its public key.
	LoadTower(*btcec.PublicKey) (*wtdb.Tower, error)

	// LoadTowerByID retrieves a tower by its tower ID.
	LoadTowerByID(wtdb.TowerID) (*wtdb.Tower, error)

	// ListTowers retrieves the list of towers available within the
	// database. The filter function may be set in order to filter out the
	// towers to be returned.
	ListTowers(filter wtdb.TowerFilterFn) ([]*wtdb.Tower, error)

	// NextSessionKeyIndex reserves a new session key derivation index for a
	// particular tower id and blob type. The index is reserved for that
	// (tower, blob type) pair until CreateClientSession is invoked for that
	// tower and index, at which point a new index for that tower can be
	// reserved. Multiple calls to this method before CreateClientSession is
	// invoked should return the same index unless forceNext is true.
	NextSessionKeyIndex(wtdb.TowerID, blob.Type, bool) (uint32, error)

	// CreateClientSession saves a newly negotiated client session to the
	// client's database. This enables the session to be used across
	// restarts.
	CreateClientSession(*wtdb.ClientSession) error

	// ListClientSessions returns the set of all client sessions known to
	// the db. An optional tower ID can be used to filter out any client
	// sessions in the response that do not correspond to this tower.
	ListClientSessions(*wtdb.TowerID, ...wtdb.ClientSessionListOption) (
		map[wtdb.SessionID]*wtdb.ClientSession, error)

	// GetClientSession loads the ClientSession with the given ID from the
	// DB.
	GetClientSession(wtdb.SessionID,
		...wtdb.ClientSessionListOption) (*wtdb.ClientSession, error)

	// FetchSessionCommittedUpdates retrieves the current set of un-acked
	// updates of the given session.
	FetchSessionCommittedUpdates(id *wtdb.SessionID) (
		[]wtdb.CommittedUpdate, error)

	// IsAcked returns true if the given backup has been backed up using
	// the given session.
	IsAcked(id *wtdb.SessionID, backupID *wtdb.BackupID) (bool, error)

	// NumAckedUpdates returns the number of backups that have been
	// successfully backed up using the given session.
	NumAckedUpdates(id *wtdb.SessionID) (uint64, error)

	// FetchChanInfos loads a mapping from all registered channels to
	// their wtdb.ChannelInfo. Only the channels that have not yet been
	// marked as closed will be loaded.
	FetchChanInfos() (wtdb.ChannelInfos, error)

	// MarkChannelClosed will mark a registered channel as closed by setting
	// its closed-height as the given block height. It returns a list of
	// session IDs for sessions that are now considered closable due to the
	// close of this channel. The details for this channel will be deleted
	// from the DB if there are no more sessions in the DB that contain
	// updates for this channel.
	MarkChannelClosed(chanID lnwire.ChannelID, blockHeight uint32) (
		[]wtdb.SessionID, error)

	// ListClosableSessions fetches and returns the IDs for all sessions
	// marked as closable.
	ListClosableSessions() (map[wtdb.SessionID]uint32, error)

	// DeleteSession can be called when a session should be deleted from the
	// DB. All references to the session will also be deleted from the DB.
	// A session will only be deleted if it was previously marked as
	// closable.
	DeleteSession(id wtdb.SessionID) error

	// RegisterChannel registers a channel for use within the client
	// database. For now, all that is stored in the channel summary is the
	// sweep pkscript that we'd like any tower sweeps to pay into. In the
	// future, this will be extended to contain more info to allow the
	// client efficiently request historical states to be backed up under
	// the client's active policy.
	RegisterChannel(lnwire.ChannelID, []byte) error

	// MarkBackupIneligible records that the state identified by the
	// (channel id, commit height) tuple was ineligible for being backed up
	// under the current policy. This state can be retried later under a
	// different policy.
	MarkBackupIneligible(chanID lnwire.ChannelID, commitHeight uint64) error

	// CommitUpdate writes the next state update for a particular
	// session, so that we can be sure to resend it after a restart if it
	// hasn't been ACK'd by the tower. The sequence number of the update
	// should be exactly one greater than the existing entry, and less that
	// or equal to the session's MaxUpdates.
	CommitUpdate(id *wtdb.SessionID,
		update *wtdb.CommittedUpdate) (uint16, error)

	// AckUpdate records an acknowledgment from the watchtower that the
	// update identified by seqNum was received and saved. The returned
	// lastApplied will be recorded.
	AckUpdate(id *wtdb.SessionID, seqNum, lastApplied uint16) error

	// GetDBQueue returns a BackupID Queue instance under the given name
	// space.
	GetDBQueue(namespace []byte) wtdb.Queue[*wtdb.BackupID]

	// DeleteCommittedUpdates deletes all the committed updates belonging to
	// the given session from the db.
	DeleteCommittedUpdates(id *wtdb.SessionID) error

	// DeactivateTower sets the given tower's status to inactive. This means
	// that this tower's sessions won't be loaded and used for backups.
	// CreateTower can be used to reactivate the tower again.
	DeactivateTower(pubKey *btcec.PublicKey) error
}

// AuthDialer connects to a remote node using an authenticated transport, such
// as brontide. The dialer argument is used to specify a resolver, which allows
// this method to be used over Tor or clear net connections.
type AuthDialer func(localKey keychain.SingleKeyECDH,
	netAddr *lnwire.NetAddress,
	dialer tor.DialFunc) (wtserver.Peer, error)

// ECDHKeyRing abstracts the ability to derive shared ECDH keys given a
// description of the derivation path of a private key.
type ECDHKeyRing interface {
	keychain.ECDHRing

	// DeriveKey attempts to derive an arbitrary key specified by the
	// passed KeyLocator. This may be used in several recovery scenarios,
	// or when manually rotating something like our current default node
	// key.
	DeriveKey(keyLoc keychain.KeyLocator) (keychain.KeyDescriptor, error)
}

// Tower represents the info about a watchtower server that a watchtower client
// needs in order to connect to it.
type Tower struct {
	// ID is the unique, db-assigned, identifier for this tower.
	ID wtdb.TowerID

	// IdentityKey is the public key of the remote node, used to
	// authenticate the brontide transport.
	IdentityKey *btcec.PublicKey

	// Addresses is an AddressIterator that can be used to manage the
	// addresses for this tower.
	Addresses AddressIterator
}

// NewTowerFromDBTower converts a wtdb.Tower, which uses a static address list,
// into a Tower which uses an address iterator.
func NewTowerFromDBTower(t *wtdb.Tower) (*Tower, error) {
	addrs, err := newAddressIterator(t.Addresses...)
	if err != nil {
		return nil, err
	}

	return &Tower{
		ID:          t.ID,
		IdentityKey: t.IdentityKey,
		Addresses:   addrs,
	}, nil
}

// ClientSession represents the session that a tower client has with a server.
type ClientSession struct {
	// ID is the client's public key used when authenticating with the
	// tower.
	ID wtdb.SessionID

	wtdb.ClientSessionBody

	// Tower represents the tower that the client session has been made
	// with.
	Tower *Tower

	// SessionKeyECDH is the ECDH capable wrapper of the ephemeral secret
	// key used to connect to the watchtower.
	SessionKeyECDH keychain.SingleKeyECDH
}

// NewClientSessionFromDBSession converts a wtdb.ClientSession to a
// ClientSession.
func NewClientSessionFromDBSession(s *wtdb.ClientSession, tower *Tower,
	keyRing ECDHKeyRing) (*ClientSession, error) {

	towerKeyDesc, err := keyRing.DeriveKey(
		keychain.KeyLocator{
			Family: keychain.KeyFamilyTowerSession,
			Index:  s.KeyIndex,
		},
	)
	if err != nil {
		return nil, err
	}

	sessionKeyECDH := keychain.NewPubKeyECDH(
		towerKeyDesc, keyRing,
	)

	return &ClientSession{
		ID:                s.ID,
		ClientSessionBody: s.ClientSessionBody,
		Tower:             tower,
		SessionKeyECDH:    sessionKeyECDH,
	}, nil
}
