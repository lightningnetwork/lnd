//go:build test_db_postgres || test_db_sqlite

package migration1

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/graph/db/migration1/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// TestMigrateGraphToSQLFeatureEncodings checks that legacy channel records
// migrate alongside records containing length-prefixed features. The legacy
// records are written directly because the current KV writer adds the prefix.
func TestMigrateGraphToSQLFeatureEncodings(t *testing.T) {
	t.Parallel()

	dbFixture := NewTestDBFixture(t)
	tests := []struct {
		name string
		bits []lnwire.FeatureBit
	}{
		{
			name: "empty",
		},
		{
			name: "single byte",
			bits: []lnwire.FeatureBit{7},
		},
		{
			name: "multiple bytes",
			bits: []lnwire.FeatureBit{0, 15},
		},
		{
			// The legacy payload starts with 0x01, 0x00 and is
			// 258 bytes long, so a length check alone would mistake
			// it for a length-prefixed vector of 256 zero bytes.
			name: "legacy length prefix collision",
			bits: []lnwire.FeatureBit{2056},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			kv := setUpKVStore(t)
			store := NewTestDBWithFixture(t, dbFixture)
			sql, ok := store.(*SQLStore)
			require.True(t, ok)
			features := lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(test.bits...),
				lnwire.Features,
			)

			// Create two channels with the same features and both
			// policies. Only the first channel will be rewritten in
			// the legacy format, leaving a mix of encodings.
			var legacyChannel *models.ChannelEdgeInfo
			for i := range 2 {
				channel := makeTestChannel(
					t, func(c *models.ChannelEdgeInfo) {
						c.ChannelID = uint64(i + 1)
						c.Features = features
					},
				)
				err := kv.AddChannelEdge(ctx, channel)
				require.NoError(t, err)

				for _, isNode1 := range []bool{true, false} {
					toNode := channel.NodeKey1Bytes
					if isNode1 {
						toNode = channel.NodeKey2Bytes
					}
					policy := makeTestPolicy(
						channel.ChannelID, toNode,
						isNode1,
					)
					_, _, err := kv.UpdateEdgePolicy(
						ctx, policy,
					)
					require.NoError(t, err)
				}

				if i == 0 {
					legacyChannel = channel
				}
			}

			// Capture the expected state before rewriting features.
			// This does not depend on legacy decoding working.
			expected := fetchAllChannelsAndPolicies(t, kv)
			err := kvdb.Update(kv.db, func(tx kvdb.RwTx) error {
				index := tx.ReadWriteBucket(edgeBucket).
					NestedReadWriteBucket(edgeIndexBucket)
				var key [8]byte
				byteOrder.PutUint64(
					key[:], legacyChannel.ChannelID,
				)
				record := index.Get(key[:])
				require.NotEmpty(t, record)

				// Four public keys precede the feature VarBytes
				// field. Keep all other record bytes intact.
				const keysLen = 4 * 33
				reader := bytes.NewReader(record[keysLen:])
				_, err := wire.ReadVarBytes(
					reader, 0, 900, "features",
				)
				if err != nil {
					return err
				}
				tail := record[len(record)-reader.Len():]

				var featureBuf, rewritten bytes.Buffer
				err = features.EncodeBase256(&featureBuf)
				if err != nil {
					return err
				}
				rewritten.Write(record[:keysLen])
				err = wire.WriteVarBytes(
					&rewritten, 0, featureBuf.Bytes(),
				)
				if err != nil {
					return err
				}
				rewritten.Write(tail)
				return index.Put(key[:], rewritten.Bytes())
			}, func() {})
			require.NoError(t, err)

			err = sql.db.ExecTx(ctx, sqldb.WriteTxOpt(),
				func(tx SQLQueries) error {
					return MigrateGraphToSQL(
						ctx, sql.cfg, kv.db, tx,
					)
				}, sqldb.NoOpReset,
			)
			require.NoError(t, err)
			actual := fetchAllChannelsAndPolicies(t, sql)
			require.Equal(t, expected, actual)
		})
	}
}
