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
	"github.com/lightningnetwork/lnd/channeldb/models"
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

func getChannelCommitment(data []byte) ([]byte, *ChannelCommitment) {
	if len(data) < 72 {
		return data, nil
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
		return data, nil
	}

	data, cc.CommitSig = getBytes(data)
	data, cc.Htlcs = getHTLCs(data)

	return data, cc
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
		_, cc := getChannelCommitment(data)
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

// getSig requires len(data) >= 65.
func getSig(data []byte) lnwire.Sig {
	var (
		sig lnwire.Sig
		err error
	)

	if getBool(data[0]) {
		sig, err = lnwire.NewSigFromWireECDSA(data[1:65])
	} else {
		sig, err = lnwire.NewSigFromSchnorrRawSignature(data[1:65])
	}
	if err != nil {
		panic(err)
	}

	return sig
}

// getSigs returns the unused data slice and a list of lnwire.Sigs.
func getSigs(data []byte) ([]byte, []lnwire.Sig) {
	var sigs []lnwire.Sig

	// Limit to 500 sigs so we don't hit the 64KB limit for lnwire.Message.
	for i := 0; i < 500 && len(data) >= 66; i++ {
		if !getBool(data[0]) {
			data = data[1:]
			break
		}
		sigs = append(sigs, getSig(data[1:66]))
		data = data[66:]
	}

	return data, sigs
}

// getPartialSigWithNonce requires len(data) >= 99.
func getPartialSigWithNonce(data []byte) *lnwire.PartialSigWithNonce {
	if !getBool(data[0]) {
		return nil
	}

	var nonce [66]byte
	copy(nonce[:], data[1:67])

	var sigBytes [32]byte
	copy(sigBytes[:], data[67:99])
	var sigScalar btcec.ModNScalar
	sigScalar.SetBytes(&sigBytes)

	return lnwire.NewPartialSigWithNonce(nonce, sigScalar)
}

// getCommitSig returns the unused data slice and an *lnwire.CommitSig.
func getCommitSig(data []byte) ([]byte, *lnwire.CommitSig) {
	if len(data) < 196 {
		return data, nil
	}

	cs := &lnwire.CommitSig{
		CommitSig:  getSig(data[0:65]),
		PartialSig: getPartialSigWithNonce(data[65:164]),
	}
	copy(cs.ChanID[:], data[164:196])

	data, cs.HtlcSigs = getSigs(data[196:])
	data, cs.ExtraData = getBytes(data)

	return data, cs
}

// getUpdateAddHTLC returns the unused data slice and an *lnwire.UpdateAddHTLC.
func getUpdateAddHTLC(data []byte) ([]byte, lnwire.Message) {
	if len(data) < 1450 {
		return data, nil
	}

	u := &lnwire.UpdateAddHTLC{
		ID:     getUint64(data[0:8]),
		Amount: getMilliSatoshi(data[8:16]),
		Expiry: getUint32(data[16:20]),
	}
	copy(u.ChanID[:], data[20:52])
	copy(u.PaymentHash[:], data[52:84])
	copy(u.OnionBlob[:], data[84:1450])
	data, u.ExtraData = getBytes(data[1450:])

	return data, u
}

// getUpdateFulfillHTLC returns the unused data slice and an
// *lnwire.UpdateFulfillHTLC.
func getUpdateFulfillHTLC(data []byte) ([]byte, lnwire.Message) {
	if len(data) < 72 {
		return data, nil
	}

	u := &lnwire.UpdateFulfillHTLC{
		ID: getUint64(data[0:8]),
	}
	copy(u.ChanID[:], data[8:40])
	copy(u.PaymentPreimage[:], data[40:72])
	data, u.ExtraData = getBytes(data[72:])

	return data, u
}

// getUpdateFailHTLC returns the unused data slice and an
// *lnwire.UpdateFailHTLC.
func getUpdateFailHTLC(data []byte) ([]byte, lnwire.Message) {
	if len(data) < 40 {
		return data, nil
	}

	u := &lnwire.UpdateFailHTLC{
		ID: getUint64(data[0:8]),
	}
	copy(u.ChanID[:], data[8:40])
	data, u.Reason = getBytes(data[40:])
	data, u.ExtraData = getBytes(data)

	return data, u
}

// getUpdateFailMalformedHTLC returns the unused data slice and an
// *lnwire.UpdateFailMalformedHTLC.
func getUpdateFailMalformedHTLC(data []byte) ([]byte, lnwire.Message) {
	if len(data) < 74 {
		return data, nil
	}

	u := &lnwire.UpdateFailMalformedHTLC{
		ID:          getUint64(data[0:8]),
		FailureCode: lnwire.FailCode(getUint16(data[8:10])),
	}
	copy(u.ChanID[:], data[10:42])
	copy(u.ShaOnionBlob[:], data[42:74])
	data, u.ExtraData = getBytes(data[74:])

	return data, u
}

// getUpdateFee returns the unused data slice and an *lnwire.UpdateFee.
func getUpdateFee(data []byte) ([]byte, lnwire.Message) {
	if len(data) < 36 {
		return data, nil
	}

	u := &lnwire.UpdateFee{
		FeePerKw: getUint32(data[0:4]),
	}
	copy(u.ChanID[:], data[4:36])
	data, u.ExtraData = getBytes(data[36:])

	return data, u
}

// getUpdateMsg returns the unused data slice and an update message.
func getUpdateMsg(data []byte) ([]byte, lnwire.Message) {
	if len(data) < 1 {
		return data, nil
	}

	switch {
	case data[0] < 0x30:
		return getUpdateAddHTLC(data[1:])
	case data[0] < 0x60:
		return getUpdateFulfillHTLC(data[1:])
	case data[0] < 0x90:
		return getUpdateFailHTLC(data[1:])
	case data[0] < 0xc0:
		return getUpdateFailMalformedHTLC(data[1:])
	default:
		return getUpdateFee(data[1:])
	}
}

// getLogUpdates returns the unused data slice and a list of LogUpdates. Each
// LogUpdate is guaranteed to have a non-nil UpdateMsg.
func getLogUpdates(data []byte) ([]byte, []LogUpdate) {
	var logUpdates []LogUpdate

	for len(data) >= 9 {
		if !getBool(data[0]) {
			data = data[1:]
			break
		}

		logUpdate := LogUpdate{
			LogIndex: getUint64(data[1:9]),
		}
		data, logUpdate.UpdateMsg = getUpdateMsg(data[9:])
		if logUpdate.UpdateMsg == nil {
			break
		}

		logUpdates = append(logUpdates, logUpdate)
	}

	return data, logUpdates
}

// getCircuitKeys returns the unused data slice and a list of
// models.CircuitKeys.
func getCircuitKeys(data []byte) ([]byte, []models.CircuitKey) {
	var circuitKeys []models.CircuitKey

	for len(data) >= 17 {
		if !getBool(data[0]) {
			data = data[1:]
			break
		}
		circuitKeys = append(circuitKeys, models.CircuitKey{
			ChanID: getShortChannelID(data[1:9]),
			HtlcID: getUint64(data[9:17]),
		})
		data = data[17:]
	}

	return data, circuitKeys
}

// getCommitDiff returns a *CommitDiff or nil. If non-nil is returned, CommitSig
// is guaranteed to be non-nil.
func getCommitDiff(data []byte) *CommitDiff {
	var cd CommitDiff

	data, commitment := getChannelCommitment(data)
	if commitment == nil {
		return nil
	}
	cd.Commitment = *commitment

	data, cd.CommitSig = getCommitSig(data)
	if cd.CommitSig == nil {
		return nil
	}

	data, cd.LogUpdates = getLogUpdates(data)
	data, cd.OpenedCircuitKeys = getCircuitKeys(data)
	_, cd.ClosedCircuitKeys = getCircuitKeys(data)

	return &cd
}

func checkMessagesEqual(t *testing.T, msg1, msg2 lnwire.Message) {
	require.IsType(t, msg1, msg2)
	switch m1 := msg1.(type) {
	case *lnwire.UpdateAddHTLC:
		m2, ok := msg2.(*lnwire.UpdateAddHTLC)
		require.True(t, ok)
		require.Equal(t, m1.ChanID, m2.ChanID)
		require.Equal(t, m1.ID, m2.ID)
		require.Equal(t, m1.Amount, m2.Amount)
		require.Equal(t, m1.PaymentHash, m2.PaymentHash)
		require.Equal(t, m1.Expiry, m2.Expiry)
		require.Equal(t, m1.OnionBlob, m2.OnionBlob)
		require.True(t, bytes.Equal(m1.ExtraData, m2.ExtraData))
	case *lnwire.UpdateFulfillHTLC:
		m2, ok := msg2.(*lnwire.UpdateFulfillHTLC)
		require.True(t, ok)
		require.Equal(t, m1.ChanID, m2.ChanID)
		require.Equal(t, m1.ID, m2.ID)
		require.Equal(t, m1.PaymentPreimage, m2.PaymentPreimage)
		require.True(t, bytes.Equal(m1.ExtraData, m2.ExtraData))
	case *lnwire.UpdateFailHTLC:
		m2, ok := msg2.(*lnwire.UpdateFailHTLC)
		require.True(t, ok)
		require.Equal(t, m1.ChanID, m2.ChanID)
		require.Equal(t, m1.ID, m2.ID)
		require.True(t, bytes.Equal(m1.Reason, m2.Reason))
		require.True(t, bytes.Equal(m1.ExtraData, m2.ExtraData))
	case *lnwire.UpdateFailMalformedHTLC:
		m2, ok := msg2.(*lnwire.UpdateFailMalformedHTLC)
		require.True(t, ok)
		require.Equal(t, m1.ChanID, m2.ChanID)
		require.Equal(t, m1.ID, m2.ID)
		require.Equal(t, m1.ShaOnionBlob, m2.ShaOnionBlob)
		require.Equal(t, m1.FailureCode, m2.FailureCode)
		require.True(t, bytes.Equal(m1.ExtraData, m2.ExtraData))
	case *lnwire.UpdateFee:
		m2, ok := msg2.(*lnwire.UpdateFee)
		require.True(t, ok)
		require.Equal(t, m1.ChanID, m2.ChanID)
		require.Equal(t, m1.FeePerKw, m2.FeePerKw)
		require.True(t, bytes.Equal(m1.ExtraData, m2.ExtraData))
	default:
		t.Fatalf("Invalid update type: %v", m1)
	}
}

func checkLogUpdatesEqual(t *testing.T, updates1, updates2 []LogUpdate) {
	require.Equal(t, len(updates1), len(updates2))
	for i, lu1 := range updates1 {
		lu2 := updates2[i]
		require.Equal(t, lu1.LogIndex, lu2.LogIndex)
		checkMessagesEqual(t, lu1.UpdateMsg, lu2.UpdateMsg)
	}
}

func checkSigsEqual(t *testing.T, sig1, sig2 lnwire.Sig) {
	// lnwire.Sig contains a sigType field that does not get encoded/decoded
	// with the signature bytes. This means that the sigType is lost during
	// encoding and can cause require.Equal() to fail. It seems users are
	// expected to recover the sigType at higher levels using
	// Sig.ForceSchnorr() if necessary (see commit
	// 39d5dffd5602bc210d668d62c0a134b52c734e6b).
	//
	// To work around this ugliness, we ignore the sigType when checking for
	// equality.
	require.Equal(t, sig1.RawBytes(), sig2.RawBytes())
}

func checkHtlcSigsEqual(t *testing.T, sigs1, sigs2 []lnwire.Sig) {
	require.Equal(t, len(sigs1), len(sigs2))
	for i, sig1 := range sigs1 {
		sig2 := sigs2[i]
		checkSigsEqual(t, sig1, sig2)
	}
}

func checkCommitSigsEqual(t *testing.T, cs1, cs2 *lnwire.CommitSig) {
	require.Equal(t, cs1.ChanID, cs2.ChanID)
	checkSigsEqual(t, cs1.CommitSig, cs2.CommitSig)
	checkHtlcSigsEqual(t, cs1.HtlcSigs, cs2.HtlcSigs)
	require.Equal(t, cs1.PartialSig, cs2.PartialSig)
	require.True(t, bytes.Equal(cs1.ExtraData, cs2.ExtraData))
}

func checkCircuitKeysEqual(t *testing.T, keys1, keys2 []models.CircuitKey) {
	require.Equal(t, len(keys1), len(keys2))
	for i, ck1 := range keys1 {
		ck2 := keys2[i]
		require.Equal(t, ck1, ck2)
	}
}

func checkCommitDiffsEqual(t *testing.T, cd1, cd2 *CommitDiff) {
	checkChannelCommitmentsEqual(t, &cd1.Commitment, &cd2.Commitment)
	checkLogUpdatesEqual(t, cd1.LogUpdates, cd2.LogUpdates)
	checkCommitSigsEqual(t, cd1.CommitSig, cd2.CommitSig)
	checkCircuitKeysEqual(t, cd1.OpenedCircuitKeys, cd2.OpenedCircuitKeys)
	checkCircuitKeysEqual(t, cd1.ClosedCircuitKeys, cd2.ClosedCircuitKeys)
	require.Equal(t, cd1.AddAcks, cd2.AddAcks)
	require.Equal(t, cd1.SettleFailAcks, cd2.SettleFailAcks)
}

// FuzzCommitDiff tests that encoding/decoding of CommitDiffs does not modify
// their contents.
func FuzzCommitDiff(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		cd := getCommitDiff(data)
		if cd == nil {
			return
		}

		var b bytes.Buffer
		err := serializeCommitDiff(&b, cd)
		require.NoError(t, err)

		cd2, err := deserializeCommitDiff(&b)
		require.NoError(t, err)

		// Because we need nil slices to be considered equal to empty
		// slices, we must implement our own equality check rather than
		// using require.Equal.
		checkCommitDiffsEqual(t, cd, cd2)
	})
}

