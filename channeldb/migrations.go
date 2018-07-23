package channeldb

import (
	"bytes"
	"fmt"

	"github.com/coreos/bbolt"
)

// migrateNodeAndEdgeUpdateIndex is a migration function that will update the
// database from version 0 to version 1. In version 1, we add two new indexes
// (one for nodes and one for edges) to keep track of the last time a node or
// edge was updated on the network. These new indexes allow us to implement the
// new graph sync protocol added.
func migrateNodeAndEdgeUpdateIndex(tx *bolt.Tx) error {
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
func migrateInvoiceTimeSeries(tx *bolt.Tx) error {
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
		if invoice.Terms.Settled {
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
func migrateInvoiceTimeSeriesOutgoingPayments(tx *bolt.Tx) error {
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
