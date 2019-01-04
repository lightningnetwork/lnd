package channeldb

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/coreos/bbolt"
)

// migrateNodeAndEdgeUpdateIndex is a migration function that will update the
// database from version 0 to version 1. In version 1, we add two new indexes
// (one for nodes and one for edges) to keep track of the last time a node or
// edge was updated on the network. These new indexes allow us to implement the
// new graph sync protocol added.
func migrateNodeAndEdgeUpdateIndex(tx *bbolt.Tx) error {
	// First, we'll populating the node portion of the new index. Before we
	// can add new values to the index, we'll first create the new bucket
	// where these items will be housed.
	nodes, err := tx.CreateBucketIfNotExists(nodeBucket)
	if err != nil {
		return fmt.Errorf("unable to create node bucket: %v", err)
	}
	nodeUpdateIndex, err := nodes.CreateBucketIfNotExists(
		nodeUpdateIndexBucket,
	)
	if err != nil {
		return fmt.Errorf("unable to create node update index: %v", err)
	}

	log.Infof("Populating new node update index bucket")

	// Now that we know the bucket has been created, we'll iterate over the
	// entire node bucket so we can add the (updateTime || nodePub) key
	// into the node update index.
	err = nodes.ForEach(func(nodePub, nodeInfo []byte) error {
		if len(nodePub) != 33 {
			return nil
		}

		log.Tracef("Adding %x to node update index", nodePub)

		// The first 8 bytes of a node's serialize data is the update
		// time, so we can extract that without decoding the entire
		// structure.
		updateTime := nodeInfo[:8]

		// Now that we have the update time, we can construct the key
		// to insert into the index.
		var indexKey [8 + 33]byte
		copy(indexKey[:8], updateTime)
		copy(indexKey[8:], nodePub)

		return nodeUpdateIndex.Put(indexKey[:], nil)
	})
	if err != nil {
		return fmt.Errorf("unable to update node indexes: %v", err)
	}

	log.Infof("Populating new edge update index bucket")

	// With the set of nodes updated, we'll now update all edges to have a
	// corresponding entry in the edge update index.
	edges, err := tx.CreateBucketIfNotExists(edgeBucket)
	if err != nil {
		return fmt.Errorf("unable to create edge bucket: %v", err)
	}
	edgeUpdateIndex, err := edges.CreateBucketIfNotExists(
		edgeUpdateIndexBucket,
	)
	if err != nil {
		return fmt.Errorf("unable to create edge update index: %v", err)
	}

	// We'll now run through each edge policy in the database, and update
	// the index to ensure each edge has the proper record.
	err = edges.ForEach(func(edgeKey, edgePolicyBytes []byte) error {
		if len(edgeKey) != 41 {
			return nil
		}

		// Now that we know this is the proper record, we'll grab the
		// channel ID (last 8 bytes of the key), and then decode the
		// edge policy so we can access the update time.
		chanID := edgeKey[33:]
		edgePolicyReader := bytes.NewReader(edgePolicyBytes)

		edgePolicy, err := deserializeChanEdgePolicy(
			edgePolicyReader, nodes,
		)
		if err != nil {
			return err
		}

		log.Tracef("Adding chan_id=%v to edge update index",
			edgePolicy.ChannelID)

		// We'll now construct the index key using the channel ID, and
		// the last time it was updated: (updateTime || chanID).
		var indexKey [8 + 8]byte
		byteOrder.PutUint64(
			indexKey[:], uint64(edgePolicy.LastUpdate.Unix()),
		)
		copy(indexKey[8:], chanID)

		return edgeUpdateIndex.Put(indexKey[:], nil)
	})
	if err != nil {
		return fmt.Errorf("unable to update edge indexes: %v", err)
	}

	log.Infof("Migration to node and edge update indexes complete!")

	return nil
}