// getChannelReestablish returns the unused data slice and an
// *lnwire.ChannelReestablish.
func getChannelReestablish(data []byte) ([]byte, *lnwire.ChannelReestablish) {
	if len(data) < 155 {
		return data, nil
	}

	cr := &lnwire.ChannelReestablish{
		NextLocalCommitHeight:     getUint64(data[0:8]),
		RemoteCommitTailHeight:    getUint64(data[8:16]),
		LocalUnrevokedCommitPoint: getInsecurePublicKey(data[16:24]),
	}
	copy(cr.ChanID[:], data[24:56])
	copy(cr.LastRemoteCommitSecret[:], data[56:88])

	if getBool(data[88]) {
		var nonce lnwire.Musig2Nonce
		copy(nonce[:], data[89:155])
		cr.LocalNonce = &nonce
	}

	data, cr.ExtraData = getBytes(data[155:])

	return data, cr
}

func getChannelCloseSummary(data []byte) *ChannelCloseSummary {
	if len(data) < 281 {
		return nil
	}

	ccs := &ChannelCloseSummary{
		ChanPoint:         getOutPoint(data[0:36]),
		ShortChanID:       getShortChannelID(data[36:44]),
		RemotePub:         getInsecurePublicKey(data[44:52]),
		Capacity:          getAmount(data[52:60]),
		CloseHeight:       getUint32(data[60:64]),
		SettledBalance:    getAmount(data[64:72]),
		TimeLockedBalance: getAmount(data[72:80]),
		CloseType:         ClosureType(data[80]),
		IsPending:         getBool(data[81]),
	}
	copy(ccs.ChainHash[:], data[82:114])
	copy(ccs.ClosingTXID[:], data[114:146])
	if !getBool(data[146]) {
		// Returning here creates a legacy summary with less data.
		return ccs
	}

	ccs.RemoteCurrentRevocation = getInsecurePublicKey(data[147:155])
	if getBool(data[155]) {
		ccs.RemoteNextRevocation = getInsecurePublicKey(data[156:164])
	}
	ccs.LocalChanConfig = getChannelConfig(data[164:280])
	if getBool(data[280]) {
		data, ccs.LastChanSyncMsg = getChannelReestablish(data[281:])
	}

	return ccs
}

