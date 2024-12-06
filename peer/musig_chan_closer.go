package peer

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
)

// MusigChanCloser is an adapter over the normal channel state machine that
// allows the chan closer to handle the musig2 details of closing taproot
// channels.
type MusigChanCloser struct {
	channel *lnwallet.LightningChannel

	musigSession *lnwallet.MusigSession

	localNonce  *musig2.Nonces
	remoteNonce *musig2.Nonces
}

// NewMusigChanCloser creates a new musig chan closer from a normal channel.
func NewMusigChanCloser(channel *lnwallet.LightningChannel) *MusigChanCloser {
	return &MusigChanCloser{
		channel: channel,
	}
}

// ProposalClosingOpts returns the options that should be used when
// generating a new co-op close signature.
func (m *MusigChanCloser) ProposalClosingOpts() (
	[]lnwallet.ChanCloseOpt, error) {

	switch {
	case m.localNonce == nil:
		return nil, fmt.Errorf("local nonce not generated")

	case m.remoteNonce == nil:
		return nil, fmt.Errorf("remote nonce not generated")
	}

	localKey, remoteKey := m.channel.MultiSigKeys()

	tapscriptTweak := fn.MapOption(lnwallet.TapscriptRootToTweak)(
		m.channel.State().TapscriptRoot,
	)

	m.musigSession = lnwallet.NewPartialMusigSession(
		*m.remoteNonce, localKey, remoteKey,
		m.channel.Signer, m.channel.FundingTxOut(),
		lnwallet.RemoteMusigCommit, tapscriptTweak,
	)

	err := m.musigSession.FinalizeSession(*m.localNonce)
	if err != nil {
		return nil, err
	}

	return []lnwallet.ChanCloseOpt{
		lnwallet.WithCoopCloseMusigSession(m.musigSession),
	}, nil
}

// CombineClosingOpts returns the options that should be used when combining
// the final musig partial signature. The method also maps the lnwire partial
// signatures into an input.Signature that can be used more generally.
func (m *MusigChanCloser) CombineClosingOpts(localSig,
	remoteSig lnwire.PartialSig) (input.Signature, input.Signature,
	[]lnwallet.ChanCloseOpt, error) {

	if m.musigSession == nil {
		return nil, nil, nil, fmt.Errorf("musig session not created")
	}

	// We'll convert the wire partial signatures into an input.Signature
	// compliant struct so we can pass it into the final combination
	// function.
	localPartialSig := &lnwire.PartialSigWithNonce{
		PartialSig: localSig,
		Nonce:      m.localNonce.PubNonce,
	}
	remotePartialSig := &lnwire.PartialSigWithNonce{
		PartialSig: remoteSig,
		Nonce:      m.remoteNonce.PubNonce,
	}

	localMuSig := new(lnwallet.MusigPartialSig).FromWireSig(
		localPartialSig,
	)
	remoteMuSig := new(lnwallet.MusigPartialSig).FromWireSig(
		remotePartialSig,
	)

	opts := []lnwallet.ChanCloseOpt{
		lnwallet.WithCoopCloseMusigSession(m.musigSession),
	}

	// For taproot channels, we'll need to pass along the session so the
	// final combined signature can be created.
	return localMuSig, remoteMuSig, opts, nil
}

// ClosingNonce returns the nonce that should be used when generating the our
// partial signature for the remote party.
func (m *MusigChanCloser) ClosingNonce() (*musig2.Nonces, error) {
	if m.localNonce != nil {
		return m.localNonce, nil
	}

	localKey, _ := m.channel.MultiSigKeys()
	nonce, err := musig2.GenNonces(
		musig2.WithPublicKey(localKey.PubKey),
	)
	if err != nil {
		return nil, err
	}

	m.localNonce = nonce

	return nonce, nil
}

// InitRemoteNonce saves the remote nonce the party sent during their shutdown
// message so it can be used later to generate and verify signatures.
func (m *MusigChanCloser) InitRemoteNonce(nonce *musig2.Nonces) {
	m.remoteNonce = nonce
}

// A compile-time assertion to ensure MusigChanCloser implements the
// chancloser.MusigSession interface.
var _ chancloser.MusigSession = (*MusigChanCloser)(nil)
