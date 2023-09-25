package channeldb

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/binary"
	"math/rand"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

func getBool(b byte) bool {
	return b%2 == 1
}

// getUint16 requires len(data) >= 2.
func getUint16(data []byte) uint16 {
	return binary.BigEndian.Uint16(data[0:2])
}

// getUint32 requires len(data) >= 4.
func getUint32(data []byte) uint32 {
	return binary.BigEndian.Uint32(data[0:4])
}

// getUint64 requires len(data) >= 8.
func getUint64(data []byte) uint64 {
	return binary.BigEndian.Uint64(data[0:8])
}

// getMilliSatoshi requires len(data) >= 8.
func getMilliSatoshi(data []byte) lnwire.MilliSatoshi {
	return lnwire.MilliSatoshi(getUint64(data[0:8]))
}

// getAmount requires len(data) >= 8.
func getAmount(data []byte) btcutil.Amount {
	return btcutil.Amount(getUint64(data[0:8]))
}

// getBalance requires len(data) >= 9. It returns an *lnwire.MilliSatoshi, with
// a small chance of returning nil.
func getBalance(data []byte) *lnwire.MilliSatoshi {
	if data[0] == 0xff {
		return nil
	}
	msat := getMilliSatoshi(data[1:9])

	return &msat
}

// getHTLCEntries returns the unused data slice and a list of HTLC entries.
func getHTLCEntries(data []byte) ([]byte, []*HTLCEntry) {
	var entries []*HTLCEntry
	for len(data) >= 48 {
		if !getBool(data[0]) {
			data = data[1:]
			break
		}

		entry := &HTLCEntry{
			RefundTimeout: getUint32(data[1:5]),
			OutputIndex:   getUint16(data[5:7]),
			Incoming:      getBool(data[7]),
			Amt:           getAmount(data[8:16]),
		}
		copy(entry.RHash[:], data[16:48])
		entries = append(entries, entry)

		data = data[48:]
	}

	return data, entries
}

func getRevocationLog(data []byte) *RevocationLog {
	if len(data) < 54 {
		return nil
	}

	rl := &RevocationLog{
		OurOutputIndex:   getUint16(data[0:2]),
		TheirOutputIndex: getUint16(data[2:4]),
		OurBalance:       getBalance(data[4:13]),
		TheirBalance:     getBalance(data[13:22]),
	}
	copy(rl.CommitTxHash[:], data[22:54])
	_, rl.HTLCEntries = getHTLCEntries(data[54:])

	return rl
}

// FuzzRevocationLog tests that encoding/decoding RevocationLogs does not modify
// their contents.
func FuzzRevocationLog(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		rl := getRevocationLog(data)
		if rl == nil {
			return
		}

		var b bytes.Buffer
		err := serializeRevocationLog(&b, rl)
		require.NoError(t, err)

		rl2, err := deserializeRevocationLog(&b)
		require.NoError(t, err)

		require.Equal(t, rl, &rl2)
	})
}

// getBytes returns the unused data slice and a byte slice of length 0-255.
func getBytes(data []byte) ([]byte, []byte) {
	if len(data) < 1 {
		return data, nil
	}

	byteLen := int(data[0])
	if len(data[1:]) < byteLen {
		return data, nil
	}

	data = data[1:]

	return data[byteLen:], data[:byteLen]
}

// getWitness returns the unused data slice and a wire.TxWitness. Witness
// elements may be empty but are guaranteed to be non-nil.
func getWitness(data []byte) ([]byte, wire.TxWitness) {
	var witness wire.TxWitness
	for len(data) >= 1 {
		if !getBool(data[0]) {
			data = data[1:]
			break
		}

		var witnessElem []byte
		data, witnessElem = getBytes(data[1:])
		if witnessElem == nil {
			break
		}

		witness = append(witness, witnessElem)
	}

	return data, witness
}

// getOutpoint requires len(data) >= 36.
func getOutPoint(data []byte) wire.OutPoint {
	var op wire.OutPoint
	copy(op.Hash[:], data[0:32])
	op.Index = getUint32(data[32:36])

	return op
}