func checkChannelReestablishesEqual(t *testing.T, cr1,
	cr2 *lnwire.ChannelReestablish) {

	if cr1 == nil {
		require.Nil(t, cr2)
		return
	}

	require.Equal(t, cr1.ChanID, cr2.ChanID)
	require.Equal(t, cr1.NextLocalCommitHeight, cr2.NextLocalCommitHeight)
	require.Equal(t, cr1.RemoteCommitTailHeight, cr2.RemoteCommitTailHeight)
	require.Equal(t, cr1.LastRemoteCommitSecret, cr2.LastRemoteCommitSecret)
	require.Equal(
		t, cr1.LocalUnrevokedCommitPoint, cr2.LocalUnrevokedCommitPoint,
	)
	require.Equal(t, cr1.LocalNonce, cr2.LocalNonce)
	require.True(t, bytes.Equal(cr1.ExtraData, cr2.ExtraData))
}

func checkChannelCloseSummariesEqual(t *testing.T, ccs1,
	ccs2 *ChannelCloseSummary) {

	require.Equal(t, ccs1.ChanPoint, ccs2.ChanPoint)
	require.Equal(t, ccs1.ShortChanID, ccs2.ShortChanID)
	require.Equal(t, ccs1.ChainHash, ccs2.ChainHash)
	require.Equal(t, ccs1.ClosingTXID, ccs2.ClosingTXID)
	require.Equal(t, ccs1.RemotePub, ccs2.RemotePub)
	require.Equal(t, ccs1.Capacity, ccs2.Capacity)
	require.Equal(t, ccs1.CloseHeight, ccs2.CloseHeight)
	require.Equal(t, ccs1.SettledBalance, ccs2.SettledBalance)
	require.Equal(t, ccs1.TimeLockedBalance, ccs2.TimeLockedBalance)
	require.Equal(t, ccs1.CloseType, ccs2.CloseType)
	require.Equal(t, ccs1.IsPending, ccs2.IsPending)
	require.Equal(
		t, ccs1.RemoteCurrentRevocation, ccs2.RemoteCurrentRevocation,
	)
	require.Equal(t, ccs1.RemoteNextRevocation, ccs2.RemoteNextRevocation)
	require.Equal(t, ccs1.LocalChanConfig, ccs2.LocalChanConfig)
	checkChannelReestablishesEqual(
		t, ccs1.LastChanSyncMsg, ccs2.LastChanSyncMsg,
	)
}

// FuzzChannelCloseSummary tests that encoding/decoding of ChannelCloseSummary
// does not modify its contents.
func FuzzChannelCloseSummary(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		ccs := getChannelCloseSummary(data)
		if ccs == nil {
			return
		}

		var b bytes.Buffer
		err := serializeChannelCloseSummary(&b, ccs)
		require.NoError(t, err)

		ccs2, err := deserializeCloseChannelSummary(&b)
		require.NoError(t, err)

		// Because we need nil slices to be considered equal to empty
		// slices, we must implement our own equality check rather than
		// using require.Equal.
		checkChannelCloseSummariesEqual(t, ccs, ccs2)
	})
}
