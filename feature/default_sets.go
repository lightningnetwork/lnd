package feature

import "github.com/lightningnetwork/lnd/lnwire"

// setDesc describes which feature bits should be advertised in which feature
// sets.
type setDesc map[lnwire.FeatureBit]map[Set]struct{}

// defaultSetDesc are the default set descriptors for generating feature
// vectors. Each set is annotated with the corresponding identifier from BOLT 9
// indicating where it should be advertised.
var defaultSetDesc = setDesc{
	lnwire.DataLossProtectRequired: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.GossipQueriesOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.TLVOnionPayloadRequired: {
		SetInit:         {}, // I
		SetNodeAnn:      {}, // N
		SetInvoice:      {}, // 9
		SetInvoiceAmp:   {}, // 9A
		SetLegacyGlobal: {},
	},
	lnwire.StaticRemoteKeyRequired: {
		SetInit:         {}, // I
		SetNodeAnn:      {}, // N
		SetLegacyGlobal: {},
	},
	lnwire.UpfrontShutdownScriptOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.PaymentAddrRequired: {
		SetInit:       {}, // I
		SetNodeAnn:    {}, // N
		SetInvoice:    {}, // 9
		SetInvoiceAmp: {}, // 9A
	},
	lnwire.MPPOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
		SetInvoice: {}, // 9
	},
	lnwire.AnchorsZeroFeeHtlcTxOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.WumboChannelsOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.AMPOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.AMPRequired: {
		SetInvoiceAmp: {}, // 9A
	},
	lnwire.ExplicitChannelTypeOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.KeysendOptional: {
		SetNodeAnn: {}, // N
	},
	lnwire.ScriptEnforcedLeaseOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.ScidAliasOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.ZeroConfOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.RouteBlindingOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
		SetInvoice: {}, // 9
	},
	lnwire.QuiescenceOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.ShutdownAnySegwitOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.SimpleTaprootChannelsOptionalStaging: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.SimpleTaprootOverlayChansOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.ExperimentalEndorsementOptional: {
		SetNodeAnn: {}, // N
	},
	lnwire.RbfCoopCloseOptionalStaging: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.RbfCoopCloseOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
}