// getTxIn returns the unused data slice and a list of *wire.TxIn. Each
// *wire.TxIn is guaranteed to be non-nil.
func getTxIn(data []byte) ([]byte, []*wire.TxIn) {
	var ins []*wire.TxIn
	for len(data) >= 41 {
		if !getBool(data[0]) {
			data = data[1:]
			break
		}

		in := &wire.TxIn{
			PreviousOutPoint: getOutPoint(data[1:37]),
			Sequence:         getUint32(data[37:41]),
		}
		data, in.SignatureScript = getBytes(data[41:])
		data, in.Witness = getWitness(data)

		ins = append(ins, in)
	}

	return data, ins
}

// getTxOut returns the unused data slice and a list of *wire.TxOut. Each
// *wire.TxOut is guaranteed to be non-nil.
func getTxOut(data []byte) ([]byte, []*wire.TxOut) {
	var outs []*wire.TxOut
	for len(data) >= 9 {
		if !getBool(data[0]) {
			data = data[1:]
			break
		}

		out := &wire.TxOut{
			Value: int64(getUint64(data[1:9])),
		}
		data, out.PkScript = getBytes(data[9:])

		outs = append(outs, out)
	}

	return data, outs
}

// getTx returns the unused data slice and a transaction that may be nil. If a
// non-nil transaction is returned, it is guaranteed to have at least one TxIn.
func getTx(data []byte) ([]byte, *wire.MsgTx) {
	if len(data) < 8 {
		return data, nil
	}

	tx := &wire.MsgTx{
		Version:  int32(getUint32(data[0:4])),
		LockTime: getUint32(data[4:8]),
	}

	data, tx.TxIn = getTxIn(data[8:])

	// Transactions with zero inputs are invalid.
	if len(tx.TxIn) == 0 {
		return data, nil
	}

	data, tx.TxOut = getTxOut(data)

	return data, tx
}

// getHTLCs returns the unused data slice and a list of HTLCs.
func getHTLCs(data []byte) ([]byte, []HTLC) {
	var htlcs []HTLC
	for len(data) >= 1432 {
		if !getBool(data[0]) {
			data = data[1:]
			break
		}

		htlc := HTLC{
			Amt:           getMilliSatoshi(data[1:9]),
			RefundTimeout: getUint32(data[9:13]),
			OutputIndex:   int32(getUint32(data[13:17])),
			Incoming:      getBool(data[17]),
			HtlcIndex:     getUint64(data[18:26]),
			LogIndex:      getUint64(data[26:34]),
		}
		copy(htlc.RHash[:], data[34:66])
		copy(htlc.OnionBlob[:], data[66:1432])
		data, htlc.Signature = getBytes(data[1432:])
		data, htlc.ExtraData = getBytes(data)

		htlcs = append(htlcs, htlc)
	}

	return data, htlcs
}

func getChannelCommitment(data []byte) *ChannelCommitment {
	if len(data) < 72 {
		return nil
	}

	cc := &ChannelCommitment{
		CommitHeight:    getUint64(data[0:8]),
		LocalLogIndex:   getUint64(data[8:16]),
		LocalHtlcIndex:  getUint64(data[16:24]),
		RemoteLogIndex:  getUint64(data[24:32]),
		RemoteHtlcIndex: getUint64(data[32:40]),
		LocalBalance:    getMilliSatoshi(data[40:48]),
		RemoteBalance:   getMilliSatoshi(data[48:56]),
		CommitFee:       getAmount(data[56:64]),
		FeePerKw:        getAmount(data[64:72]),
	}

	// CommitTx is expected to never be nil.
	data, cc.CommitTx = getTx(data[72:])
	if cc.CommitTx == nil {
		return nil
	}

	data, cc.CommitSig = getBytes(data)
	data, cc.Htlcs = getHTLCs(data)

	return cc
}

func checkWitnessesEqual(t *testing.T, wit1, wit2 wire.TxWitness) {
	require.Equal(t, len(wit1), len(wit2))
	for i, elem1 := range wit1 {
		elem2 := wit2[i]
		require.True(t, bytes.Equal(elem1, elem2))
	}
}

func checkTxsEqual(t *testing.T, tx1, tx2 *wire.MsgTx) {
	if tx1 == nil {
		require.Nil(t, tx2)
		return
	}

	require.Equal(t, tx1.Version, tx2.Version)

	require.Equal(t, len(tx1.TxIn), len(tx2.TxIn))
	for i, in1 := range tx1.TxIn {
		in2 := tx2.TxIn[i]
		require.Equal(t, in1.PreviousOutPoint, in2.PreviousOutPoint)
		require.True(
			t,
			bytes.Equal(in1.SignatureScript, in2.SignatureScript),
		)
		checkWitnessesEqual(t, in1.Witness, in2.Witness)
		require.Equal(t, in1.Sequence, in2.Sequence)
	}

	require.Equal(t, len(tx1.TxOut), len(tx2.TxOut))
	for i, out1 := range tx1.TxOut {
		out2 := tx2.TxOut[i]
		require.Equal(t, out1.Value, out2.Value)
		require.True(t, bytes.Equal(out1.PkScript, out2.PkScript))
	}

	require.Equal(t, tx1.LockTime, tx2.LockTime)
}

