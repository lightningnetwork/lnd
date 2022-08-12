package itest

import (
	"flag"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntemp"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	testFeeBase    = 1e+6
	defaultCSV     = node.DefaultCSV
	defaultTimeout = wait.DefaultTimeout
	itestLndBinary = "../../lnd-itest"
	anchorSize     = 330
	noFeeLimitMsat = math.MaxInt64

	AddrTypeWitnessPubkeyHash = lnrpc.AddressType_WITNESS_PUBKEY_HASH
	AddrTypeNestedPubkeyHash  = lnrpc.AddressType_NESTED_PUBKEY_HASH
	AddrTypeTaprootPubkey     = lnrpc.AddressType_TAPROOT_PUBKEY
)

var (
	harnessNetParams = &chaincfg.RegressionNetParams

	// lndExecutable is the full path to the lnd binary.
	lndExecutable = flag.String(
		"lndexec", itestLndBinary, "full path to lnd binary",
	)
)

// commitTypeHasAnchors returns whether commitType uses anchor outputs.
func commitTypeHasAnchors(commitType lnrpc.CommitmentType) bool {
	switch commitType {
	case lnrpc.CommitmentType_ANCHORS,
		lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		return true
	default:
		return false
	}
}

// nodeArgsForCommitType returns the command line flag to supply to enable this
// commitment type.
func nodeArgsForCommitType(commitType lnrpc.CommitmentType) []string {
	switch commitType {
	case lnrpc.CommitmentType_LEGACY:
		return []string{"--protocol.legacy.committweak"}
	case lnrpc.CommitmentType_STATIC_REMOTE_KEY:
		return []string{}
	case lnrpc.CommitmentType_ANCHORS:
		return []string{"--protocol.anchors"}
	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		return []string{
			"--protocol.anchors",
			"--protocol.script-enforced-lease",
		}
	}

	return nil
}

// calcStaticFee calculates appropriate fees for commitment transactions.  This
// function provides a simple way to allow test balance assertions to take fee
// calculations into account.
func calcStaticFee(c lnrpc.CommitmentType, numHTLCs int) btcutil.Amount {
	const htlcWeight = input.HTLCWeight
	var (
		feePerKw = chainfee.SatPerKWeight(
			lntemp.DefaultFeeRateSatPerKw,
		)
		commitWeight       = input.CommitWeight
		anchors            = btcutil.Amount(0)
		defaultSatPerVByte = lnwallet.DefaultAnchorsCommitMaxFeeRateSatPerVByte
	)

	// The anchor commitment type is slightly heavier, and we must also add
	// the value of the two anchors to the resulting fee the initiator
	// pays. In addition the fee rate is capped at 10 sat/vbyte for anchor
	// channels.
	if commitTypeHasAnchors(c) {
		feePerKw = chainfee.SatPerKVByte(
			defaultSatPerVByte * 1000).FeePerKWeight()
		commitWeight = input.AnchorCommitWeight
		anchors = 2 * anchorSize
	}

	return feePerKw.FeeForWeight(int64(commitWeight+htlcWeight*numHTLCs)) +
		anchors
}

// calculateMaxHtlc re-implements the RequiredRemoteChannelReserve of the
// funding manager's config, which corresponds to the maximum MaxHTLC value we
// allow users to set when updating a channel policy.
func calculateMaxHtlc(chanCap btcutil.Amount) uint64 {
	reserve := lnwire.NewMSatFromSatoshis(chanCap / 100)
	max := lnwire.NewMSatFromSatoshis(chanCap) - reserve
	return uint64(max)
}
