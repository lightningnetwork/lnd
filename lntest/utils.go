package lntest

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// NeutrinoBackendName is the name of the neutrino backend.
	NeutrinoBackendName = "neutrino"

	DefaultTimeout = wait.DefaultTimeout

	// noFeeLimitMsat is used to specify we will put no requirements on fee
	// charged when choosing a route path.
	noFeeLimitMsat = math.MaxInt64
)

// CopyFile copies the file src to dest.
func CopyFile(dest, src string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	d, err := os.Create(dest)
	if err != nil {
		return err
	}

	if _, err := io.Copy(d, s); err != nil {
		d.Close()
		return err
	}

	return d.Close()
}

// errNumNotMatched is a helper method to return a nicely formatted error.
func errNumNotMatched(name string, subject string,
	want, got, total, old int) error {

	return fmt.Errorf("%s: assert %s failed: want %d, got: %d, total: "+
		"%d, previously had: %d", name, subject, want, got, total, old)
}

// parseDerivationPath parses a path in the form of m/x'/y'/z'/a/b into a slice
// of [x, y, z, a, b], meaning that the apostrophe is ignored and 2^31 is _not_
// added to the numbers.
func ParseDerivationPath(path string) ([]uint32, error) {
	path = strings.TrimSpace(path)
	if len(path) == 0 {
		return nil, fmt.Errorf("path cannot be empty")
	}
	if !strings.HasPrefix(path, "m/") {
		return nil, fmt.Errorf("path must start with m/")
	}

	// Just the root key, no path was provided. This is valid but not useful
	// in most cases.
	rest := strings.ReplaceAll(path, "m/", "")
	if rest == "" {
		return []uint32{}, nil
	}

	parts := strings.Split(rest, "/")
	indices := make([]uint32, len(parts))
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if strings.Contains(parts[i], "'") {
			part = strings.TrimRight(parts[i], "'")
		}
		parsed, err := strconv.ParseInt(part, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("could not parse part \"%s\": "+
				"%v", part, err)
		}
		indices[i] = uint32(parsed)
	}

	return indices, nil
}

// ChanPointFromPendingUpdate constructs a channel point from a lnrpc pending
// update.
func ChanPointFromPendingUpdate(pu *lnrpc.PendingUpdate) *lnrpc.ChannelPoint {
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pu.Txid,
		},
		OutputIndex: pu.OutputIndex,
	}

	return chanPoint
}

// channelPointStr returns the string representation of the channel's
// funding transaction.
func channelPointStr(chanPoint *lnrpc.ChannelPoint) string {
	fundingTxID, err := lnrpc.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		return ""
	}
	cp := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: chanPoint.OutputIndex,
	}

	return cp.String()
}

// CommitTypeHasAnchors returns whether commitType uses anchor outputs.
func CommitTypeHasAnchors(commitType lnrpc.CommitmentType) bool {
	switch commitType {
	case lnrpc.CommitmentType_ANCHORS,
		lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		return true
	default:
		return false
	}
}

// NodeArgsForCommitType returns the command line flag to supply to enable this
// commitment type.
func NodeArgsForCommitType(commitType lnrpc.CommitmentType) []string {
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

// CalcStaticFee calculates appropriate fees for commitment transactions. This
// function provides a simple way to allow test balance assertions to take fee
// calculations into account.
func CalcStaticFee(c lnrpc.CommitmentType, numHTLCs int) btcutil.Amount {
	//nolint:lll
	const (
		htlcWeight         = input.HTLCWeight
		anchorSize         = 330 * 2
		defaultSatPerVByte = lnwallet.DefaultAnchorsCommitMaxFeeRateSatPerVByte
		scale              = 1000
	)

	var (
		anchors      = btcutil.Amount(0)
		commitWeight = input.CommitWeight
		feePerKw     = chainfee.SatPerKWeight(DefaultFeeRateSatPerKw)
	)

	// The anchor commitment type is slightly heavier, and we must also add
	// the value of the two anchors to the resulting fee the initiator
	// pays. In addition the fee rate is capped at 10 sat/vbyte for anchor
	// channels.
	if CommitTypeHasAnchors(c) {
		feePerKw = chainfee.SatPerKVByte(
			defaultSatPerVByte * scale,
		).FeePerKWeight()
		commitWeight = input.AnchorCommitWeight
		anchors = anchorSize
	}

	return feePerKw.FeeForWeight(int64(commitWeight+htlcWeight*numHTLCs)) +
		anchors
}

// CalculateMaxHtlc re-implements the RequiredRemoteChannelReserve of the
// funding manager's config, which corresponds to the maximum MaxHTLC value we
// allow users to set when updating a channel policy.
func CalculateMaxHtlc(chanCap btcutil.Amount) uint64 {
	const ratio = 100
	reserve := lnwire.NewMSatFromSatoshis(chanCap / ratio)
	max := lnwire.NewMSatFromSatoshis(chanCap) - reserve

	return uint64(max)
}