func checkHTLCsEqual(t *testing.T, htlcs1, htlcs2 []HTLC) {
	require.Equal(t, len(htlcs1), len(htlcs2))
	for i, htlc1 := range htlcs1 {
		htlc2 := htlcs2[i]
		require.True(t, bytes.Equal(htlc1.Signature, htlc2.Signature))
		require.Equal(t, htlc1.RHash, htlc2.RHash)
		require.Equal(t, htlc1.Amt, htlc2.Amt)
		require.Equal(t, htlc1.RefundTimeout, htlc2.RefundTimeout)
		require.Equal(t, htlc1.OutputIndex, htlc2.OutputIndex)
		require.Equal(t, htlc1.Incoming, htlc2.Incoming)
		require.Equal(t, htlc1.OnionBlob, htlc2.OnionBlob)
		require.Equal(t, htlc1.HtlcIndex, htlc2.HtlcIndex)
		require.Equal(t, htlc1.LogIndex, htlc2.LogIndex)
		require.True(t, bytes.Equal(htlc1.ExtraData, htlc2.ExtraData))
	}
}

func checkChannelCommitmentsEqual(t *testing.T, cc1, cc2 *ChannelCommitment) {
	require.Equal(t, cc1.CommitHeight, cc2.CommitHeight)
	require.Equal(t, cc1.LocalLogIndex, cc2.LocalLogIndex)
	require.Equal(t, cc1.LocalHtlcIndex, cc2.LocalHtlcIndex)
	require.Equal(t, cc1.RemoteLogIndex, cc2.RemoteLogIndex)
	require.Equal(t, cc1.RemoteHtlcIndex, cc2.RemoteHtlcIndex)
	require.Equal(t, cc1.LocalBalance, cc2.LocalBalance)
	require.Equal(t, cc1.RemoteBalance, cc2.RemoteBalance)
	require.Equal(t, cc1.CommitFee, cc2.CommitFee)
	require.Equal(t, cc1.FeePerKw, cc2.FeePerKw)
	checkTxsEqual(t, cc1.CommitTx, cc2.CommitTx)
	require.True(t, bytes.Equal(cc1.CommitSig, cc2.CommitSig))
	checkHTLCsEqual(t, cc1.Htlcs, cc2.Htlcs)
}

// FuzzChannelCommitment tests that encoding/decoding ChannelCommitments does
// not modify their contents.
func FuzzChannelCommitment(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		cc := getChannelCommitment(data)
		if cc == nil {
			return
		}

		var b bytes.Buffer
		err := serializeChanCommit(&b, cc)
		require.NoError(t, err)

		cc2, err := deserializeChanCommit(&b)
		require.NoError(t, err)

		// Because we need nil slices to be considered equal to empty
		// slices, we must implement our own equality check rather than
		// using require.Equal.
		checkChannelCommitmentsEqual(t, cc, &cc2)
	})
}

// getShortChannelID requires len(data) >= 8.
func getShortChannelID(data []byte) lnwire.ShortChannelID {
	return lnwire.NewShortChanIDFromInt(getUint64(data[0:8]))
}

// getInsecurePublicKey requires len(data) >= 8.
func getInsecurePublicKey(data []byte) *btcec.PublicKey {
	seed := int64(getUint64(data[0:8]))
	rng := rand.New(rand.NewSource(seed))

	key, err := ecdsa.GenerateKey(secp256k1.S256(), rng)
	if err != nil {
		panic(err)
	}

	return secp256k1.PrivKeyFromBytes(key.D.Bytes()).PubKey()
}

// getKeyDescriptor requires len(data) >= 16.
func getKeyDescriptor(data []byte) keychain.KeyDescriptor {
	return keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(getUint32(data[0:4])),
			Index:  getUint32(data[4:8]),
		},
		PubKey: getInsecurePublicKey(data[8:16]),
	}
}

