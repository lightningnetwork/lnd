package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/kvdb"
)

func putChanEdgeInfo(edgeIndex kvdb.RwBucket, edgeInfo *models.ChannelEdgeInfo1,
	chanID [8]byte) error {

	var b bytes.Buffer

	if _, err := b.Write(edgeInfo.NodeKey1Bytes[:]); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.NodeKey2Bytes[:]); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.BitcoinKey1Bytes[:]); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.BitcoinKey2Bytes[:]); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(&b, 0, edgeInfo.Features); err != nil {
		return err
	}

	authProof := edgeInfo.AuthProof
	var nodeSig1, nodeSig2, bitcoinSig1, bitcoinSig2 []byte
	if authProof != nil {
		nodeSig1 = authProof.NodeSig1Bytes
		nodeSig2 = authProof.NodeSig2Bytes
		bitcoinSig1 = authProof.BitcoinSig1Bytes
		bitcoinSig2 = authProof.BitcoinSig2Bytes
	}

	if err := wire.WriteVarBytes(&b, 0, nodeSig1); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, nodeSig2); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, bitcoinSig1); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, bitcoinSig2); err != nil {
		return err
	}

	if err := writeOutpoint(&b, &edgeInfo.ChannelPoint); err != nil {
		return err
	}
	err := binary.Write(&b, byteOrder, uint64(edgeInfo.Capacity))
	if err != nil {
		return err
	}
	if _, err := b.Write(chanID[:]); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.ChainHash[:]); err != nil {
		return err
	}

	if len(edgeInfo.ExtraOpaqueData) > MaxAllowedExtraOpaqueBytes {
		return ErrTooManyExtraOpaqueBytes(len(edgeInfo.ExtraOpaqueData))
	}
	err = wire.WriteVarBytes(&b, 0, edgeInfo.ExtraOpaqueData)
	if err != nil {
		return err
	}

	return edgeIndex.Put(chanID[:], b.Bytes())
}

func fetchChanEdgeInfo(edgeIndex kvdb.RBucket,
	chanID []byte) (models.ChannelEdgeInfo1, error) {

	edgeInfoBytes := edgeIndex.Get(chanID)
	if edgeInfoBytes == nil {
		return models.ChannelEdgeInfo1{}, ErrEdgeNotFound
	}

	edgeInfoReader := bytes.NewReader(edgeInfoBytes)

	return deserializeChanEdgeInfo(edgeInfoReader)
}

func deserializeChanEdgeInfo(r io.Reader) (models.ChannelEdgeInfo1, error) {
	var (
		err      error
		edgeInfo models.ChannelEdgeInfo1
	)

	if _, err := io.ReadFull(r, edgeInfo.NodeKey1Bytes[:]); err != nil {
		return models.ChannelEdgeInfo1{}, err
	}
	if _, err := io.ReadFull(r, edgeInfo.NodeKey2Bytes[:]); err != nil {
		return models.ChannelEdgeInfo1{}, err
	}
	if _, err := io.ReadFull(r, edgeInfo.BitcoinKey1Bytes[:]); err != nil {
		return models.ChannelEdgeInfo1{}, err
	}
	if _, err := io.ReadFull(r, edgeInfo.BitcoinKey2Bytes[:]); err != nil {
		return models.ChannelEdgeInfo1{}, err
	}

	edgeInfo.Features, err = wire.ReadVarBytes(r, 0, 900, "features")
	if err != nil {
		return models.ChannelEdgeInfo1{}, err
	}

	proof := &models.ChannelAuthProof1{}

	proof.NodeSig1Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return models.ChannelEdgeInfo1{}, err
	}
	proof.NodeSig2Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return models.ChannelEdgeInfo1{}, err
	}
	proof.BitcoinSig1Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return models.ChannelEdgeInfo1{}, err
	}
	proof.BitcoinSig2Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return models.ChannelEdgeInfo1{}, err
	}

	if !proof.IsEmpty() {
		edgeInfo.AuthProof = proof
	}

	edgeInfo.ChannelPoint = wire.OutPoint{}
	if err := readOutpoint(r, &edgeInfo.ChannelPoint); err != nil {
		return models.ChannelEdgeInfo1{}, err
	}
	if err := binary.Read(r, byteOrder, &edgeInfo.Capacity); err != nil {
		return models.ChannelEdgeInfo1{}, err
	}
	if err := binary.Read(r, byteOrder, &edgeInfo.ChannelID); err != nil {
		return models.ChannelEdgeInfo1{}, err
	}

	if _, err := io.ReadFull(r, edgeInfo.ChainHash[:]); err != nil {
		return models.ChannelEdgeInfo1{}, err
	}

	// We'll try and see if there are any opaque bytes left, if not, then
	// we'll ignore the EOF error and return the edge as is.
	edgeInfo.ExtraOpaqueData, err = wire.ReadVarBytes(
		r, 0, MaxAllowedExtraOpaqueBytes, "blob",
	)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
	case errors.Is(err, io.EOF):
	case err != nil:
		return models.ChannelEdgeInfo1{}, err
	}

	return edgeInfo, nil
}
