package htlcswitch

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwire"
)

type DynUpdate func(*lnwire.DynPropose)

func WithDustLimit(amt btcutil.Amount) DynUpdate {
	return func(dp *lnwire.DynPropose) {
		dp.DustLimit = fn.Some(amt)
	}
}

func WithMaxValueInFlight(amt lnwire.MilliSatoshi) DynUpdate {
	return func(dp *lnwire.DynPropose) {
		dp.MaxValueInFlight = fn.Some(amt)
	}
}

func WithChannelReserve(amt btcutil.Amount) DynUpdate {
	return func(dp *lnwire.DynPropose) {
		dp.ChannelReserve = fn.Some(amt)
	}
}

func WithCsvDelay(blks uint16) DynUpdate {
	return func(dp *lnwire.DynPropose) {
		dp.CsvDelay = fn.Some(blks)
	}
}

func WithMaxAcceptedHTLCs(max uint16) DynUpdate {
	return func(dp *lnwire.DynPropose) {
		dp.MaxAcceptedHTLCs = fn.Some(max)
	}
}

func WithFundingKey(key btcec.PublicKey) DynUpdate {
	return func(dp *lnwire.DynPropose) {
		dp.FundingKey = fn.Some(key)
	}
}

func WithChannelType(ty lnwire.ChannelType) DynUpdate {

	return func(dp *lnwire.DynPropose) {
		dp.ChannelType = fn.Some(ty)
	}
}

type dyncommNegotiator struct {}

type DynResponse = fn.Either[lnwire.DynAck, lnwire.DynReject]

func (*dyncommNegotiator) recvDynPropose(msg lnwire.DynPropose) (
	DynResponse, error) {

	panic("NOT IMPLEMENTED: dyncommNegotiator.recvDynPropose")
}

func (*dyncommNegotiator) recvDynAck(msg lnwire.DynAck) (lnwire.DynPropose,
	error) {

	panic("NOT IMPLEMENTED: dyncommNegotiator.recvDynAck")
}

func (*dyncommNegotiator) recvDynReject(msg lnwire.DynReject) error {
	panic("NOT IMPLEMENTED: dyncommNegotiator.recvDynReject")
}

func (*dyncommNegotiator) sendDynPropose(
	updates ...DynUpdate) lnwire.DynPropose {

	panic("NOT IMPLEMENTED: dyncommNegotiator.sendDynPropose")
}

func (*dyncommNegotiator) reset() {
	panic("NOT IMPLEMENTED: dyncommNegotiator.reset")
}