// migrateInvoiceTimeSeries is a database migration that assigns all existing
// invoices an index in the add and/or the settle index. Additionally, all
// existing invoices will have their bytes padded out in order to encode the
// add+settle index as well as the amount paid.
func migrateInvoiceTimeSeries(tx *bbolt.Tx) error {
	invoices, err := tx.CreateBucketIfNotExists(invoiceBucket)
	if err != nil {
		return err
	}

	addIndex, err := invoices.CreateBucketIfNotExists(
		addIndexBucket,
	)
	if err != nil {
		return err
	}
	settleIndex, err := invoices.CreateBucketIfNotExists(
		settleIndexBucket,
	)
	if err != nil {
		return err
	}

	log.Infof("Migrating invoice database to new time series format")

	// Now that we have all the buckets we need, we'll run through each
	// invoice in the database, and update it to reflect the new format
	// expected post migration.
	err = invoices.ForEach(func(invoiceNum, invoiceBytes []byte) error {
		// If this is a sub bucket, then we'll skip it.
		if invoiceBytes == nil {
			return nil
		}

		// First, we'll make a copy of the encoded invoice bytes.
		invoiceBytesCopy := make([]byte, len(invoiceBytes))
		copy(invoiceBytesCopy, invoiceBytes)

		// With the bytes copied over, we'll append 24 additional
		// bytes. We do this so we can decode the invoice under the new
		// serialization format.
		padding := bytes.Repeat([]byte{0}, 24)
		invoiceBytesCopy = append(invoiceBytesCopy, padding...)

		invoiceReader := bytes.NewReader(invoiceBytesCopy)
		invoice, err := deserializeInvoice(invoiceReader)
		if err != nil {
			return fmt.Errorf("unable to decode invoice: %v", err)
		}

		// Now that we have the fully decoded invoice, we can update
		// the various indexes that we're added, and finally the
		// invoice itself before re-inserting it.

		// First, we'll get the new sequence in the addIndex in order
		// to create the proper mapping.
		nextAddSeqNo, err := addIndex.NextSequence()
		if err != nil {
			return err
		}
		var seqNoBytes [8]byte
		byteOrder.PutUint64(seqNoBytes[:], nextAddSeqNo)
		err = addIndex.Put(seqNoBytes[:], invoiceNum[:])
		if err != nil {
			return err
		}

		log.Tracef("Adding invoice (preimage=%x, add_index=%v) to add "+
			"time series", invoice.Terms.PaymentPreimage[:],
			nextAddSeqNo)

		// Next, we'll check if the invoice has been settled or not. If
		// so, then we'll also add it to the settle index.
		var nextSettleSeqNo uint64
		if invoice.Terms.State == ContractSettled {
			nextSettleSeqNo, err = settleIndex.NextSequence()
			if err != nil {
				return err
			}

			var seqNoBytes [8]byte
			byteOrder.PutUint64(seqNoBytes[:], nextSettleSeqNo)
			err := settleIndex.Put(seqNoBytes[:], invoiceNum)
			if err != nil {
				return err
			}

			invoice.AmtPaid = invoice.Terms.Value

			log.Tracef("Adding invoice (preimage=%x, "+
				"settle_index=%v) to add time series",
				invoice.Terms.PaymentPreimage[:],
				nextSettleSeqNo)
		}

		// Finally, we'll update the invoice itself with the new
		// indexing information as well as the amount paid if it has
		// been settled or not.
		invoice.AddIndex = nextAddSeqNo
		invoice.SettleIndex = nextSettleSeqNo

		// We've fully migrated an invoice, so we'll now update the
		// invoice in-place.
		var b bytes.Buffer
		if err := serializeInvoice(&b, &invoice); err != nil {
			return err
		}

		return invoices.Put(invoiceNum, b.Bytes())
	})
	if err != nil {
		return err
	}

	log.Infof("Migration to invoice time series index complete!")

	return nil
}