// getChannelConfig requires len(data) >= 116.
func getChannelConfig(data []byte) ChannelConfig {
	return ChannelConfig{
		ChannelConstraints: ChannelConstraints{
			DustLimit:        getAmount(data[0:8]),
			ChanReserve:      getAmount(data[8:16]),
			MaxPendingAmount: getMilliSatoshi(data[16:24]),
			MinHTLC:          getMilliSatoshi(data[24:32]),
			MaxAcceptedHtlcs: getUint16(data[32:34]),
			CsvDelay:         getUint16(data[34:36]),
		},
		MultiSigKey:         getKeyDescriptor(data[36:52]),
		RevocationBasePoint: getKeyDescriptor(data[52:68]),
		PaymentBasePoint:    getKeyDescriptor(data[68:84]),
		DelayBasePoint:      getKeyDescriptor(data[84:100]),
		HtlcBasePoint:       getKeyDescriptor(data[100:116]),
	}
}

// getChanInfo returns an *OpenChannel with the necessary fields populated for
// use by serializeChanInfo and deserializeChanInfo.
func getChanInfo(data []byte) *OpenChannel {
	if len(data) < 397 {
		return nil
	}

	oc := &OpenChannel{
		ChanType:               ChannelType(getUint64(data[0:8])),
		FundingOutpoint:        getOutPoint(data[8:44]),
		ShortChannelID:         getShortChannelID(data[44:52]),
		IsPending:              getBool(data[52]),
		IsInitiator:            getBool(data[53]),
		chanStatus:             ChannelStatus(getUint64(data[54:62])),
		FundingBroadcastHeight: getUint32(data[62:66]),
		NumConfsRequired:       getUint16(data[66:68]),
		ChannelFlags:           lnwire.FundingFlag(data[68]),
		IdentityPub:            getInsecurePublicKey(data[69:77]),
		Capacity:               getAmount(data[77:85]),
		TotalMSatSent:          getMilliSatoshi(data[85:93]),
		TotalMSatReceived:      getMilliSatoshi(data[93:101]),
		InitialLocalBalance:    getMilliSatoshi(data[101:109]),
		InitialRemoteBalance:   getMilliSatoshi(data[109:117]),
		LocalChanCfg:           getChannelConfig(data[117:233]),
		RemoteChanCfg:          getChannelConfig(data[233:349]),
		RevocationKeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(getUint32(data[349:353])),
			Index:  getUint32(data[353:357]),
		},
		confirmedScid: getShortChannelID(data[357:365]),
	}
	copy(oc.ChainHash[:], data[365:397])
	data = data[397:]

	if fundingTxPresent(oc) {
		data, oc.FundingTxn = getTx(data)
		if oc.FundingTxn == nil {
			return nil
		}
	}
	_, oc.Memo = getBytes(data)

	return oc
}

func checkOpenChannelsEqual(t *testing.T, oc1, oc2 *OpenChannel) {
	require.Equal(t, oc1.ChanType, oc2.ChanType)
	require.Equal(t, oc1.ChainHash, oc2.ChainHash)
	require.Equal(t, oc1.FundingOutpoint, oc2.FundingOutpoint)
	require.Equal(t, oc1.ShortChannelID, oc2.ShortChannelID)
	require.Equal(t, oc1.IsPending, oc2.IsPending)
	require.Equal(t, oc1.IsInitiator, oc2.IsInitiator)
	require.Equal(t, oc1.chanStatus, oc2.chanStatus)
	require.Equal(t, oc1.FundingBroadcastHeight, oc2.FundingBroadcastHeight)
	require.Equal(t, oc1.NumConfsRequired, oc2.NumConfsRequired)
	require.Equal(t, oc1.ChannelFlags, oc2.ChannelFlags)
	require.Equal(t, oc1.IdentityPub, oc2.IdentityPub)
	require.Equal(t, oc1.Capacity, oc2.Capacity)
	require.Equal(t, oc1.TotalMSatSent, oc2.TotalMSatSent)
	require.Equal(t, oc1.TotalMSatReceived, oc2.TotalMSatReceived)
	require.Equal(t, oc1.InitialLocalBalance, oc2.InitialLocalBalance)
	require.Equal(t, oc1.InitialRemoteBalance, oc2.InitialRemoteBalance)
	require.Equal(t, oc1.LocalChanCfg, oc2.LocalChanCfg)
	require.Equal(t, oc1.RemoteChanCfg, oc2.RemoteChanCfg)
	checkChannelCommitmentsEqual(
		t, &oc1.LocalCommitment, &oc2.LocalCommitment,
	)
	checkChannelCommitmentsEqual(
		t, &oc1.RemoteCommitment, &oc2.RemoteCommitment,
	)
	require.Equal(
		t, oc1.RemoteCurrentRevocation, oc2.RemoteCurrentRevocation,
	)
	require.Equal(t, oc1.RemoteNextRevocation, oc2.RemoteNextRevocation)
	require.Equal(t, oc1.RevocationProducer, oc2.RevocationProducer)
	require.Equal(t, oc1.RevocationStore, oc2.RevocationStore)
	require.Equal(t, oc1.Packager, oc2.Packager)
	checkTxsEqual(t, oc1.FundingTxn, oc2.FundingTxn)
	require.True(
		t,
		bytes.Equal(oc1.LocalShutdownScript, oc2.LocalShutdownScript),
	)
	require.True(
		t,
		bytes.Equal(oc1.RemoteShutdownScript, oc2.RemoteShutdownScript),
	)
	require.Equal(t, oc1.ThawHeight, oc2.ThawHeight)
	require.Equal(t, oc1.LastWasRevoke, oc2.LastWasRevoke)
	require.Equal(t, oc1.RevocationKeyLocator, oc2.RevocationKeyLocator)
	require.Equal(t, oc1.confirmedScid, oc2.confirmedScid)
	require.True(t, bytes.Equal(oc1.Memo, oc2.Memo))
}

