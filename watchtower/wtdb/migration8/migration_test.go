package migration8

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

const (
	chan1ID = 10
	chan2ID = 20
	chan3ID = 30
	chan4ID = 40

	chan1DBID = 111
	chan2DBID = 222
	chan3DBID = 333
)

var (
	// preDetails is the expected data of the channel details bucket before
	// the migration.
	preDetails = map[string]interface{}{
		channelIDString(chan1ID): map[string]interface{}{},
		channelIDString(chan2ID): map[string]interface{}{},
		channelIDString(chan3ID): map[string]interface{}{},
	}

	// channelIDIndex is the data in the channelID index that is used to
	// find the mapping between the db-assigned channel ID and the real
	// channel ID.
	channelIDIndex = map[string]interface{}{
		uint64ToStr(chan1DBID): channelIDString(chan1ID),
		uint64ToStr(chan2DBID): channelIDString(chan2ID),
		uint64ToStr(chan3DBID): channelIDString(chan3ID),
	}

	// postDetails is the expected data in the channel details bucket after
	// the migration.
	postDetails = map[string]interface{}{
		channelIDString(chan1ID): map[string]interface{}{
			string(cChanMaxCommitmentHeight): uint64ToStr(105),
		},
		channelIDString(chan2ID): map[string]interface{}{
			string(cChanMaxCommitmentHeight): uint64ToStr(205),
		},
		channelIDString(chan3ID): map[string]interface{}{
			string(cChanMaxCommitmentHeight): uint64ToStr(304),
		},
	}
)

// TestMigrateChannelToSessionIndex tests that the MigrateChannelToSessionIndex
// function correctly builds the new channel-to-sessionID index to the tower
// client DB.
func TestMigrateChannelToSessionIndex(t *testing.T) {
	t.Parallel()

	update1 := &CommittedUpdate{
		SeqNum: 1,
		CommittedUpdateBody: CommittedUpdateBody{
			BackupID: BackupID{
				ChanID:       intToChannelID(chan1ID),
				CommitHeight: 105,
			},
		},
	}
	var update1B bytes.Buffer
	require.NoError(t, update1.Encode(&update1B))

	update3 := &CommittedUpdate{
		SeqNum: 1,
		CommittedUpdateBody: CommittedUpdateBody{
			BackupID: BackupID{
				ChanID:       intToChannelID(chan3ID),
				CommitHeight: 304,
			},
		},
	}
	var update3B bytes.Buffer
	require.NoError(t, update3.Encode(&update3B))

	update4 := &CommittedUpdate{
		SeqNum: 1,
		CommittedUpdateBody: CommittedUpdateBody{
			BackupID: BackupID{
				ChanID:       intToChannelID(chan4ID),
				CommitHeight: 400,
			},
		},
	}
	var update4B bytes.Buffer
	require.NoError(t, update4.Encode(&update4B))

	// sessions is the expected data in the sessions bucket before and
	// after the migration.
	sessions := map[string]interface{}{
		// A session with both acked and committed updates.
		sessionIDString("1"): map[string]interface{}{
			string(cSessionAckRangeIndex): map[string]interface{}{
				// This range index gives channel 1 a max height
				// of 104.
				uint64ToStr(chan1DBID): map[string]interface{}{
					uint64ToStr(100): uint64ToStr(101),
					uint64ToStr(104): uint64ToStr(104),
				},
				// This range index gives channel 2 a max height
				// of 200.
				uint64ToStr(chan2DBID): map[string]interface{}{
					uint64ToStr(200): uint64ToStr(200),
				},
			},
			string(cSessionCommits): map[string]interface{}{
				// This committed update gives channel 1 a max
				// height of 105 and so it overrides the heights
				// from the range index.
				uint64ToStr(1): update1B.String(),
			},
		},
		// A session with only acked updates.
		sessionIDString("2"): map[string]interface{}{
			string(cSessionAckRangeIndex): map[string]interface{}{
				// This range index gives channel 2 a max height
				// of 205.
				uint64ToStr(chan2DBID): map[string]interface{}{
					uint64ToStr(201): uint64ToStr(205),
				},
			},
		},
		// A session with only committed updates.
		sessionIDString("3"): map[string]interface{}{
			string(cSessionCommits): map[string]interface{}{
				// This committed update gives channel 3 a max
				// height of 304.
				uint64ToStr(1): update3B.String(),
			},
		},
		// This session only contains heights for channel 4 which has
		// been closed and so this should have no effect.
		sessionIDString("4"): map[string]interface{}{
			string(cSessionAckRangeIndex): map[string]interface{}{
				uint64ToStr(444): map[string]interface{}{
					uint64ToStr(400): uint64ToStr(402),
					uint64ToStr(403): uint64ToStr(405),
				},
			},
			string(cSessionCommits): map[string]interface{}{
				uint64ToStr(1): update4B.String(),
			},
		},
		// A session with no updates.
		sessionIDString("5"): map[string]interface{}{},
	}

	// Before the migration we have a channel details
	// bucket, a sessions bucket, a session ID index bucket
	// and a channel ID index bucket.
	before := func(tx kvdb.RwTx) error {
		err := migtest.RestoreDB(tx, cChanDetailsBkt, preDetails)
		if err != nil {
			return err
		}

		err = migtest.RestoreDB(tx, cSessionBkt, sessions)
		if err != nil {
			return err
		}

		return migtest.RestoreDB(tx, cChanIDIndexBkt, channelIDIndex)
	}

	after := func(tx kvdb.RwTx) error {
		err := migtest.VerifyDB(tx, cSessionBkt, sessions)
		if err != nil {
			return err
		}

		return migtest.VerifyDB(tx, cChanDetailsBkt, postDetails)
	}

	migtest.ApplyMigration(
		t, before, after, MigrateChannelMaxHeights, false,
	)
}

func sessionIDString(id string) string {
	var sessID SessionID
	copy(sessID[:], id)
	return sessID.String()
}

func channelIDString(id uint64) string {
	var chanID ChannelID
	byteOrder.PutUint64(chanID[:], id)
	return string(chanID[:])
}

func uint64ToStr(id uint64) string {
	b, err := writeBigSize(id)
	if err != nil {
		panic(err)
	}

	return string(b)
}

func intToChannelID(id uint64) ChannelID {
	var chanID ChannelID
	byteOrder.PutUint64(chanID[:], id)
	return chanID
}