// migrateInvoiceTimeSeriesOutgoingPayments is a follow up to the
// migrateInvoiceTimeSeries migration. As at the time of writing, the
// OutgoingPayment struct embeddeds an instance of the Invoice struct. As a
// result, we also need to migrate the internal invoice to the new format.
func migrateInvoiceTimeSeriesOutgoingPayments(tx *bbolt.Tx) error {
	payBucket := tx.Bucket(paymentBucket)
	if payBucket == nil {
		return nil
	}

	log.Infof("Migrating invoice database to new outgoing payment format")

	err := payBucket.ForEach(func(payID, paymentBytes []byte) error {
		log.Tracef("Migrating payment %x", payID[:])

		// The internal invoices for each payment only contain a
		// populated contract term, and creation date, as a result,
		// most of the bytes will be "empty".

		// We'll calculate the end of the invoice index assuming a
		// "minimal" index that's embedded within the greater
		// OutgoingPayment. The breakdown is:
		//  3 bytes empty var bytes, 16 bytes creation date, 16 bytes
		//  settled date, 32 bytes payment pre-image, 8 bytes value, 1
		//  byte settled.
		endOfInvoiceIndex := 1 + 1 + 1 + 16 + 16 + 32 + 8 + 1

		// We'll now extract the prefix of the pure invoice embedded
		// within.
		invoiceBytes := paymentBytes[:endOfInvoiceIndex]

		// With the prefix extracted, we'll copy over the invoice, and
		// also add padding for the new 24 bytes of fields, and finally
		// append the remainder of the outgoing payment.
		paymentCopy := make([]byte, len(invoiceBytes))
		copy(paymentCopy[:], invoiceBytes)

		padding := bytes.Repeat([]byte{0}, 24)
		paymentCopy = append(paymentCopy, padding...)
		paymentCopy = append(
			paymentCopy, paymentBytes[endOfInvoiceIndex:]...,
		)

		// At this point, we now have the new format of the outgoing
		// payments, we'll attempt to deserialize it to ensure the
		// bytes are properly formatted.
		paymentReader := bytes.NewReader(paymentCopy)
		_, err := deserializeOutgoingPayment(paymentReader)
		if err != nil {
			return fmt.Errorf("unable to deserialize payment: %v", err)
		}

		// Now that we know the modifications was successful, we'll
		// write it back to disk in the new format.
		if err := payBucket.Put(payID, paymentCopy); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	log.Infof("Migration to outgoing payment invoices complete!")

	return nil
}

// migrateEdgePolicies is a migration function that will update the edges
// bucket. It ensure that edges with unknown policies will also have an entry
// in the bucket. After the migration, there will be two edge entries for
// every channel, regardless of whether the policies are known.
func migrateEdgePolicies(tx *bbolt.Tx) error {
	nodes := tx.Bucket(nodeBucket)
	if nodes == nil {
		return nil
	}

	edges := tx.Bucket(edgeBucket)
	if edges == nil {
		return nil
	}

	edgeIndex := edges.Bucket(edgeIndexBucket)
	if edgeIndex == nil {
		return nil
	}

	// checkKey gets the policy from the database with a low-level call
	// so that it is still possible to distinguish between unknown and
	// not present.
	checkKey := func(channelId uint64, keyBytes []byte) error {
		var channelID [8]byte
		byteOrder.PutUint64(channelID[:], channelId)

		_, err := fetchChanEdgePolicy(edges,
			channelID[:], keyBytes, nodes)

		if err == ErrEdgeNotFound {
			log.Tracef("Adding unknown edge policy present for node %x, channel %v",
				keyBytes, channelId)

			err := putChanEdgePolicyUnknown(edges, channelId, keyBytes)
			if err != nil {
				return err
			}

			return nil
		}

		return err
	}

	// Iterate over all channels and check both edge policies.
	err := edgeIndex.ForEach(func(chanID, edgeInfoBytes []byte) error {
		infoReader := bytes.NewReader(edgeInfoBytes)
		edgeInfo, err := deserializeChanEdgeInfo(infoReader)
		if err != nil {
			return err
		}

		for _, key := range [][]byte{edgeInfo.NodeKey1Bytes[:],
			edgeInfo.NodeKey2Bytes[:]} {

			if err := checkKey(edgeInfo.ChannelID, key); err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("unable to update edge policies: %v", err)
	}

	log.Infof("Migration of edge policies complete!")

	return nil
}

// paymentStatusesMigration is a database migration intended for adding payment
// statuses for each existing payment entity in bucket to be able control
// transitions of statuses and prevent cases such as double payment
func paymentStatusesMigration(tx *bbolt.Tx) error {
	// Get the bucket dedicated to storing statuses of payments,
	// where a key is payment hash, value is payment status.
	paymentStatuses, err := tx.CreateBucketIfNotExists(paymentStatusBucket)
	if err != nil {
		return err
	}

	log.Infof("Migrating database to support payment statuses")

	circuitAddKey := []byte("circuit-adds")
	circuits := tx.Bucket(circuitAddKey)
	if circuits != nil {
		log.Infof("Marking all known circuits with status InFlight")

		err = circuits.ForEach(func(k, v []byte) error {
			// Parse the first 8 bytes as the short chan ID for the
			// circuit. We'll skip all short chan IDs are not
			// locally initiated, which includes all non-zero short
			// chan ids.
			chanID := binary.BigEndian.Uint64(k[:8])
			if chanID != 0 {
				return nil
			}

			// The payment hash is the third item in the serialized
			// payment circuit. The first two items are an AddRef
			// (10 bytes) and the incoming circuit key (16 bytes).
			const payHashOffset = 10 + 16

			paymentHash := v[payHashOffset : payHashOffset+32]

			return paymentStatuses.Put(
				paymentHash[:], StatusInFlight.Bytes(),
			)
		})
		if err != nil {
			return err
		}
	}

	log.Infof("Marking all existing payments with status Completed")

	// Get the bucket dedicated to storing payments
	bucket := tx.Bucket(paymentBucket)
	if bucket == nil {
		return nil
	}

	// For each payment in the bucket, deserialize the payment and mark it
	// as completed.
	err = bucket.ForEach(func(k, v []byte) error {
		// Ignores if it is sub-bucket.
		if v == nil {
			return nil
		}

		r := bytes.NewReader(v)
		payment, err := deserializeOutgoingPayment(r)
		if err != nil {
			return err
		}

		// Calculate payment hash for current payment.
		paymentHash := sha256.Sum256(payment.PaymentPreimage[:])

		// Update status for current payment to completed. If it fails,
		// the migration is aborted and the payment bucket is returned
		// to its previous state.
		return paymentStatuses.Put(paymentHash[:], StatusCompleted.Bytes())
	})
	if err != nil {
		return err
	}

	log.Infof("Migration of payment statuses complete!")

	return nil
}

// migratePruneEdgeUpdateIndex is a database migration that attempts to resolve
// some lingering bugs with regards to edge policies and their update index.
// Stale entries within the edge update index were not being properly pruned due
// to a miscalculation on the offset of an edge's policy last update. This
// migration also fixes the case where the public keys within edge policies were
// being serialized with an extra byte, causing an even greater error when
// attempting to perform the offset calculation described earlier.
func migratePruneEdgeUpdateIndex(tx *bbolt.Tx) error {
	// To begin the migration, we'll retrieve the update index bucket. If it
	// does not exist, we have nothing left to do so we can simply exit.
	edges := tx.Bucket(edgeBucket)
	if edges == nil {
		return nil
	}
	edgeUpdateIndex := edges.Bucket(edgeUpdateIndexBucket)
	if edgeUpdateIndex == nil {
		return nil
	}

	// Retrieve some buckets that will be needed later on. These should
	// already exist given the assumption that the buckets above do as
	// well.
	edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
	if edgeIndex == nil {
		return fmt.Errorf("unable to create/fetch edge index " +
			"bucket")
	}
	nodes, err := tx.CreateBucketIfNotExists(nodeBucket)
	if err != nil {
		return fmt.Errorf("unable to make node bucket")
	}

	log.Info("Migrating database to properly prune edge update index")

	// We'll need to properly prune all the outdated entries within the edge
	// update index. To do so, we'll gather all of the existing policies
	// within the graph to re-populate them later on.
	var edgeKeys [][]byte
	err = edges.ForEach(func(edgeKey, edgePolicyBytes []byte) error {
		// All valid entries are indexed by a public key (33 bytes)
		// followed by a channel ID (8 bytes), so we'll skip any entries
		// with keys that do not match this.
		if len(edgeKey) != 33+8 {
			return nil
		}

		edgeKeys = append(edgeKeys, edgeKey)

		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to gather existing edge policies: %v",
			err)
	}

	log.Info("Constructing set of edge update entries to purge.")

	// Build the set of keys that we will remove from the edge update index.
	// This will include all keys contained within the bucket.
	var updateKeysToRemove [][]byte
	err = edgeUpdateIndex.ForEach(func(updKey, _ []byte) error {
		updateKeysToRemove = append(updateKeysToRemove, updKey)
		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to gather existing edge updates: %v",
			err)
	}

	log.Infof("Removing %d entries from edge update index.",
		len(updateKeysToRemove))

	// With the set of keys contained in the edge update index constructed,
	// we'll proceed in purging all of them from the index.
	for _, updKey := range updateKeysToRemove {
		if err := edgeUpdateIndex.Delete(updKey); err != nil {
			return err
		}
	}

	log.Infof("Repopulating edge update index with %d valid entries.",
		len(edgeKeys))

	// For each edge key, we'll retrieve the policy, deserialize it, and
	// re-add it to the different buckets. By doing so, we'll ensure that
	// all existing edge policies are serialized correctly within their
	// respective buckets and that the correct entries are populated within
	// the edge update index.
	for _, edgeKey := range edgeKeys {
		edgePolicyBytes := edges.Get(edgeKey)

		// Skip any entries with unknown policies as there will not be
		// any entries for them in the edge update index.
		if bytes.Equal(edgePolicyBytes[:], unknownPolicy) {
			continue
		}

		edgePolicy, err := deserializeChanEdgePolicy(
			bytes.NewReader(edgePolicyBytes), nodes,
		)
		if err != nil {
			return err
		}

		err = updateEdgePolicy(edges, edgeIndex, nodes, edgePolicy)
		if err != nil {
			return err
		}
	}

	log.Info("Migration to properly prune edge update index complete!")

	return nil
}

// migrateOptionalChannelCloseSummaryFields migrates the serialized format of
// ChannelCloseSummary to a format where optional fields' presence is indicated
// with boolean markers.
func migrateOptionalChannelCloseSummaryFields(tx *bbolt.Tx) error {
	closedChanBucket := tx.Bucket(closedChannelBucket)
	if closedChanBucket == nil {
		return nil
	}

	log.Info("Migrating to new closed channel format...")
	err := closedChanBucket.ForEach(func(chanID, summary []byte) error {
		r := bytes.NewReader(summary)

		// Read the old (v6) format from the database.
		c, err := deserializeCloseChannelSummaryV6(r)
		if err != nil {
			return err
		}

		// Serialize using the new format, and put back into the
		// bucket.
		var b bytes.Buffer
		if err := serializeChannelCloseSummary(&b, c); err != nil {
			return err
		}

		return closedChanBucket.Put(chanID, b.Bytes())
	})
	if err != nil {
		return fmt.Errorf("unable to update closed channels: %v", err)
	}

	log.Info("Migration to new closed channel format complete!")

	return nil
}