// FuzzChanInfo tests that encoding/decoding of the channel info in OpenChannels
// does not modify their contents.
func FuzzChanInfo(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		oc := getChanInfo(data)
		if oc == nil {
			return
		}

		var b bytes.Buffer
		err := serializeChanInfo(&b, oc)
		require.NoError(t, err)

		var oc2 OpenChannel
		err = deserializeChanInfo(&b, &oc2)
		require.NoError(t, err)

		// Because we need nil slices to be considered equal to empty
		// slices, we must implement our own equality check rather than
		// using require.Equal.
		checkOpenChannelsEqual(t, oc, &oc2)
	})
}

// getRevocationStore returns the unused data slice and a shachain.Store
// populated using producer.
func getRevocationStore(data []byte,
	producer shachain.Producer) ([]byte, shachain.Store) {

	rs := shachain.NewRevocationStore()
	index := 0

	for len(data) >= 2 {
		if !getBool(data[0]) {
			data = data[1:]
			break
		}

		numHashesToAdd := int(data[1])
		for end := index + numHashesToAdd; index < end; index++ {
			hash, err := producer.AtIndex(uint64(index))
			if err != nil {
				panic(err)
			}

			if err = rs.AddNextEntry(hash); err != nil {
				panic(err)
			}
		}

		data = data[2:]
	}

	return data, rs
}

// getChanRevocationState returns an *OpenChannel with the necessary fields
// populated for use by serializeChanRevocationState and
// deserializeChanRevocationState.
func getChanRevocationState(data []byte) *OpenChannel {
	if len(data) < 49 {
		return nil
	}

	oc := &OpenChannel{
		RemoteCurrentRevocation: getInsecurePublicKey(data[0:8]),
	}

	if getBool(data[8]) {
		oc.RemoteNextRevocation = getInsecurePublicKey(data[9:17])
	}

	var err error
	oc.RevocationProducer, err = shachain.NewRevocationProducerFromBytes(
		data[17:49],
	)
	if err != nil {
		panic(err)
	}

	_, oc.RevocationStore = getRevocationStore(
		data[49:], oc.RevocationProducer,
	)

	return oc
}

// FuzzChanRevocationState tests that encoding/decoding of the channel
// revocation state in OpenChannels does not modify their contents.
func FuzzChanRevocationState(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		oc := getChanRevocationState(data)
		if oc == nil {
			return
		}

		var b bytes.Buffer
		err := serializeChanRevocationState(&b, oc)
		require.NoError(t, err)

		var oc2 OpenChannel
		r := bytes.NewReader(b.Bytes())
		err = deserializeChanRevocationState(r, &oc2)
		require.NoError(t, err)

		// Because we need nil slices to be considered equal to empty
		// slices, we must implement our own equality check rather than
		// using require.Equal.
		checkOpenChannelsEqual(t, oc, &oc2)
	})
}
