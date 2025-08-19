package htlcswitch

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hodl"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/require"
)

var zeroCircuit = models.CircuitKey{}
var emptyScid = lnwire.ShortChannelID{}

func genPreimage() ([32]byte, error) {
	var preimage [32]byte
	if _, err := io.ReadFull(rand.Reader, preimage[:]); err != nil {
		return preimage, err
	}
	return preimage, nil
}

// TestSwitchAddDuplicateLink tests that the switch will reject duplicate links
// for live links. It also tests that we can successfully add a link after
// having removed it.
func TestSwitchAddDuplicateLink(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, aliceScid := genID()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceScid, emptyScid, alicePeer, false, false,
		false, false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}

	// Alice should have a live link, adding again should fail.
	if err := s.AddLink(aliceChannelLink); err == nil {
		t.Fatalf("adding duplicate link should have failed")
	}

	// Remove the live link to ensure the indexes are cleared.
	s.RemoveLink(chanID1)

	// Alice has no links, adding should succeed.
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
}

// TestSwitchHasActiveLink tests the behavior of HasActiveLink, and asserts that
// it only returns true if a link's short channel id has confirmed (meaning the
// channel is no longer pending) and it's EligibleToForward method returns true,
// i.e. it has received ChannelReady from the remote peer.
func TestSwitchHasActiveLink(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, aliceScid := genID()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceScid, emptyScid, alicePeer, false, false,
		false, false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}

	// The link has been added, but it's still pending. HasActiveLink should
	// return false since the link has not been added to the linkIndex
	// containing live links.
	if s.HasActiveLink(chanID1) {
		t.Fatalf("link should not be active yet, still pending")
	}

	// Finally, simulate the link receiving channel_ready by setting its
	// eligibility to true.
	aliceChannelLink.eligible = true

	// The link should now be reported as active, since EligibleToForward
	// returns true and the link is in the linkIndex.
	if !s.HasActiveLink(chanID1) {
		t.Fatalf("link should not be active now")
	}
}

// TestSwitchSendPending checks the inability of htlc switch to forward adds
// over pending links.
func TestSwitchSendPending(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")

	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	pendingChanID := lnwire.ShortChannelID{}

	aliceChannelLink := newMockChannelLink(
		s, chanID1, pendingChanID, emptyScid, alicePeer, false, false,
		false, false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}

	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should is being forwarded from Bob channel
	// link to Alice channel link.
	preimage, err := genPreimage()
	require.NoError(t, err, "unable to generate preimage")
	rhash := sha256.Sum256(preimage[:])
	packet := &htlcPacket{
		incomingChanID: bobChanID,
		incomingHTLCID: 0,
		outgoingChanID: aliceChanID,
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Send the ADD packet, this should not be forwarded out to the link
	// since there are no eligible links.
	if err = s.ForwardPackets(nil, packet); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-bobChannelLink.packets:
		if p.linkFailure != nil {
			err = p.linkFailure
		}
	case <-time.After(time.Second):
		t.Fatal("no timely reply from switch")
	}
	linkErr, ok := err.(*LinkError)
	if !ok {
		t.Fatalf("expected link error, got: %T", err)
	}
	if linkErr.WireMessage().Code() != lnwire.CodeUnknownNextPeer {
		t.Fatalf("expected fail unknown next peer, got: %T",
			linkErr.WireMessage().Code())
	}

	// No message should be sent, since the packet was failed.
	select {
	case <-aliceChannelLink.packets:
		t.Fatal("expected not to receive message")
	case <-time.After(time.Second):
	}

	// Since the packet should have been failed, there should be no active
	// circuits.
	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSwitchForwardMapping checks that the Switch properly consults its maps
// when forwarding packets.
func TestSwitchForwardMapping(t *testing.T) {
	tests := []struct {
		name string

		// If this is true, then Alice's channel will be private.
		alicePrivate bool

		// If this is true, then Alice's channel will be a zero-conf
		// channel.
		zeroConf bool

		// If this is true, then Alice's channel will be an
		// option-scid-alias feature-bit, non-zero-conf channel.
		optionScid bool

		// If this is true, then an alias will be used for forwarding.
		useAlias bool

		// This is Alice's channel alias. This may not be set if this
		// is not an option_scid_alias channel (feature bit).
		aliceAlias lnwire.ShortChannelID

		// This is Alice's confirmed SCID. This may not be set if this
		// is a zero-conf channel before confirmation.
		aliceReal lnwire.ShortChannelID

		// If this is set, we expect Bob forwarding to Alice to fail.
		expectErr bool
	}{
		{
			name:         "private unconfirmed zero-conf",
			alicePrivate: true,
			zeroConf:     true,
			useAlias:     true,
			aliceAlias: lnwire.ShortChannelID{
				BlockHeight: 16_000_002,
				TxIndex:     2,
				TxPosition:  2,
			},
			aliceReal: lnwire.ShortChannelID{},
			expectErr: false,
		},
		{
			name:         "private confirmed zero-conf",
			alicePrivate: true,
			zeroConf:     true,
			useAlias:     true,
			aliceAlias: lnwire.ShortChannelID{
				BlockHeight: 16_000_003,
				TxIndex:     3,
				TxPosition:  3,
			},
			aliceReal: lnwire.ShortChannelID{
				BlockHeight: 300000,
				TxIndex:     3,
				TxPosition:  3,
			},
			expectErr: false,
		},
		{
			name:         "private confirmed zero-conf failure",
			alicePrivate: true,
			zeroConf:     true,
			useAlias:     false,
			aliceAlias: lnwire.ShortChannelID{
				BlockHeight: 16_000_004,
				TxIndex:     4,
				TxPosition:  4,
			},
			aliceReal: lnwire.ShortChannelID{
				BlockHeight: 300002,
				TxIndex:     4,
				TxPosition:  4,
			},
			expectErr: true,
		},
		{
			name:         "public unconfirmed zero-conf",
			alicePrivate: false,
			zeroConf:     true,
			useAlias:     true,
			aliceAlias: lnwire.ShortChannelID{
				BlockHeight: 16_000_005,
				TxIndex:     5,
				TxPosition:  5,
			},
			aliceReal: lnwire.ShortChannelID{},
			expectErr: false,
		},
		{
			name:         "public confirmed zero-conf w/ alias",
			alicePrivate: false,
			zeroConf:     true,
			useAlias:     true,
			aliceAlias: lnwire.ShortChannelID{
				BlockHeight: 16_000_006,
				TxIndex:     6,
				TxPosition:  6,
			},
			aliceReal: lnwire.ShortChannelID{
				BlockHeight: 500000,
				TxIndex:     6,
				TxPosition:  6,
			},
			expectErr: false,
		},
		{
			name:         "public confirmed zero-conf w/ real",
			alicePrivate: false,
			zeroConf:     true,
			useAlias:     false,
			aliceAlias: lnwire.ShortChannelID{
				BlockHeight: 16_000_007,
				TxIndex:     7,
				TxPosition:  7,
			},
			aliceReal: lnwire.ShortChannelID{
				BlockHeight: 502000,
				TxIndex:     7,
				TxPosition:  7,
			},
			expectErr: false,
		},
		{
			name:         "private non-option channel",
			alicePrivate: true,
			aliceAlias:   lnwire.ShortChannelID{},
			aliceReal: lnwire.ShortChannelID{
				BlockHeight: 505000,
				TxIndex:     8,
				TxPosition:  8,
			},
		},
		{
			name:         "private option channel w/ alias",
			alicePrivate: true,
			optionScid:   true,
			useAlias:     true,
			aliceAlias: lnwire.ShortChannelID{
				BlockHeight: 16_000_015,
				TxIndex:     9,
				TxPosition:  9,
			},
			aliceReal: lnwire.ShortChannelID{
				BlockHeight: 506000,
				TxIndex:     10,
				TxPosition:  10,
			},
			expectErr: false,
		},
		{
			name:         "private option channel failure",
			alicePrivate: true,
			optionScid:   true,
			useAlias:     false,
			aliceAlias: lnwire.ShortChannelID{
				BlockHeight: 16_000_016,
				TxIndex:     16,
				TxPosition:  16,
			},
			aliceReal: lnwire.ShortChannelID{
				BlockHeight: 507000,
				TxIndex:     17,
				TxPosition:  17,
			},
			expectErr: true,
		},
		{
			name:         "public non-option channel",
			alicePrivate: false,
			useAlias:     false,
			aliceAlias:   lnwire.ShortChannelID{},
			aliceReal: lnwire.ShortChannelID{
				BlockHeight: 508000,
				TxIndex:     17,
				TxPosition:  17,
			},
			expectErr: false,
		},
		{
			name:         "public option channel w/ alias",
			alicePrivate: false,
			optionScid:   true,
			useAlias:     true,
			aliceAlias: lnwire.ShortChannelID{
				BlockHeight: 16_000_018,
				TxIndex:     18,
				TxPosition:  18,
			},
			aliceReal: lnwire.ShortChannelID{
				BlockHeight: 509000,
				TxIndex:     19,
				TxPosition:  19,
			},
			expectErr: false,
		},
		{
			name:         "public option channel w/ real",
			alicePrivate: false,
			optionScid:   true,
			useAlias:     false,
			aliceAlias: lnwire.ShortChannelID{
				BlockHeight: 16_000_019,
				TxIndex:     19,
				TxPosition:  19,
			},
			aliceReal: lnwire.ShortChannelID{
				BlockHeight: 510000,
				TxIndex:     20,
				TxPosition:  20,
			},
			expectErr: false,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testSwitchForwardMapping(
				t, test.alicePrivate, test.zeroConf,
				test.useAlias, test.optionScid,
				test.aliceAlias, test.aliceReal,
				test.expectErr,
			)
		})
	}
}

func testSwitchForwardMapping(t *testing.T, alicePrivate, aliceZeroConf,
	useAlias, optionScid bool, aliceAlias, aliceReal lnwire.ShortChannelID,
	expectErr bool) {

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err)
	err = s.Start()
	require.NoError(t, err)
	defer func() { _ = s.Stop() }()

	// Create the lnwire.ChannelIDs that we'll use.
	chanID1, chanID2, _, _ := genIDs()

	var aliceChannelLink *mockChannelLink

	if aliceZeroConf {
		aliceChannelLink = newMockChannelLink(
			s, chanID1, aliceAlias, aliceReal, alicePeer, true,
			alicePrivate, true, false,
		)
	} else {
		aliceChannelLink = newMockChannelLink(
			s, chanID1, aliceReal, emptyScid, alicePeer, true,
			alicePrivate, false, optionScid,
		)

		if optionScid {
			aliceChannelLink.addAlias(aliceAlias)
		}
	}

	err = s.AddLink(aliceChannelLink)
	require.NoError(t, err)

	// Bob will just have a non-option_scid_alias channel so no mapping is
	// necessary.
	bobScid := lnwire.ShortChannelID{
		BlockHeight: 501000,
		TxIndex:     200,
		TxPosition:  2,
	}

	bobChannelLink := newMockChannelLink(
		s, chanID2, bobScid, emptyScid, bobPeer, true, false, false,
		false,
	)
	err = s.AddLink(bobChannelLink)
	require.NoError(t, err)

	// Generate preimage.
	preimage, err := genPreimage()
	require.NoError(t, err, "unable to generate preimage")
	rhash := sha256.Sum256(preimage[:])

	// Determine the outgoing SCID to use.
	outgoingSCID := aliceReal
	if useAlias {
		outgoingSCID = aliceAlias
	}

	packet := &htlcPacket{
		incomingChanID: bobScid,
		incomingHTLCID: 0,
		outgoingChanID: outgoingSCID,
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}
	err = s.ForwardPackets(nil, packet)
	require.NoError(t, err)

	// If we expect a forwarding error, then assert that we receive one.
	// option_scid_alias forwards may fail if forwarding would be a privacy
	// leak.
	if expectErr {
		select {
		case <-bobChannelLink.packets:
		case <-time.After(time.Second * 5):
			t.Fatal("expected a forwarding error")
		}

		select {
		case <-aliceChannelLink.packets:
			t.Fatal("did not expect a packet")
		case <-time.After(time.Second * 5):
		}
	} else {
		select {
		case <-bobChannelLink.packets:
			t.Fatal("did not expect a forwarding error")
		case <-time.After(time.Second * 5):
		}

		select {
		case <-aliceChannelLink.packets:
		case <-time.After(time.Second * 5):
			t.Fatal("expected alice to receive packet")
		}
	}
}

// TestSwitchSendHTLCMapping tests that SendHTLC will properly route packets to
// zero-conf or option-scid-alias (feature-bit) channels if the confirmed SCID
// is used. It also tests that nothing breaks with the mapping change.
func TestSwitchSendHTLCMapping(t *testing.T) {
	tests := []struct {
		name string

		// If this is true, the channel will be zero-conf.
		zeroConf bool

		// Denotes whether the channel is option-scid-alias, non
		// zero-conf feature bit.
		optionFeature bool

		// If this is true, then the alias will be used in the packet.
		useAlias bool

		// This will be the channel alias if there is a mapping.
		alias lnwire.ShortChannelID

		// This will be the confirmed SCID if the channel is confirmed.
		real lnwire.ShortChannelID
	}{
		{
			name:          "non-zero-conf real scid w/ option",
			zeroConf:      false,
			optionFeature: true,
			useAlias:      false,
			alias: lnwire.ShortChannelID{
				BlockHeight: 10010,
				TxIndex:     10,
				TxPosition:  10,
			},
			real: lnwire.ShortChannelID{
				BlockHeight: 500000,
				TxIndex:     50,
				TxPosition:  50,
			},
		},
		{
			name:     "non-zero-conf real scid no option",
			zeroConf: false,
			useAlias: false,
			alias:    lnwire.ShortChannelID{},
			real: lnwire.ShortChannelID{
				BlockHeight: 400000,
				TxIndex:     50,
				TxPosition:  50,
			},
		},
		{
			name:     "zero-conf alias scid w/ conf",
			zeroConf: true,
			useAlias: true,
			alias: lnwire.ShortChannelID{
				BlockHeight: 10020,
				TxIndex:     20,
				TxPosition:  20,
			},
			real: lnwire.ShortChannelID{
				BlockHeight: 450000,
				TxIndex:     50,
				TxPosition:  50,
			},
		},
		{
			name:     "zero-conf alias scid no conf",
			zeroConf: true,
			useAlias: true,
			alias: lnwire.ShortChannelID{
				BlockHeight: 10015,
				TxIndex:     25,
				TxPosition:  35,
			},
			real: lnwire.ShortChannelID{},
		},
		{
			name:     "zero-conf real scid",
			zeroConf: true,
			useAlias: false,
			alias: lnwire.ShortChannelID{
				BlockHeight: 10035,
				TxIndex:     35,
				TxPosition:  35,
			},
			real: lnwire.ShortChannelID{
				BlockHeight: 470000,
				TxIndex:     35,
				TxPosition:  45,
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testSwitchSendHtlcMapping(
				t, test.zeroConf, test.useAlias, test.alias,
				test.real, test.optionFeature,
			)
		})
	}
}

func testSwitchSendHtlcMapping(t *testing.T, zeroConf, useAlias bool, alias,
	realScid lnwire.ShortChannelID, optionFeature bool) {

	peer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err)
	err = s.Start()
	require.NoError(t, err)
	defer func() { _ = s.Stop() }()

	// Create the lnwire.ChannelID that we'll use.
	chanID, _ := genID()

	var link *mockChannelLink

	if zeroConf {
		link = newMockChannelLink(
			s, chanID, alias, realScid, peer, true, false, true,
			false,
		)
	} else {
		link = newMockChannelLink(
			s, chanID, realScid, emptyScid, peer, true, false,
			false, true,
		)

		if optionFeature {
			link.addAlias(alias)
		}
	}

	err = s.AddLink(link)
	require.NoError(t, err)

	// Generate preimage.
	preimage, err := genPreimage()
	require.NoError(t, err)
	rhash := sha256.Sum256(preimage[:])

	// Determine the outgoing SCID to use.
	outgoingSCID := realScid
	if useAlias {
		outgoingSCID = alias
	}

	// Send the HTLC and assert that we don't get an error.
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      1,
	}

	err = s.SendHTLC(outgoingSCID, 0, htlc)
	require.NoError(t, err)
}

// TestSwitchUpdateScid verifies that zero-conf and non-zero-conf
// option-scid-alias (feature bit) channels will have the expected entries in
// the aliasToReal and baseIndex maps.
func TestSwitchUpdateScid(t *testing.T) {
	t.Parallel()

	peer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err)
	err = s.Start()
	require.NoError(t, err)
	defer func() { _ = s.Stop() }()

	// Create the IDs that we'll use.
	chanID, chanID2, _, _ := genIDs()

	alias := lnwire.ShortChannelID{
		BlockHeight: 16_000_000,
		TxIndex:     0,
		TxPosition:  0,
	}
	alias2 := alias
	alias2.TxPosition = 1

	realScid := lnwire.ShortChannelID{
		BlockHeight: 500000,
		TxIndex:     0,
		TxPosition:  0,
	}

	link := newMockChannelLink(
		s, chanID, alias, emptyScid, peer, true, false, true, false,
	)
	link.addAlias(alias2)

	err = s.AddLink(link)
	require.NoError(t, err)

	// Assert that the zero-conf link does not have entries in the
	// aliasToReal map.
	s.indexMtx.RLock()
	_, ok := s.aliasToReal[alias]
	require.False(t, ok)
	_, ok = s.aliasToReal[alias2]
	require.False(t, ok)

	// Assert that both aliases point to the "base" SCID, which is actually
	// just the first alias.
	baseScid, ok := s.baseIndex[alias]
	require.True(t, ok)
	require.Equal(t, alias, baseScid)

	baseScid, ok = s.baseIndex[alias2]
	require.True(t, ok)
	require.Equal(t, alias, baseScid)

	s.indexMtx.RUnlock()

	// We'll set the mock link's confirmed SCID so that UpdateShortChanID
	// populates aliasToReal and adds an entry to baseIndex.
	link.realScid = realScid
	link.confirmedZC = true

	err = s.UpdateShortChanID(chanID)
	require.NoError(t, err)

	// Assert that aliasToReal is populated and there is an entry in
	// baseIndex for realScid.
	s.indexMtx.RLock()
	realMapping, ok := s.aliasToReal[alias]
	require.True(t, ok)
	require.Equal(t, realScid, realMapping)

	realMapping, ok = s.aliasToReal[alias2]
	require.True(t, ok)
	require.Equal(t, realScid, realMapping)

	baseScid, ok = s.baseIndex[realScid]
	require.True(t, ok)
	require.Equal(t, alias, baseScid)

	s.indexMtx.RUnlock()

	// Now we'll perform the same checks with a non-zero-conf
	// option-scid-alias channel (feature-bit).
	optionReal := lnwire.ShortChannelID{
		BlockHeight: 600000,
		TxIndex:     0,
		TxPosition:  0,
	}
	optionAlias := lnwire.ShortChannelID{
		BlockHeight: 12000,
		TxIndex:     0,
		TxPosition:  0,
	}
	optionAlias2 := optionAlias
	optionAlias2.TxPosition = 1
	link2 := newMockChannelLink(
		s, chanID2, optionReal, emptyScid, peer, true, false, false,
		true,
	)
	link2.addAlias(optionAlias)
	link2.addAlias(optionAlias2)

	err = s.AddLink(link2)
	require.NoError(t, err)

	// Assert that the option-scid-alias link does have entries in the
	// aliasToReal and baseIndex maps.
	s.indexMtx.RLock()
	realMapping, ok = s.aliasToReal[optionAlias]
	require.True(t, ok)
	require.Equal(t, optionReal, realMapping)

	realMapping, ok = s.aliasToReal[optionAlias2]
	require.True(t, ok)
	require.Equal(t, optionReal, realMapping)

	baseScid, ok = s.baseIndex[optionReal]
	require.True(t, ok)
	require.Equal(t, optionReal, baseScid)

	baseScid, ok = s.baseIndex[optionAlias]
	require.True(t, ok)
	require.Equal(t, optionReal, baseScid)

	baseScid, ok = s.baseIndex[optionAlias2]
	require.True(t, ok)
	require.Equal(t, optionReal, baseScid)

	s.indexMtx.RUnlock()
}

// TestSwitchForward checks the ability of htlc switch to forward add/settle
// requests.
func TestSwitchForward(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should be forwarded from Alice channel link to
	// bob channel link.
	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
	rhash := sha256.Sum256(preimage[:])
	packet := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.ForwardPackets(nil, packet); err != nil {
		t.Fatal(err)
	}

	select {
	case <-bobChannelLink.packets:
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumOpen() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	if !s.IsForwardedHTLC(bobChannelLink.ShortChanID(), 0) {
		t.Fatal("htlc should be identified as forwarded")
	}

	// Create settle request pretending that bob link handled the add htlc
	// request and sent the htlc settle request back. This request should
	// be forwarder back to Alice link.
	packet = &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc: &lnwire.UpdateFulfillHTLC{
			PaymentPreimage: preimage,
		},
	}

	// Handle the request and checks that payment circuit works properly.
	if err := s.ForwardPackets(nil, packet); err != nil {
		t.Fatal(err)
	}

	select {
	case pkt := <-aliceChannelLink.packets:
		if err := aliceChannelLink.deleteCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

func TestSwitchForwardFailAfterFullAdd(t *testing.T) {
	t.Parallel()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	tempPath := t.TempDir()

	cdb := channeldb.OpenForTesting(t, tempPath)

	s, err := initSwitchWithDB(testStartingHeight, cdb)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}

	// Even though we intend to Stop s later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s.Stop()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should be forwarded from Alice channel link to
	// bob channel link.
	preimage := [sha256.Size]byte{1}
	rhash := sha256.Sum256(preimage[:])
	ogPacket := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	if s.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.ForwardPackets(nil, ogPacket); err != nil {
		t.Fatal(err)
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Pull packet from bob's link, but do not perform a full add.
	select {
	case packet := <-bobChannelLink.packets:
		// Complete the payment circuit and assign the outgoing htlc id
		// before restarting.
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 1 {
		t.Fatalf("wrong amount of circuits")
	}

	// Now we will restart bob, leaving the forwarding decision for this
	// htlc is in the half-added state.
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}

	if err := cdb.Close(); err != nil {
		t.Fatal(err)
	}

	cdb2 := channeldb.OpenForTesting(t, tempPath)

	s2, err := initSwitchWithDB(testStartingHeight, cdb2)
	require.NoError(t, err, "unable reinit switch")
	if err := s2.Start(); err != nil {
		t.Fatalf("unable to restart switch: %v", err)
	}

	// Even though we intend to Stop s2 later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s2.Stop()

	aliceChannelLink = newMockChannelLink(
		s2, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink = newMockChannelLink(
		s2, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s2.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s2.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	if s2.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 1 {
		t.Fatalf("wrong amount of circuits")
	}

	// Craft a failure message from the remote peer.
	fail := &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc:           &lnwire.UpdateFailHTLC{},
	}

	// Send the fail packet from the remote peer through the switch.
	if err := s2.ForwardPackets(nil, fail); err != nil {
		t.Fatal(err)
	}

	// Pull packet from alice's link, as it should have gone through
	// successfully.
	select {
	case pkt := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	// Circuit map should be empty now.
	if s2.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Send the fail packet from the remote peer through the switch.
	if err := s.ForwardPackets(nil, fail); err != nil {
		t.Fatal(err)
	}
	select {
	case <-aliceChannelLink.packets:
		t.Fatalf("expected duplicate fail to not arrive at the destination")
	case <-time.After(time.Second):
	}
}

func TestSwitchForwardSettleAfterFullAdd(t *testing.T) {
	t.Parallel()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	tempPath := t.TempDir()

	cdb := channeldb.OpenForTesting(t, tempPath)

	s, err := initSwitchWithDB(testStartingHeight, cdb)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}

	// Even though we intend to Stop s later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s.Stop()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should be forwarded from Alice channel link to
	// bob channel link.
	preimage := [sha256.Size]byte{1}
	rhash := sha256.Sum256(preimage[:])
	ogPacket := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	if s.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.ForwardPackets(nil, ogPacket); err != nil {
		t.Fatal(err)
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Pull packet from bob's link, but do not perform a full add.
	select {
	case packet := <-bobChannelLink.packets:
		// Complete the payment circuit and assign the outgoing htlc id
		// before restarting.
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 1 {
		t.Fatalf("wrong amount of circuits")
	}

	// Now we will restart bob, leaving the forwarding decision for this
	// htlc is in the half-added state.
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}

	if err := cdb.Close(); err != nil {
		t.Fatal(err)
	}

	cdb2 := channeldb.OpenForTesting(t, tempPath)

	s2, err := initSwitchWithDB(testStartingHeight, cdb2)
	require.NoError(t, err, "unable reinit switch")
	if err := s2.Start(); err != nil {
		t.Fatalf("unable to restart switch: %v", err)
	}

	// Even though we intend to Stop s2 later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s2.Stop()

	aliceChannelLink = newMockChannelLink(
		s2, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink = newMockChannelLink(
		s2, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s2.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s2.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	if s2.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 1 {
		t.Fatalf("wrong amount of circuits")
	}

	// Craft a settle message from the remote peer.
	settle := &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc: &lnwire.UpdateFulfillHTLC{
			PaymentPreimage: preimage,
		},
	}

	// Send the settle packet from the remote peer through the switch.
	if err := s2.ForwardPackets(nil, settle); err != nil {
		t.Fatal(err)
	}

	// Pull packet from alice's link, as it should have gone through
	// successfully.
	select {
	case packet := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete circuit with in key=%s: %v",
				packet.inKey(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	// Circuit map should be empty now.
	if s2.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Send the settle packet again, which not arrive at destination.
	if err := s2.ForwardPackets(nil, settle); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bobChannelLink.packets:
		t.Fatalf("expected duplicate fail to not arrive at the destination")
	case <-time.After(time.Second):
	}
}

func TestSwitchForwardDropAfterFullAdd(t *testing.T) {
	t.Parallel()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	tempPath := t.TempDir()

	cdb := channeldb.OpenForTesting(t, tempPath)

	s, err := initSwitchWithDB(testStartingHeight, cdb)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}

	// Even though we intend to Stop s later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s.Stop()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should be forwarded from Alice channel link to
	// bob channel link.
	preimage := [sha256.Size]byte{1}
	rhash := sha256.Sum256(preimage[:])
	ogPacket := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	if s.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.ForwardPackets(nil, ogPacket); err != nil {
		t.Fatal(err)
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}

	// Pull packet from bob's link, but do not perform a full add.
	select {
	case packet := <-bobChannelLink.packets:
		// Complete the payment circuit and assign the outgoing htlc id
		// before restarting.
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	// Now we will restart bob, leaving the forwarding decision for this
	// htlc is in the half-added state.
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}

	if err := cdb.Close(); err != nil {
		t.Fatal(err)
	}

	cdb2 := channeldb.OpenForTesting(t, tempPath)

	s2, err := initSwitchWithDB(testStartingHeight, cdb2)
	require.NoError(t, err, "unable reinit switch")
	if err := s2.Start(); err != nil {
		t.Fatalf("unable to restart switch: %v", err)
	}

	// Even though we intend to Stop s2 later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s2.Stop()

	aliceChannelLink = newMockChannelLink(
		s2, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink = newMockChannelLink(
		s2, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s2.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s2.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	if s2.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}

	// Resend the failed htlc. The packet will be dropped silently since the
	// switch will detect that it has been half added previously.
	if err := s2.ForwardPackets(nil, ogPacket); err != nil {
		t.Fatal(err)
	}

	// After detecting an incomplete forward, the fail packet should have
	// been returned to the sender.
	select {
	case <-aliceChannelLink.packets:
		t.Fatal("request should not have returned to source")
	case <-bobChannelLink.packets:
		t.Fatal("request should not have forwarded to destination")
	case <-time.After(time.Second):
	}
}

func TestSwitchForwardFailAfterHalfAdd(t *testing.T) {
	t.Parallel()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	tempPath := t.TempDir()

	cdb := channeldb.OpenForTesting(t, tempPath)

	s, err := initSwitchWithDB(testStartingHeight, cdb)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}

	// Even though we intend to Stop s later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s.Stop()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should be forwarded from Alice channel link to
	// bob channel link.
	preimage := [sha256.Size]byte{1}
	rhash := sha256.Sum256(preimage[:])
	ogPacket := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	if s.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.ForwardPackets(nil, ogPacket); err != nil {
		t.Fatal(err)
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}

	// Pull packet from bob's link, but do not perform a full add.
	select {
	case <-bobChannelLink.packets:
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	// Now we will restart bob, leaving the forwarding decision for this
	// htlc is in the half-added state.
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}

	if err := cdb.Close(); err != nil {
		t.Fatal(err)
	}

	cdb2 := channeldb.OpenForTesting(t, tempPath)

	s2, err := initSwitchWithDB(testStartingHeight, cdb2)
	require.NoError(t, err, "unable reinit switch")
	if err := s2.Start(); err != nil {
		t.Fatalf("unable to restart switch: %v", err)
	}

	// Even though we intend to Stop s2 later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s2.Stop()

	aliceChannelLink = newMockChannelLink(
		s2, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink = newMockChannelLink(
		s2, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s2.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s2.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	if s2.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}

	// Resend the failed htlc, it should be returned to alice since the
	// switch will detect that it has been half added previously.
	err = s2.ForwardPackets(nil, ogPacket)
	if err != nil {
		t.Fatal(err)
	}

	// After detecting an incomplete forward, the fail packet should have
	// been returned to the sender.
	select {
	case pkt := <-aliceChannelLink.packets:
		linkErr := pkt.linkFailure
		if linkErr.FailureDetail != OutgoingFailureIncompleteForward {
			t.Fatalf("expected incomplete forward, got: %v",
				linkErr.FailureDetail)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}
}

// TestSwitchForwardCircuitPersistence checks the ability of htlc switch to
// maintain the proper entries in the circuit map in the face of restarts.
func TestSwitchForwardCircuitPersistence(t *testing.T) {
	t.Parallel()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	tempPath := t.TempDir()

	cdb := channeldb.OpenForTesting(t, tempPath)

	s, err := initSwitchWithDB(testStartingHeight, cdb)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}

	// Even though we intend to Stop s later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s.Stop()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should be forwarded from Alice channel link to
	// bob channel link.
	preimage := [sha256.Size]byte{1}
	rhash := sha256.Sum256(preimage[:])
	ogPacket := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	if s.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.ForwardPackets(nil, ogPacket); err != nil {
		t.Fatal(err)
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Retrieve packet from outgoing link and cache until after restart.
	var packet *htlcPacket
	select {
	case packet = <-bobChannelLink.packets:
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}

	if err := cdb.Close(); err != nil {
		t.Fatal(err)
	}

	cdb2 := channeldb.OpenForTesting(t, tempPath)

	s2, err := initSwitchWithDB(testStartingHeight, cdb2)
	require.NoError(t, err, "unable reinit switch")
	if err := s2.Start(); err != nil {
		t.Fatalf("unable to restart switch: %v", err)
	}

	// Even though we intend to Stop s2 later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s2.Stop()

	aliceChannelLink = newMockChannelLink(
		s2, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink = newMockChannelLink(
		s2, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s2.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s2.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	if s2.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}

	// Now that the switch has restarted, complete the payment circuit.
	if err := bobChannelLink.completeCircuit(packet); err != nil {
		t.Fatalf("unable to complete payment circuit: %v", err)
	}

	if s2.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	// Create settle request pretending that bob link handled the add htlc
	// request and sent the htlc settle request back. This request should
	// be forwarder back to Alice link.
	ogPacket = &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc: &lnwire.UpdateFulfillHTLC{
			PaymentPreimage: preimage,
		},
	}

	// Handle the request and checks that payment circuit works properly.
	if err := s2.ForwardPackets(nil, ogPacket); err != nil {
		t.Fatal(err)
	}

	select {
	case packet = <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete circuit with in key=%s: %v",
				packet.inKey(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s2.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits, want 1, got %d",
			s2.circuits.NumPending())
	}
	if s2.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}

	if err := s2.Stop(); err != nil {
		t.Fatal(err)
	}

	if err := cdb2.Close(); err != nil {
		t.Fatal(err)
	}

	cdb3 := channeldb.OpenForTesting(t, tempPath)

	s3, err := initSwitchWithDB(testStartingHeight, cdb3)
	require.NoError(t, err, "unable reinit switch")
	if err := s3.Start(); err != nil {
		t.Fatalf("unable to restart switch: %v", err)
	}
	defer s3.Stop()

	aliceChannelLink = newMockChannelLink(
		s3, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink = newMockChannelLink(
		s3, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s3.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s3.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	if s3.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s3.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}
}

type multiHopFwdTest struct {
	name                 string
	eligible1, eligible2 bool
	failure1, failure2   *LinkError
	expectedReply        lnwire.FailCode
}

// TestCircularForwards tests the allowing/disallowing of circular payments
// through the same channel in the case where the switch is configured to allow
// and disallow same channel circular forwards.
func TestCircularForwards(t *testing.T) {
	chanID1, aliceChanID := genID()
	preimage := [sha256.Size]byte{1}
	hash := sha256.Sum256(preimage[:])

	tests := []struct {
		name                 string
		allowCircularPayment bool
		expectedErr          error
	}{
		{
			name:                 "circular payment allowed",
			allowCircularPayment: true,
			expectedErr:          nil,
		},
		{
			name:                 "circular payment disallowed",
			allowCircularPayment: false,
			expectedErr: NewDetailedLinkError(
				lnwire.NewTemporaryChannelFailure(nil),
				OutgoingFailureCircularRoute,
			),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			alicePeer, err := newMockServer(
				t, "alice", testStartingHeight, nil,
				testDefaultDelta,
			)
			if err != nil {
				t.Fatalf("unable to create alice server: %v",
					err)
			}

			s, err := initSwitchWithTempDB(t, testStartingHeight)
			if err != nil {
				t.Fatalf("unable to init switch: %v", err)
			}
			if err := s.Start(); err != nil {
				t.Fatalf("unable to start switch: %v", err)
			}
			defer func() { _ = s.Stop() }()

			// Set the switch to allow or disallow circular routes
			// according to the test's requirements.
			s.cfg.AllowCircularRoute = test.allowCircularPayment

			aliceChannelLink := newMockChannelLink(
				s, chanID1, aliceChanID, emptyScid, alicePeer,
				true, false, false, false,
			)

			if err := s.AddLink(aliceChannelLink); err != nil {
				t.Fatalf("unable to add alice link: %v", err)
			}

			// Create a new packet that loops through alice's link
			// in a circle.
			obfuscator := NewMockObfuscator()
			packet := &htlcPacket{
				incomingChanID: aliceChannelLink.ShortChanID(),
				outgoingChanID: aliceChannelLink.ShortChanID(),
				htlc: &lnwire.UpdateAddHTLC{
					PaymentHash: hash,
					Amount:      1,
				},
				obfuscator: obfuscator,
			}

			// Attempt to forward the packet and check for the expected
			// error.
			if err = s.ForwardPackets(nil, packet); err != nil {
				t.Fatal(err)
			}
			select {
			case p := <-aliceChannelLink.packets:
				if p.linkFailure != nil {
					err = p.linkFailure
				}
			case <-time.After(time.Second):
				t.Fatal("no timely reply from switch")
			}
			if !reflect.DeepEqual(err, test.expectedErr) {
				t.Fatalf("expected: %v, got: %v",
					test.expectedErr, err)
			}

			// Ensure that no circuits were opened.
			if s.circuits.NumOpen() > 0 {
				t.Fatal("do not expect any open circuits")
			}
		})
	}
}

// TestCheckCircularForward tests the error returned by checkCircularForward
// in cases where we allow and disallow same channel circular forwards.
func TestCheckCircularForward(t *testing.T) {
	tests := []struct {
		name string

		// aliasMapping determines whether the test should add an alias
		// mapping to Switch alias maps before checkCircularForward.
		aliasMapping bool

		// allowCircular determines whether we should allow circular
		// forwards.
		allowCircular bool

		// incomingLink is the link that the htlc arrived on.
		incomingLink lnwire.ShortChannelID

		// outgoingLink is the link that the htlc forward
		// is destined to leave on.
		outgoingLink lnwire.ShortChannelID

		// expectedErr is the error we expect to be returned.
		expectedErr *LinkError
	}{
		{
			name:          "not circular, allowed in config",
			aliasMapping:  false,
			allowCircular: true,
			incomingLink:  lnwire.NewShortChanIDFromInt(123),
			outgoingLink:  lnwire.NewShortChanIDFromInt(321),
			expectedErr:   nil,
		},
		{
			name:          "not circular, not allowed in config",
			aliasMapping:  false,
			allowCircular: false,
			incomingLink:  lnwire.NewShortChanIDFromInt(123),
			outgoingLink:  lnwire.NewShortChanIDFromInt(321),
			expectedErr:   nil,
		},
		{
			name:          "circular, allowed in config",
			aliasMapping:  false,
			allowCircular: true,
			incomingLink:  lnwire.NewShortChanIDFromInt(123),
			outgoingLink:  lnwire.NewShortChanIDFromInt(123),
			expectedErr:   nil,
		},
		{
			name:          "circular, not allowed in config",
			aliasMapping:  false,
			allowCircular: false,
			incomingLink:  lnwire.NewShortChanIDFromInt(123),
			outgoingLink:  lnwire.NewShortChanIDFromInt(123),
			expectedErr: NewDetailedLinkError(
				lnwire.NewTemporaryChannelFailure(nil),
				OutgoingFailureCircularRoute,
			),
		},
		{
			name:          "circular with map, not allowed",
			aliasMapping:  true,
			allowCircular: false,
			incomingLink:  lnwire.NewShortChanIDFromInt(1 << 60),
			outgoingLink:  lnwire.NewShortChanIDFromInt(1 << 55),
			expectedErr: NewDetailedLinkError(
				lnwire.NewTemporaryChannelFailure(nil),
				OutgoingFailureCircularRoute,
			),
		},
		{
			name:          "circular with map, not allowed 2",
			aliasMapping:  true,
			allowCircular: false,
			incomingLink:  lnwire.NewShortChanIDFromInt(1 << 55),
			outgoingLink:  lnwire.NewShortChanIDFromInt(1 << 60),
			expectedErr: NewDetailedLinkError(
				lnwire.NewTemporaryChannelFailure(nil),
				OutgoingFailureCircularRoute,
			),
		},
		{
			name:          "circular with map, allowed",
			aliasMapping:  true,
			allowCircular: true,
			incomingLink:  lnwire.NewShortChanIDFromInt(1 << 60),
			outgoingLink:  lnwire.NewShortChanIDFromInt(1 << 55),
			expectedErr:   nil,
		},
		{
			name:          "circular with map, allowed 2",
			aliasMapping:  true,
			allowCircular: true,
			incomingLink:  lnwire.NewShortChanIDFromInt(1 << 55),
			outgoingLink:  lnwire.NewShortChanIDFromInt(1 << 61),
			expectedErr:   nil,
		},
		{
			name:          "not circular, both confirmed SCID",
			aliasMapping:  false,
			allowCircular: false,
			incomingLink:  lnwire.NewShortChanIDFromInt(1 << 60),
			outgoingLink:  lnwire.NewShortChanIDFromInt(1 << 61),
			expectedErr:   nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			s, err := initSwitchWithTempDB(t, testStartingHeight)
			require.NoError(t, err)
			err = s.Start()
			require.NoError(t, err)
			defer func() { _ = s.Stop() }()

			if test.aliasMapping {
				// Make the incoming and outgoing point to the
				// same base SCID.
				inScid := test.incomingLink
				outScid := test.outgoingLink
				s.indexMtx.Lock()
				s.baseIndex[inScid] = outScid
				s.baseIndex[outScid] = outScid
				s.indexMtx.Unlock()
			}

			// Check for a circular forward, the hash passed can
			// be nil because it is only used for logging.
			err = s.checkCircularForward(
				test.incomingLink, test.outgoingLink,
				test.allowCircular, lntypes.Hash{},
			)
			if !reflect.DeepEqual(err, test.expectedErr) {
				t.Fatalf("expected: %v, got: %v",
					test.expectedErr, err)
			}
		})
	}
}

// TestSkipIneligibleLinksMultiHopForward tests that if a multi-hop HTLC comes
// along, then we won't attempt to forward it down al ink that isn't yet able
// to forward any HTLC's.
func TestSkipIneligibleLinksMultiHopForward(t *testing.T) {
	tests := []multiHopFwdTest{
		// None of the channels is eligible.
		{
			name:          "not eligible",
			expectedReply: lnwire.CodeUnknownNextPeer,
		},

		// Channel one has a policy failure and the other channel isn't
		// available.
		{
			name:      "policy fail",
			eligible1: true,
			failure1: NewLinkError(
				lnwire.NewFinalIncorrectCltvExpiry(0),
			),
			expectedReply: lnwire.CodeFinalIncorrectCltvExpiry,
		},

		// The requested channel is not eligible, but the packet is
		// forwarded through the other channel.
		{
			name:          "non-strict success",
			eligible2:     true,
			expectedReply: lnwire.CodeNone,
		},

		// The requested channel has insufficient bandwidth and the
		// other channel's policy isn't satisfied.
		{
			name:      "non-strict policy fail",
			eligible1: true,
			failure1: NewDetailedLinkError(
				lnwire.NewTemporaryChannelFailure(nil),
				OutgoingFailureInsufficientBalance,
			),
			eligible2: true,
			failure2: NewLinkError(
				lnwire.NewFinalIncorrectCltvExpiry(0),
			),
			expectedReply: lnwire.CodeTemporaryChannelFailure,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testSkipIneligibleLinksMultiHopForward(t, &test)
		})
	}
}

// testSkipIneligibleLinksMultiHopForward tests that if a multi-hop HTLC comes
// along, then we won't attempt to forward it down al ink that isn't yet able
// to forward any HTLC's.
func testSkipIneligibleLinksMultiHopForward(t *testing.T,
	testCase *multiHopFwdTest) {

	t.Parallel()

	var packet *htlcPacket

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, aliceChanID := genID()
	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)

	// We'll create a link for Bob, but mark the link as unable to forward
	// any new outgoing HTLC's.
	chanID2, bobChanID2 := genID()
	bobChannelLink1 := newMockChannelLink(
		s, chanID2, bobChanID2, emptyScid, bobPeer, testCase.eligible1,
		false, false, false,
	)
	bobChannelLink1.checkHtlcForwardResult = testCase.failure1

	chanID3, bobChanID3 := genID()
	bobChannelLink2 := newMockChannelLink(
		s, chanID3, bobChanID3, emptyScid, bobPeer, testCase.eligible2,
		false, false, false,
	)
	bobChannelLink2.checkHtlcForwardResult = testCase.failure2

	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink1); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}
	if err := s.AddLink(bobChannelLink2); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create a new packet that's destined for Bob as an incoming HTLC from
	// Alice.
	preimage := [sha256.Size]byte{1}
	rhash := sha256.Sum256(preimage[:])
	obfuscator := NewMockObfuscator()
	packet = &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink1.ShortChanID(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
		obfuscator: obfuscator,
	}

	// The request to forward should fail as
	if err := s.ForwardPackets(nil, packet); err != nil {
		t.Fatal(err)
	}

	// We select from all links and extract the error if exists.
	// The packet must be selected but we don't always expect a link error.
	var linkError *LinkError
	select {
	case p := <-aliceChannelLink.packets:
		linkError = p.linkFailure
	case p := <-bobChannelLink1.packets:
		linkError = p.linkFailure
	case p := <-bobChannelLink2.packets:
		linkError = p.linkFailure
	case <-time.After(time.Second):
		t.Fatal("no timely reply from switch")
	}
	failure := obfuscator.(*mockObfuscator).failure
	if testCase.expectedReply == lnwire.CodeNone {
		if linkError != nil {
			t.Fatalf("forwarding should have succeeded")
		}
		if failure != nil {
			t.Fatalf("unexpected failure %T", failure)
		}
	} else {
		if linkError == nil {
			t.Fatalf("forwarding should have failed due to " +
				"inactive link")
		}
		if failure.Code() != testCase.expectedReply {
			t.Fatalf("unexpected failure %T", failure)
		}
	}

	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSkipIneligibleLinksLocalForward ensures that the switch will not attempt
// to forward any HTLC's down a link that isn't yet eligible for forwarding.
func TestSkipIneligibleLinksLocalForward(t *testing.T) {
	t.Parallel()

	testSkipLinkLocalForward(t, false, nil)
}

// TestSkipPolicyUnsatisfiedLinkLocalForward ensures that the switch will not
// attempt to send locally initiated HTLCs that would violate the channel policy
// down a link.
func TestSkipPolicyUnsatisfiedLinkLocalForward(t *testing.T) {
	t.Parallel()

	testSkipLinkLocalForward(t, true, lnwire.NewTemporaryChannelFailure(nil))
}

func testSkipLinkLocalForward(t *testing.T, eligible bool,
	policyResult lnwire.FailureMessage) {

	// We'll create a single link for this test, marking it as being unable
	// to forward form the get go.
	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, _, aliceChanID, _ := genIDs()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, eligible, false,
		false, false,
	)
	aliceChannelLink.checkHtlcTransitResult = NewLinkError(
		policyResult,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}

	preimage, err := genPreimage()
	require.NoError(t, err, "unable to generate preimage")
	rhash := sha256.Sum256(preimage[:])
	addMsg := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      1,
	}

	// We'll attempt to send out a new HTLC that has Alice as the first
	// outgoing link. This should fail as Alice isn't yet able to forward
	// any active HTLC's.
	err = s.SendHTLC(aliceChannelLink.ShortChanID(), 0, addMsg)
	if err == nil {
		t.Fatalf("local forward should fail due to inactive link")
	}

	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSwitchCancel checks that if htlc was rejected we remove unused
// circuits.
func TestSwitchCancel(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should be forwarder from alice channel link
	// to bob channel link.
	preimage, err := genPreimage()
	require.NoError(t, err, "unable to generate preimage")
	rhash := sha256.Sum256(preimage[:])
	request := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.ForwardPackets(nil, request); err != nil {
		t.Fatal(err)
	}

	select {
	case packet := <-bobChannelLink.packets:
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	// Create settle request pretending that bob channel link handled
	// the add htlc request and sent the htlc settle request back. This
	// request should be forwarder back to alice channel link.
	request = &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc:           &lnwire.UpdateFailHTLC{},
	}

	// Handle the request and checks that payment circuit works properly.
	if err := s.ForwardPackets(nil, request); err != nil {
		t.Fatal(err)
	}

	select {
	case pkt := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.NumPending() != 0 {
		t.Fatal("wrong amount of circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSwitchAddSamePayment tests that we send the payment with the same
// payment hash.
func TestSwitchAddSamePayment(t *testing.T) {
	t.Parallel()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should be forwarder from alice channel link
	// to bob channel link.
	preimage, err := genPreimage()
	require.NoError(t, err, "unable to generate preimage")
	rhash := sha256.Sum256(preimage[:])
	request := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.ForwardPackets(nil, request); err != nil {
		t.Fatal(err)
	}

	select {
	case packet := <-bobChannelLink.packets:
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumOpen() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	request = &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 1,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.ForwardPackets(nil, request); err != nil {
		t.Fatal(err)
	}

	select {
	case packet := <-bobChannelLink.packets:
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumOpen() != 2 {
		t.Fatal("wrong amount of circuits")
	}

	// Create settle request pretending that bob channel link handled
	// the add htlc request and sent the htlc settle request back. This
	// request should be forwarder back to alice channel link.
	request = &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc:           &lnwire.UpdateFailHTLC{},
	}

	// Handle the request and checks that payment circuit works properly.
	if err := s.ForwardPackets(nil, request); err != nil {
		t.Fatal(err)
	}

	select {
	case pkt := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.NumOpen() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	request = &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 1,
		amount:         1,
		htlc:           &lnwire.UpdateFailHTLC{},
	}

	// Handle the request and checks that payment circuit works properly.
	if err := s.ForwardPackets(nil, request); err != nil {
		t.Fatal(err)
	}

	select {
	case pkt := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSwitchSendPayment tests ability of htlc switch to respond to the
// users when response is came back from channel link.
func TestSwitchSendPayment(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, _, aliceChanID, _ := genIDs()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add link: %v", err)
	}

	// Create request which should be forwarder from alice channel link
	// to bob channel link.
	preimage, err := genPreimage()
	require.NoError(t, err, "unable to generate preimage")
	rhash := sha256.Sum256(preimage[:])
	update := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      1,
	}
	paymentID := uint64(123)

	// First check that the switch will correctly respond that this payment
	// ID is unknown.
	_, err = s.GetAttemptResult(
		paymentID, rhash, newMockDeobfuscator(),
	)
	if err != ErrPaymentIDNotFound {
		t.Fatalf("expected ErrPaymentIDNotFound, got %v", err)
	}

	// Handle the request and checks that bob channel link received it.
	errChan := make(chan error)
	go func() {
		err := s.SendHTLC(
			aliceChannelLink.ShortChanID(), paymentID, update,
		)
		if err != nil {
			errChan <- err
			return
		}

		resultChan, err := s.GetAttemptResult(
			paymentID, rhash, newMockDeobfuscator(),
		)
		if err != nil {
			errChan <- err
			return
		}

		result, ok := <-resultChan
		if !ok {
			errChan <- fmt.Errorf("shutting down")
		}

		if result.Error != nil {
			errChan <- result.Error
			return
		}

		errChan <- nil
	}()

	select {
	case packet := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case err := <-errChan:
		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumOpen() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	// Create fail request pretending that bob channel link handled
	// the add htlc request with error and sent the htlc fail request
	// back. This request should be forwarded back to alice channel link.
	obfuscator := NewMockObfuscator()
	failure := lnwire.NewFailIncorrectDetails(update.Amount, 100)
	reason, err := obfuscator.EncryptFirstHop(failure)
	require.NoError(t, err, "unable obfuscate failure")

	if s.IsForwardedHTLC(aliceChannelLink.ShortChanID(), update.ID) {
		t.Fatal("htlc should be identified as not forwarded")
	}
	packet := &htlcPacket{
		outgoingChanID: aliceChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc: &lnwire.UpdateFailHTLC{
			Reason: reason,
		},
	}

	if err := s.ForwardPackets(nil, packet); err != nil {
		t.Fatalf("can't forward htlc packet: %v", err)
	}

	select {
	case err := <-errChan:
		assertFailureCode(
			t, err, lnwire.CodeIncorrectOrUnknownPaymentDetails,
		)
	case <-time.After(time.Second):
		t.Fatal("err wasn't received")
	}
}

// TestLocalPaymentNoForwardingEvents tests that if we send a series of locally
// initiated payments, then they aren't reflected in the forwarding log.
func TestLocalPaymentNoForwardingEvents(t *testing.T) {
	t.Parallel()

	// First, we'll create our traditional three hop network. We'll only be
	// interacting with and asserting the state of the first end point for
	// this test.
	channels, _, err := createClusterChannels(
		t, btcutil.SatoshiPerBitcoin*3, btcutil.SatoshiPerBitcoin*5,
	)
	require.NoError(t, err, "unable to create channel")

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}

	// We'll now craft and send a payment from Alice to Bob.
	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	htlcAmt, totalTimelock, hops := generateHops(
		amount, testStartingHeight, n.firstBobChannelLink,
	)

	// With the payment crafted, we'll send it from Alice to Bob. We'll
	// wait for Alice to receive the preimage for the payment before
	// proceeding.
	receiver := n.bobServer
	firstHop := n.firstBobChannelLink.ShortChanID()
	_, err = makePayment(
		n.aliceServer, receiver, firstHop, hops, amount, htlcAmt,
		totalTimelock,
	).Wait(30 * time.Second)
	require.NoError(t, err, "unable to make the payment")

	// At this point, we'll forcibly stop the three hop network. Doing
	// this will cause any pending forwarding events to be flushed by the
	// various switches in the network.
	n.stop()

	// With all the switches stopped, we'll fetch Alice's mock forwarding
	// event log.
	log, ok := n.aliceServer.htlcSwitch.cfg.FwdingLog.(*mockForwardingLog)
	if !ok {
		t.Fatalf("mockForwardingLog assertion failed")
	}
	log.Lock()
	defer log.Unlock()

	// If we examine the memory of the forwarding log, then it should be
	// blank.
	if len(log.events) != 0 {
		t.Fatalf("log should have no events, instead has: %v",
			spew.Sdump(log.events))
	}
}

// TestMultiHopPaymentForwardingEvents tests that if we send a series of
// multi-hop payments via Alice->Bob->Carol. Then Bob properly logs forwarding
// events, while Alice and Carol don't.
func TestMultiHopPaymentForwardingEvents(t *testing.T) {
	t.Parallel()

	// First, we'll create our traditional three hop network.
	channels, _, err := createClusterChannels(
		t, btcutil.SatoshiPerBitcoin*3, btcutil.SatoshiPerBitcoin*5,
	)
	require.NoError(t, err, "unable to create channel")

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}

	// We'll make now 10 payments, of 100k satoshis each from Alice to
	// Carol via Bob.
	const numPayments = 10
	finalAmt := lnwire.NewMSatFromSatoshis(100000)
	htlcAmt, totalTimelock, hops := generateHops(
		finalAmt, testStartingHeight, n.firstBobChannelLink,
		n.carolChannelLink,
	)
	firstHop := n.firstBobChannelLink.ShortChanID()
	for i := 0; i < numPayments/2; i++ {
		_, err := makePayment(
			n.aliceServer, n.carolServer, firstHop, hops, finalAmt,
			htlcAmt, totalTimelock,
		).Wait(30 * time.Second)
		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
	}

	bobLog, ok := n.bobServer.htlcSwitch.cfg.FwdingLog.(*mockForwardingLog)
	if !ok {
		t.Fatalf("mockForwardingLog assertion failed")
	}

	// After sending 5 of the payments, trigger the forwarding ticker, to
	// make sure the events are properly flushed.
	bobTicker, ok := n.bobServer.htlcSwitch.cfg.FwdEventTicker.(*ticker.Force)
	if !ok {
		t.Fatalf("mockTicker assertion failed")
	}

	// We'll trigger the ticker, and wait for the events to appear in Bob's
	// forwarding log.
	timeout := time.After(15 * time.Second)
	for {
		select {
		case bobTicker.Force <- time.Now():
		case <-time.After(1 * time.Second):
			t.Fatalf("unable to force tick")
		}

		// If all 5 events is found in Bob's log, we can break out and
		// continue the test.
		bobLog.Lock()
		if len(bobLog.events) == 5 {
			bobLog.Unlock()
			break
		}
		bobLog.Unlock()

		// Otherwise wait a little bit before checking again.
		select {
		case <-time.After(50 * time.Millisecond):
		case <-timeout:
			bobLog.Lock()
			defer bobLog.Unlock()
			t.Fatalf("expected 5 events in event log, instead "+
				"found: %v", spew.Sdump(bobLog.events))
		}
	}

	// Send the remaining payments.
	for i := numPayments / 2; i < numPayments; i++ {
		_, err := makePayment(
			n.aliceServer, n.carolServer, firstHop, hops, finalAmt,
			htlcAmt, totalTimelock,
		).Wait(30 * time.Second)
		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
	}

	// With all 10 payments sent. We'll now manually stop each of the
	// switches so we can examine their end state.
	n.stop()

	// Alice and Carol shouldn't have any recorded forwarding events, as
	// they were the source and the sink for these payment flows.
	aliceLog, ok := n.aliceServer.htlcSwitch.cfg.FwdingLog.(*mockForwardingLog)
	if !ok {
		t.Fatalf("mockForwardingLog assertion failed")
	}
	aliceLog.Lock()
	defer aliceLog.Unlock()
	if len(aliceLog.events) != 0 {
		t.Fatalf("log should have no events, instead has: %v",
			spew.Sdump(aliceLog.events))
	}

	carolLog, ok := n.carolServer.htlcSwitch.cfg.FwdingLog.(*mockForwardingLog)
	if !ok {
		t.Fatalf("mockForwardingLog assertion failed")
	}
	carolLog.Lock()
	defer carolLog.Unlock()
	if len(carolLog.events) != 0 {
		t.Fatalf("log should have no events, instead has: %v",
			spew.Sdump(carolLog.events))
	}

	// Bob on the other hand, should have 10 events.
	bobLog.Lock()
	defer bobLog.Unlock()
	if len(bobLog.events) != 10 {
		t.Fatalf("log should have 10 events, instead has: %v",
			spew.Sdump(bobLog.events))
	}

	// Each of the 10 events should have had all fields set properly.
	for _, event := range bobLog.events {
		// The incoming and outgoing channels should properly be set for
		// the event.
		if event.IncomingChanID != n.aliceChannelLink.ShortChanID() {
			t.Fatalf("chan id mismatch: expected %v, got %v",
				event.IncomingChanID,
				n.aliceChannelLink.ShortChanID())
		}
		if event.OutgoingChanID != n.carolChannelLink.ShortChanID() {
			t.Fatalf("chan id mismatch: expected %v, got %v",
				event.OutgoingChanID,
				n.carolChannelLink.ShortChanID())
		}

		// Additionally, the incoming and outgoing amounts should also
		// be properly set.
		if event.AmtIn != htlcAmt {
			t.Fatalf("incoming amt mismatch: expected %v, got %v",
				event.AmtIn, htlcAmt)
		}
		if event.AmtOut != finalAmt {
			t.Fatalf("outgoing amt mismatch: expected %v, got %v",
				event.AmtOut, finalAmt)
		}
	}
}

// TestUpdateFailMalformedHTLCErrorConversion tests that we're able to properly
// convert malformed HTLC errors that originate at the direct link, as well as
// during multi-hop HTLC forwarding.
func TestUpdateFailMalformedHTLCErrorConversion(t *testing.T) {
	t.Parallel()

	// First, we'll create our traditional three hop network.
	channels, _, err := createClusterChannels(
		t, btcutil.SatoshiPerBitcoin*3, btcutil.SatoshiPerBitcoin*5,
	)
	require.NoError(t, err, "unable to create channel")

	n := newThreeHopNetwork(
		t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}

	assertPaymentFailure := func(t *testing.T) {
		// With the decoder modified, we'll now attempt to send a
		// payment from Alice to carol.
		finalAmt := lnwire.NewMSatFromSatoshis(100000)
		htlcAmt, totalTimelock, hops := generateHops(
			finalAmt, testStartingHeight, n.firstBobChannelLink,
			n.carolChannelLink,
		)
		firstHop := n.firstBobChannelLink.ShortChanID()
		_, err = makePayment(
			n.aliceServer, n.carolServer, firstHop, hops, finalAmt,
			htlcAmt, totalTimelock,
		).Wait(30 * time.Second)

		// The payment should fail as Carol is unable to decode the
		// onion blob sent to her.
		if err == nil {
			t.Fatalf("unable to send payment: %v", err)
		}

		routingErr := err.(ClearTextError)
		failureMsg := routingErr.WireMessage()
		if _, ok := failureMsg.(*lnwire.FailInvalidOnionKey); !ok {
			t.Fatalf("expected onion failure instead got: %v",
				routingErr.WireMessage())
		}
	}

	t.Run("multi-hop error conversion", func(t *testing.T) {
		// Now that we have our network up, we'll modify the hop
		// iterator for the Bob <-> Carol channel to fail to decode in
		// order to simulate either a replay attack or an issue
		// decoding the onion.
		n.carolOnionDecoder.decodeFail = true

		assertPaymentFailure(t)
	})

	t.Run("direct channel error conversion", func(t *testing.T) {
		// Similar to the above test case, we'll now make the Alice <->
		// Bob link always fail to decode an onion. This differs from
		// the above test case in that there's no encryption on the
		// error at all since Alice will directly receive a
		// UpdateFailMalformedHTLC message.
		n.bobOnionDecoder.decodeFail = true

		assertPaymentFailure(t)
	})
}

// TestSwitchGetAttemptResult tests that the switch interacts as expected with
// the circuit map and network result store when looking up the result of a
// payment ID. This is important for not to lose results under concurrent
// lookup and receiving results.
func TestSwitchGetAttemptResult(t *testing.T) {
	t.Parallel()

	const paymentID = 123
	var preimg lntypes.Preimage
	preimg[0] = 3

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	lookup := make(chan *PaymentCircuit, 1)
	s.circuits = &mockCircuitMap{
		lookup: lookup,
	}

	// If the payment circuit is not found in the circuit map, the payment
	// result must be found in the store if available. Since we haven't
	// added anything to the store yet, ErrPaymentIDNotFound should be
	// returned.
	lookup <- nil
	_, err = s.GetAttemptResult(
		paymentID, lntypes.Hash{}, newMockDeobfuscator(),
	)
	if err != ErrPaymentIDNotFound {
		t.Fatalf("expected ErrPaymentIDNotFound, got %v", err)
	}

	// Next let the lookup find the circuit in the circuit map. It should
	// subscribe to payment results, and return the result when available.
	lookup <- &PaymentCircuit{}
	resultChan, err := s.GetAttemptResult(
		paymentID, lntypes.Hash{}, newMockDeobfuscator(),
	)
	require.NoError(t, err, "unable to get payment result")

	// Add the result to the store.
	n := &networkResult{
		msg: &lnwire.UpdateFulfillHTLC{
			PaymentPreimage: preimg,
		},
		unencrypted:  true,
		isResolution: true,
	}

	err = s.networkResults.storeResult(paymentID, n)
	require.NoError(t, err, "unable to store result")

	// The result should be available.
	select {
	case res, ok := <-resultChan:
		if !ok {
			t.Fatalf("channel was closed")
		}

		if res.Error != nil {
			t.Fatalf("got unexpected error result")
		}

		if res.Preimage != preimg {
			t.Fatalf("expected preimg %v, got %v",
				preimg, res.Preimage)
		}

	case <-time.After(1 * time.Second):
		t.Fatalf("result not received")
	}

	// As a final test, try to get the result again. Now that is no longer
	// in the circuit map, it should be immediately available from the
	// store.
	lookup <- nil
	resultChan, err = s.GetAttemptResult(
		paymentID, lntypes.Hash{}, newMockDeobfuscator(),
	)
	require.NoError(t, err, "unable to get payment result")

	select {
	case res, ok := <-resultChan:
		if !ok {
			t.Fatalf("channel was closed")
		}

		if res.Error != nil {
			t.Fatalf("got unexpected error result")
		}

		if res.Preimage != preimg {
			t.Fatalf("expected preimg %v, got %v",
				preimg, res.Preimage)
		}

	case <-time.After(1 * time.Second):
		t.Fatalf("result not received")
	}
}

// TestInvalidFailure tests that the switch returns an unreadable failure error
// if the failure cannot be decrypted.
func TestInvalidFailure(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, _, aliceChanID, _ := genIDs()

	// Set up a mock channel link.
	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add link: %v", err)
	}

	// Create a request which should be forwarded to the mock channel link.
	preimage, err := genPreimage()
	require.NoError(t, err, "unable to generate preimage")
	rhash := sha256.Sum256(preimage[:])
	update := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      1,
	}

	paymentID := uint64(123)

	// Send the request.
	err = s.SendHTLC(
		aliceChannelLink.ShortChanID(), paymentID, update,
	)
	require.NoError(t, err, "unable to send payment")

	// Catch the packet and complete the circuit so that the switch is ready
	// for a response.
	select {
	case packet := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	// Send response packet with an unreadable failure message to the
	// switch. The reason failed is not relevant, because we mock the
	// decryption.
	packet := &htlcPacket{
		outgoingChanID: aliceChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc: &lnwire.UpdateFailHTLC{
			Reason: []byte{1, 2, 3},
		},
	}

	if err := s.ForwardPackets(nil, packet); err != nil {
		t.Fatalf("can't forward htlc packet: %v", err)
	}

	// Get payment result from switch. We expect an unreadable failure
	// message error.
	deobfuscator := SphinxErrorDecrypter{
		OnionErrorDecrypter: &mockOnionErrorDecryptor{
			err: ErrUnreadableFailureMessage,
		},
	}

	resultChan, err := s.GetAttemptResult(
		paymentID, rhash, &deobfuscator,
	)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case result := <-resultChan:
		if result.Error != ErrUnreadableFailureMessage {
			t.Fatal("expected unreadable failure message")
		}

	case <-time.After(time.Second):
		t.Fatal("err wasn't received")
	}

	// Modify the decryption to simulate that decryption went alright, but
	// the failure cannot be decoded.
	deobfuscator = SphinxErrorDecrypter{
		OnionErrorDecrypter: &mockOnionErrorDecryptor{
			sourceIdx: 2,
			message:   []byte{200},
		},
	}

	resultChan, err = s.GetAttemptResult(
		paymentID, rhash, &deobfuscator,
	)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case result := <-resultChan:
		rtErr, ok := result.Error.(ClearTextError)
		if !ok {
			t.Fatal("expected ClearTextError")
		}
		source, ok := rtErr.(*ForwardingError)
		if !ok {
			t.Fatalf("expected forwarding error, got: %T", rtErr)
		}
		if source.FailureSourceIdx != 2 {
			t.Fatal("unexpected error source index")
		}
		if rtErr.WireMessage() != nil {
			t.Fatal("expected empty failure message")
		}

	case <-time.After(time.Second):
		t.Fatal("err wasn't received")
	}
}

// htlcNotifierEvents is a function that generates a set of expected htlc
// notifier evetns for each node in a three hop network with the dynamic
// values provided. These functions take dynamic values so that changes to
// external systems (such as our default timelock delta) do not break
// these tests.
type htlcNotifierEvents func(channels *clusterChannels, htlcID uint64,
	ts time.Time, htlc *lnwire.UpdateAddHTLC,
	hops []*hop.Payload,
	preimage *lntypes.Preimage) ([]interface{}, []interface{}, []interface{})

// TestHtlcNotifier tests the notifying of htlc events that are routed over a
// three hop network. It sets up an Alice -> Bob -> Carol network and routes
// payments from Alice -> Carol to test events from the perspective of a
// sending (Alice), forwarding (Bob) and receiving (Carol) node. Test cases
// are present for saduccessful and failed payments.
func TestHtlcNotifier(t *testing.T) {
	tests := []struct {
		name string

		// Options is a set of options to apply to the three hop
		// network's servers.
		options []serverOption

		// expectedEvents is a function which returns an expected set
		// of events for the test.
		expectedEvents htlcNotifierEvents

		// iterations is the number of times we will send a payment,
		// this is used to send more than one payment to force non-
		// zero htlc indexes to make sure we aren't just checking
		// default values.
		iterations int
	}{
		{
			name:    "successful three hop payment",
			options: nil,
			expectedEvents: func(channels *clusterChannels,
				htlcID uint64, ts time.Time,
				htlc *lnwire.UpdateAddHTLC,
				hops []*hop.Payload,
				preimage *lntypes.Preimage) ([]interface{},
				[]interface{}, []interface{}) {

				return getThreeHopEvents(
					channels, htlcID, ts, htlc, hops, nil, preimage,
				)
			},
			iterations: 2,
		},
		{
			name: "failed at forwarding link",
			// Set a functional option which disables bob as a
			// forwarding node to force a payment error.
			options: []serverOption{
				serverOptionRejectHtlc(false, true, false),
			},
			expectedEvents: func(channels *clusterChannels,
				htlcID uint64, ts time.Time,
				htlc *lnwire.UpdateAddHTLC,
				hops []*hop.Payload,
				preimage *lntypes.Preimage) ([]interface{},
				[]interface{}, []interface{}) {

				return getThreeHopEvents(
					channels, htlcID, ts, htlc, hops,
					&LinkError{
						msg:           &lnwire.FailChannelDisabled{},
						FailureDetail: OutgoingFailureForwardsDisabled,
					},
					preimage,
				)
			},
			iterations: 1,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			testHtcNotifier(
				t, test.options, test.iterations,
				test.expectedEvents,
			)
		})
	}
}

// testHtcNotifier runs a htlc notifier test.
func testHtcNotifier(t *testing.T, testOpts []serverOption, iterations int,
	getEvents htlcNotifierEvents) {

	t.Parallel()

	// First, we'll create our traditional three hop
	// network.
	channels, _, err := createClusterChannels(
		t, btcutil.SatoshiPerBitcoin*3, btcutil.SatoshiPerBitcoin*5,
	)
	require.NoError(t, err, "unable to create channel")

	// Mock time so that all events are reported with a static timestamp.
	now := time.Now()
	mockTime := func() time.Time {
		return now
	}

	// Create htlc notifiers for each server in the three hop network and
	// start them.
	aliceNotifier := NewHtlcNotifier(mockTime)
	if err := aliceNotifier.Start(); err != nil {
		t.Fatalf("could not start alice notifier")
	}
	t.Cleanup(func() {
		if err := aliceNotifier.Stop(); err != nil {
			t.Fatalf("failed to stop alice notifier: %v", err)
		}
	})

	bobNotifier := NewHtlcNotifier(mockTime)
	if err := bobNotifier.Start(); err != nil {
		t.Fatalf("could not start bob notifier")
	}
	t.Cleanup(func() {
		if err := bobNotifier.Stop(); err != nil {
			t.Fatalf("failed to stop bob notifier: %v", err)
		}
	})

	carolNotifier := NewHtlcNotifier(mockTime)
	if err := carolNotifier.Start(); err != nil {
		t.Fatalf("could not start carol notifier")
	}
	t.Cleanup(func() {
		if err := carolNotifier.Stop(); err != nil {
			t.Fatalf("failed to stop carol notifier: %v", err)
		}
	})

	// Create a notifier server option which will set our htlc notifiers
	// for the three hop network.
	notifierOption := serverOptionWithHtlcNotifier(
		aliceNotifier, bobNotifier, carolNotifier,
	)

	// Add the htlcNotifier option to any other options
	// set in the test.
	options := append(testOpts, notifierOption) // nolint:gocritic

	n := newThreeHopNetwork(
		t, channels.aliceToBob,
		channels.bobToAlice, channels.bobToCarol,
		channels.carolToBob, testStartingHeight,
		options...,
	)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop "+
			"network: %v", err)
	}
	t.Cleanup(n.stop)

	// Before we forward anything, subscribe to htlc events
	// from each notifier.
	aliceEvents, err := aliceNotifier.SubscribeHtlcEvents()
	if err != nil {
		t.Fatalf("could not subscribe to alice's"+
			" events: %v", err)
	}
	t.Cleanup(aliceEvents.Cancel)

	bobEvents, err := bobNotifier.SubscribeHtlcEvents()
	if err != nil {
		t.Fatalf("could not subscribe to bob's"+
			" events: %v", err)
	}
	t.Cleanup(bobEvents.Cancel)

	carolEvents, err := carolNotifier.SubscribeHtlcEvents()
	if err != nil {
		t.Fatalf("could not subscribe to carol's"+
			" events: %v", err)
	}
	t.Cleanup(carolEvents.Cancel)

	// Send multiple payments, as specified by the test to test incrementing
	// of htlc ids.
	for i := 0; i < iterations; i++ {
		// We'll start off by making a payment from
		// Alice -> Bob -> Carol. The preimage, generated
		// by Carol's Invoice is expected in the Settle events
		htlc, hops, preimage := n.sendThreeHopPayment(t)

		alice, bob, carol := getEvents(
			channels, uint64(i), now, htlc, hops, preimage,
		)

		checkHtlcEvents(t, aliceEvents.Updates(), alice)
		checkHtlcEvents(t, bobEvents.Updates(), bob)
		checkHtlcEvents(t, carolEvents.Updates(), carol)
	}
}

// checkHtlcEvents checks that a subscription has the set of htlc events
// we expect it to have.
func checkHtlcEvents(t *testing.T, events <-chan interface{},
	expectedEvents []interface{}) {

	t.Helper()

	for _, expected := range expectedEvents {
		select {
		case event := <-events:
			if !reflect.DeepEqual(event, expected) {
				t.Fatalf("expected %v, got: %v", expected,
					event)
			}

		case <-time.After(5 * time.Second):
			t.Fatalf("expected event: %v", expected)
		}
	}

	// Check that there are no unexpected events following.
	select {
	case event := <-events:
		t.Fatalf("unexpected event: %v", event)
	default:
	}
}

// sendThreeHopPayment is a helper function which sends a payment over
// Alice -> Bob -> Carol in a three hop network and returns Alice's first htlc
// and the remainder of the hops.
func (n *threeHopNetwork) sendThreeHopPayment(t *testing.T) (*lnwire.UpdateAddHTLC,
	[]*hop.Payload, *lntypes.Preimage) {

	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)

	htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
		n.firstBobChannelLink, n.carolChannelLink)
	blob, err := generateRoute(hops...)
	if err != nil {
		t.Fatal(err)
	}
	invoice, htlc, pid, err := generatePayment(
		amount, htlcAmt, totalTimelock, blob,
	)
	if err != nil {
		t.Fatal(err)
	}

	err = n.carolServer.registry.AddInvoice(
		t.Context(), *invoice, htlc.PaymentHash,
	)
	require.NoError(t, err, "unable to add invoice in carol registry")

	if err := n.aliceServer.htlcSwitch.SendHTLC(
		n.firstBobChannelLink.ShortChanID(), pid, htlc,
	); err != nil {
		t.Fatalf("could not send htlc")
	}

	return htlc, hops, invoice.Terms.PaymentPreimage
}

// getThreeHopEvents gets the set of htlc events that we expect for a payment
// from Alice -> Bob -> Carol. If a non-nil link error is provided, the set
// of events will fail on Bob's outgoing link.
func getThreeHopEvents(channels *clusterChannels, htlcID uint64,
	ts time.Time, htlc *lnwire.UpdateAddHTLC, hops []*hop.Payload,
	linkError *LinkError,
	preimage *lntypes.Preimage) ([]interface{}, []interface{}, []interface{}) {

	aliceKey := HtlcKey{
		IncomingCircuit: zeroCircuit,
		OutgoingCircuit: models.CircuitKey{
			ChanID: channels.aliceToBob.ShortChanID(),
			HtlcID: htlcID,
		},
	}

	// Alice always needs a forwarding event because she initiates the
	// send.
	aliceEvents := []interface{}{
		&ForwardingEvent{
			HtlcKey: aliceKey,
			HtlcInfo: HtlcInfo{
				OutgoingTimeLock: htlc.Expiry,
				OutgoingAmt:      htlc.Amount,
			},
			HtlcEventType: HtlcEventTypeSend,
			Timestamp:     ts,
		},
	}

	bobKey := HtlcKey{
		IncomingCircuit: models.CircuitKey{
			ChanID: channels.bobToAlice.ShortChanID(),
			HtlcID: htlcID,
		},
		OutgoingCircuit: models.CircuitKey{
			ChanID: channels.bobToCarol.ShortChanID(),
			HtlcID: htlcID,
		},
	}

	bobInfo := HtlcInfo{
		IncomingTimeLock: htlc.Expiry,
		IncomingAmt:      htlc.Amount,
		OutgoingTimeLock: hops[1].FwdInfo.OutgoingCTLV,
		OutgoingAmt:      hops[1].FwdInfo.AmountToForward,
	}

	// If we expect the payment to fail, we add failures for alice and
	// bob, and no events for carol because the payment never reaches her.
	if linkError != nil {
		aliceEvents = append(aliceEvents,
			&ForwardingFailEvent{
				HtlcKey:       aliceKey,
				HtlcEventType: HtlcEventTypeSend,
				Timestamp:     ts,
			},
		)

		bobEvents := []interface{}{
			&LinkFailEvent{
				HtlcKey:       bobKey,
				HtlcInfo:      bobInfo,
				HtlcEventType: HtlcEventTypeForward,
				LinkError:     linkError,
				Incoming:      false,
				Timestamp:     ts,
			},
			&FinalHtlcEvent{
				CircuitKey: bobKey.IncomingCircuit,
				Settled:    false,
				Offchain:   true,
				Timestamp:  ts,
			},
		}

		return aliceEvents, bobEvents, nil
	}

	// If we want to get events for a successful payment, we add a settle
	// for alice, a forward and settle for bob and a receive settle for
	// carol.
	aliceEvents = append(
		aliceEvents,
		&SettleEvent{
			HtlcKey:       aliceKey,
			Preimage:      *preimage,
			HtlcEventType: HtlcEventTypeSend,
			Timestamp:     ts,
		},
	)

	bobEvents := []interface{}{
		&ForwardingEvent{
			HtlcKey:       bobKey,
			HtlcInfo:      bobInfo,
			HtlcEventType: HtlcEventTypeForward,
			Timestamp:     ts,
		},
		&SettleEvent{
			HtlcKey:       bobKey,
			Preimage:      *preimage,
			HtlcEventType: HtlcEventTypeForward,
			Timestamp:     ts,
		},
		&FinalHtlcEvent{
			CircuitKey: bobKey.IncomingCircuit,
			Settled:    true,
			Offchain:   true,
			Timestamp:  ts,
		},
	}

	carolEvents := []interface{}{
		&SettleEvent{
			HtlcKey: HtlcKey{
				IncomingCircuit: models.CircuitKey{
					ChanID: channels.carolToBob.ShortChanID(),
					HtlcID: htlcID,
				},
				OutgoingCircuit: zeroCircuit,
			},
			Preimage:      *preimage,
			HtlcEventType: HtlcEventTypeReceive,
			Timestamp:     ts,
		}, &FinalHtlcEvent{
			CircuitKey: models.CircuitKey{
				ChanID: channels.carolToBob.ShortChanID(),
				HtlcID: htlcID,
			},
			Settled:   true,
			Offchain:  true,
			Timestamp: ts,
		},
	}

	return aliceEvents, bobEvents, carolEvents
}

type mockForwardInterceptor struct {
	t *testing.T

	interceptedChan chan InterceptedPacket
}

func (m *mockForwardInterceptor) InterceptForwardHtlc(
	intercepted InterceptedPacket) error {

	m.interceptedChan <- intercepted

	return nil
}

func (m *mockForwardInterceptor) getIntercepted() InterceptedPacket {
	m.t.Helper()

	select {
	case p := <-m.interceptedChan:
		return p

	case <-time.After(time.Second):
		require.Fail(m.t, "timeout")

		return InterceptedPacket{}
	}
}

func assertNumCircuits(t *testing.T, s *Switch, pending, opened int) {
	if s.circuits.NumPending() != pending {
		t.Fatalf("wrong amount of half circuits, expected %v but "+
			"got %v", pending, s.circuits.NumPending())
	}
	if s.circuits.NumOpen() != opened {
		t.Fatalf("wrong amount of circuits, expected %v but got %v",
			opened, s.circuits.NumOpen())
	}
}

func assertOutgoingLinkReceive(t *testing.T, targetLink *mockChannelLink,
	expectReceive bool) *htlcPacket {

	// Pull packet from targetLink link.
	select {
	case packet := <-targetLink.packets:
		if !expectReceive {
			t.Fatal("forward was intercepted, shouldn't land at bob link")
		} else if err := targetLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

		return packet

	case <-time.After(time.Second):
		if expectReceive {
			t.Fatal("request was not propagated to destination")
		}
	}

	return nil
}

func assertOutgoingLinkReceiveIntercepted(t *testing.T,
	targetLink *mockChannelLink) {

	t.Helper()

	select {
	case <-targetLink.packets:
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}
}

type interceptableSwitchTestContext struct {
	t *testing.T

	preimage           [sha256.Size]byte
	rhash              [32]byte
	onionBlob          [1366]byte
	incomingHtlcID     uint64
	cltvRejectDelta    uint32
	cltvInterceptDelta uint32

	forwardInterceptor *mockForwardInterceptor
	aliceChannelLink   *mockChannelLink
	bobChannelLink     *mockChannelLink
	s                  *Switch
}

func newInterceptableSwitchTestContext(
	t *testing.T) *interceptableSwitchTestContext {

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	tempPath := t.TempDir()

	cdb := channeldb.OpenForTesting(t, tempPath)

	s, err := initSwitchWithDB(testStartingHeight, cdb)
	require.NoError(t, err, "unable to init switch")
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	preimage := [sha256.Size]byte{1}

	ctx := &interceptableSwitchTestContext{
		t:                  t,
		preimage:           preimage,
		rhash:              sha256.Sum256(preimage[:]),
		onionBlob:          [1366]byte{4, 5, 6},
		incomingHtlcID:     uint64(0),
		cltvRejectDelta:    10,
		cltvInterceptDelta: 13,
		forwardInterceptor: &mockForwardInterceptor{
			t:               t,
			interceptedChan: make(chan InterceptedPacket),
		},
		aliceChannelLink: aliceChannelLink,
		bobChannelLink:   bobChannelLink,
		s:                s,
	}

	return ctx
}

func (c *interceptableSwitchTestContext) createTestPacket() *htlcPacket {
	c.incomingHtlcID++

	return &htlcPacket{
		incomingChanID:  c.aliceChannelLink.ShortChanID(),
		incomingHTLCID:  c.incomingHtlcID,
		incomingTimeout: testStartingHeight + c.cltvInterceptDelta + 1,
		outgoingChanID:  c.bobChannelLink.ShortChanID(),
		obfuscator:      NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: c.rhash,
			Amount:      1,
			OnionBlob:   c.onionBlob,
		},
	}
}

func (c *interceptableSwitchTestContext) finish() {
	if err := c.s.Stop(); err != nil {
		c.t.Fatal(err)
	}
}

func (c *interceptableSwitchTestContext) createSettlePacket(
	outgoingHTLCID uint64) *htlcPacket {

	return &htlcPacket{
		outgoingChanID: c.bobChannelLink.ShortChanID(),
		outgoingHTLCID: outgoingHTLCID,
		amount:         1,
		htlc: &lnwire.UpdateFulfillHTLC{
			PaymentPreimage: c.preimage,
		},
	}
}

func TestSwitchHoldForward(t *testing.T) {
	t.Parallel()

	c := newInterceptableSwitchTestContext(t)
	defer c.finish()

	notifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
	}
	notifier.EpochChan <- &chainntnfs.BlockEpoch{Height: testStartingHeight}

	switchForwardInterceptor, err := NewInterceptableSwitch(
		&InterceptableSwitchConfig{
			Switch:             c.s,
			CltvRejectDelta:    c.cltvRejectDelta,
			CltvInterceptDelta: c.cltvInterceptDelta,
			Notifier:           notifier,
		},
	)
	require.NoError(t, err)
	require.NoError(t, switchForwardInterceptor.Start())

	switchForwardInterceptor.SetInterceptor(c.forwardInterceptor.InterceptForwardHtlc)
	linkQuit := make(chan struct{})

	// Test a forward that expires too soon.
	packet := c.createTestPacket()
	packet.incomingTimeout = testStartingHeight + c.cltvRejectDelta - 1

	err = switchForwardInterceptor.ForwardPackets(linkQuit, false, packet)
	require.NoError(t, err, "can't forward htlc packet")
	assertOutgoingLinkReceive(t, c.bobChannelLink, false)
	assertOutgoingLinkReceiveIntercepted(t, c.aliceChannelLink)
	assertNumCircuits(t, c.s, 0, 0)

	// Test a forward that expires too soon and can't be failed.
	packet = c.createTestPacket()
	packet.incomingTimeout = testStartingHeight + c.cltvRejectDelta - 1

	// Simulate an error during the composition of the failure message.
	currentCallback := c.s.cfg.FetchLastChannelUpdate
	c.s.cfg.FetchLastChannelUpdate = func(
		lnwire.ShortChannelID) (*lnwire.ChannelUpdate1, error) {

		return nil, errors.New("cannot fetch update")
	}

	err = switchForwardInterceptor.ForwardPackets(linkQuit, false, packet)
	require.NoError(t, err, "can't forward htlc packet")
	receivedPkt := assertOutgoingLinkReceive(t, c.bobChannelLink, true)
	assertNumCircuits(t, c.s, 1, 1)

	require.NoError(t, switchForwardInterceptor.ForwardPackets(
		linkQuit, false,
		c.createSettlePacket(receivedPkt.outgoingHTLCID),
	))

	assertOutgoingLinkReceive(t, c.aliceChannelLink, true)
	assertNumCircuits(t, c.s, 0, 0)

	c.s.cfg.FetchLastChannelUpdate = currentCallback

	// Test resume a hold forward.
	assertNumCircuits(t, c.s, 0, 0)
	err = switchForwardInterceptor.ForwardPackets(
		linkQuit, false, c.createTestPacket(),
	)
	require.NoError(t, err)

	assertNumCircuits(t, c.s, 0, 0)
	assertOutgoingLinkReceive(t, c.bobChannelLink, false)

	require.NoError(t, switchForwardInterceptor.Resolve(&FwdResolution{
		Action: FwdActionResume,
		Key:    c.forwardInterceptor.getIntercepted().IncomingCircuit,
	}))
	receivedPkt = assertOutgoingLinkReceive(t, c.bobChannelLink, true)
	assertNumCircuits(t, c.s, 1, 1)

	// settling the htlc to close the circuit.
	err = switchForwardInterceptor.ForwardPackets(
		linkQuit, false,
		c.createSettlePacket(receivedPkt.outgoingHTLCID),
	)
	require.NoError(t, err)

	assertOutgoingLinkReceive(t, c.aliceChannelLink, true)
	assertNumCircuits(t, c.s, 0, 0)

	// Test resume a hold forward after disconnection.
	require.NoError(t, switchForwardInterceptor.ForwardPackets(
		linkQuit, false, c.createTestPacket(),
	))

	// Wait until the packet is offered to the interceptor.
	_ = c.forwardInterceptor.getIntercepted()

	// No forward expected yet.
	assertNumCircuits(t, c.s, 0, 0)
	assertOutgoingLinkReceive(t, c.bobChannelLink, false)

	// Disconnect should resume the forwarding.
	switchForwardInterceptor.SetInterceptor(nil)

	receivedPkt = assertOutgoingLinkReceive(t, c.bobChannelLink, true)
	assertNumCircuits(t, c.s, 1, 1)

	// Settle the htlc to close the circuit.
	require.NoError(t, switchForwardInterceptor.ForwardPackets(
		linkQuit, false,
		c.createSettlePacket(receivedPkt.outgoingHTLCID),
	))

	assertOutgoingLinkReceive(t, c.aliceChannelLink, true)
	assertNumCircuits(t, c.s, 0, 0)

	// Test failing a hold forward
	switchForwardInterceptor.SetInterceptor(
		c.forwardInterceptor.InterceptForwardHtlc,
	)

	require.NoError(t, switchForwardInterceptor.ForwardPackets(
		linkQuit, false, c.createTestPacket(),
	))
	assertNumCircuits(t, c.s, 0, 0)
	assertOutgoingLinkReceive(t, c.bobChannelLink, false)

	require.NoError(t, switchForwardInterceptor.Resolve(&FwdResolution{
		Action:      FwdActionFail,
		Key:         c.forwardInterceptor.getIntercepted().IncomingCircuit,
		FailureCode: lnwire.CodeTemporaryChannelFailure,
	}))
	assertOutgoingLinkReceive(t, c.bobChannelLink, false)
	assertOutgoingLinkReceive(t, c.aliceChannelLink, true)
	assertNumCircuits(t, c.s, 0, 0)

	// Test failing a hold forward with a failure message.
	require.NoError(t,
		switchForwardInterceptor.ForwardPackets(
			linkQuit, false, c.createTestPacket(),
		),
	)
	assertNumCircuits(t, c.s, 0, 0)
	assertOutgoingLinkReceive(t, c.bobChannelLink, false)

	reason := lnwire.OpaqueReason([]byte{1, 2, 3})
	require.NoError(t, switchForwardInterceptor.Resolve(&FwdResolution{
		Action:         FwdActionFail,
		Key:            c.forwardInterceptor.getIntercepted().IncomingCircuit,
		FailureMessage: reason,
	}))

	assertOutgoingLinkReceive(t, c.bobChannelLink, false)
	packet = assertOutgoingLinkReceive(t, c.aliceChannelLink, true)

	require.Equal(t, reason, packet.htlc.(*lnwire.UpdateFailHTLC).Reason)

	assertNumCircuits(t, c.s, 0, 0)

	// Test failing a hold forward with a malformed htlc failure.
	err = switchForwardInterceptor.ForwardPackets(
		linkQuit, false, c.createTestPacket(),
	)
	require.NoError(t, err)

	assertNumCircuits(t, c.s, 0, 0)
	assertOutgoingLinkReceive(t, c.bobChannelLink, false)

	code := lnwire.CodeInvalidOnionKey
	require.NoError(t, switchForwardInterceptor.Resolve(&FwdResolution{
		Action:      FwdActionFail,
		Key:         c.forwardInterceptor.getIntercepted().IncomingCircuit,
		FailureCode: code,
	}))

	assertOutgoingLinkReceive(t, c.bobChannelLink, false)
	packet = assertOutgoingLinkReceive(t, c.aliceChannelLink, true)
	failPacket := packet.htlc.(*lnwire.UpdateFailHTLC)

	shaOnionBlob := sha256.Sum256(c.onionBlob[:])
	expectedFailure := &lnwire.FailInvalidOnionKey{
		OnionSHA256: shaOnionBlob,
	}

	fwdErr, err := newMockDeobfuscator().DecryptError(failPacket.Reason)
	require.NoError(t, err)
	require.Equal(t, expectedFailure, fwdErr.WireMessage())

	assertNumCircuits(t, c.s, 0, 0)

	// Test settling a hold forward
	require.NoError(t, switchForwardInterceptor.ForwardPackets(
		linkQuit, false, c.createTestPacket(),
	))
	assertNumCircuits(t, c.s, 0, 0)
	assertOutgoingLinkReceive(t, c.bobChannelLink, false)

	require.NoError(t, switchForwardInterceptor.Resolve(&FwdResolution{
		Key:      c.forwardInterceptor.getIntercepted().IncomingCircuit,
		Action:   FwdActionSettle,
		Preimage: c.preimage,
	}))
	assertOutgoingLinkReceive(t, c.bobChannelLink, false)
	assertOutgoingLinkReceive(t, c.aliceChannelLink, true)
	assertNumCircuits(t, c.s, 0, 0)

	require.NoError(t, switchForwardInterceptor.Stop())

	// Test always-on interception.
	notifier = &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
	}
	notifier.EpochChan <- &chainntnfs.BlockEpoch{Height: testStartingHeight}

	switchForwardInterceptor, err = NewInterceptableSwitch(
		&InterceptableSwitchConfig{
			Switch:             c.s,
			CltvRejectDelta:    c.cltvRejectDelta,
			CltvInterceptDelta: c.cltvInterceptDelta,
			RequireInterceptor: true,
			Notifier:           notifier,
		},
	)
	require.NoError(t, err)
	require.NoError(t, switchForwardInterceptor.Start())

	// Forward a fresh packet. It is expected to be failed immediately,
	// because there is no interceptor registered.
	require.NoError(t, switchForwardInterceptor.ForwardPackets(
		linkQuit, false, c.createTestPacket(),
	))

	assertOutgoingLinkReceive(t, c.bobChannelLink, false)
	assertOutgoingLinkReceive(t, c.aliceChannelLink, true)
	assertNumCircuits(t, c.s, 0, 0)

	// Forward a replayed packet. It is expected to be held until the
	// interceptor connects. To continue the test, it needs to be ran in a
	// goroutine.
	errChan := make(chan error)
	go func() {
		errChan <- switchForwardInterceptor.ForwardPackets(
			linkQuit, true, c.createTestPacket(),
		)
	}()

	// Assert that nothing is forward to the switch.
	assertOutgoingLinkReceive(t, c.bobChannelLink, false)
	assertNumCircuits(t, c.s, 0, 0)

	// Register an interceptor.
	switchForwardInterceptor.SetInterceptor(
		c.forwardInterceptor.InterceptForwardHtlc,
	)

	// Expect the ForwardPackets call to unblock.
	require.NoError(t, <-errChan)

	// Now expect the queued packet to come through.
	c.forwardInterceptor.getIntercepted()

	// Disconnect and reconnect interceptor.
	switchForwardInterceptor.SetInterceptor(nil)
	switchForwardInterceptor.SetInterceptor(
		c.forwardInterceptor.InterceptForwardHtlc,
	)

	// A replay of the held packet is expected.
	intercepted := c.forwardInterceptor.getIntercepted()

	// Settle the packet.
	require.NoError(t, switchForwardInterceptor.Resolve(&FwdResolution{
		Key:      intercepted.IncomingCircuit,
		Action:   FwdActionSettle,
		Preimage: c.preimage,
	}))
	assertOutgoingLinkReceive(t, c.bobChannelLink, false)
	assertOutgoingLinkReceive(t, c.aliceChannelLink, true)
	assertNumCircuits(t, c.s, 0, 0)

	require.NoError(t, switchForwardInterceptor.Stop())

	select {
	case <-c.forwardInterceptor.interceptedChan:
		require.Fail(t, "unexpected interception")

	default:
	}
}

func TestInterceptableSwitchWatchDog(t *testing.T) {
	t.Parallel()

	c := newInterceptableSwitchTestContext(t)
	defer c.finish()

	// Start interceptable switch.
	notifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
	}
	notifier.EpochChan <- &chainntnfs.BlockEpoch{Height: testStartingHeight}

	switchForwardInterceptor, err := NewInterceptableSwitch(
		&InterceptableSwitchConfig{
			Switch:             c.s,
			CltvRejectDelta:    c.cltvRejectDelta,
			CltvInterceptDelta: c.cltvInterceptDelta,
			Notifier:           notifier,
		},
	)
	require.NoError(t, err)
	require.NoError(t, switchForwardInterceptor.Start())

	// Set interceptor.
	switchForwardInterceptor.SetInterceptor(
		c.forwardInterceptor.InterceptForwardHtlc,
	)

	// Receive a packet.
	linkQuit := make(chan struct{})

	packet := c.createTestPacket()

	err = switchForwardInterceptor.ForwardPackets(linkQuit, false, packet)
	require.NoError(t, err, "can't forward htlc packet")

	// Intercept the packet.
	intercepted := c.forwardInterceptor.getIntercepted()

	require.Equal(t,
		int32(packet.incomingTimeout-c.cltvRejectDelta),
		intercepted.AutoFailHeight,
	)

	// Htlc expires before a resolution from the interceptor.
	notifier.EpochChan <- &chainntnfs.BlockEpoch{
		Height: int32(packet.incomingTimeout) -
			int32(c.cltvRejectDelta),
	}

	// Expect the htlc to be failed back.
	assertOutgoingLinkReceive(t, c.aliceChannelLink, true)

	// It is too late now to resolve. Expect an error.
	require.Error(t, switchForwardInterceptor.Resolve(&FwdResolution{
		Action:   FwdActionSettle,
		Key:      intercepted.IncomingCircuit,
		Preimage: c.preimage,
	}))
}

// TestSwitchDustForwarding tests that the switch properly fails HTLC's which
// have incoming or outgoing links that breach their fee thresholds.
func TestSwitchDustForwarding(t *testing.T) {
	t.Parallel()

	// We'll create a three-hop network:
	// - Alice has a dust limit of 200sats with Bob
	// - Bob has a dust limit of 800sats with Alice
	// - Bob has a dust limit of 200sats with Carol
	// - Carol has a dust limit of 800sats with Bob
	channels, _, err := createClusterChannels(
		t, btcutil.SatoshiPerBitcoin, btcutil.SatoshiPerBitcoin,
	)
	require.NoError(t, err)

	n := newThreeHopNetwork(
		t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight,
	)
	err = n.start()
	require.NoError(t, err)

	// We'll also put Alice and Bob into hodl.ExitSettle mode, such that
	// they won't settle incoming exit-hop HTLC's automatically.
	n.aliceChannelLink.cfg.HodlMask = hodl.ExitSettle.Mask()
	n.firstBobChannelLink.cfg.HodlMask = hodl.ExitSettle.Mask()

	// We'll test that once the default threshold is exceeded on the
	// Alice -> Bob channel, either side's calls to SendHTLC will fail.
	numHTLCs := maxInflightHtlcs
	aliceAttemptID, bobAttemptID := numHTLCs, numHTLCs
	amt := lnwire.NewMSatFromSatoshis(700)
	aliceBobFirstHop := n.aliceChannelLink.ShortChanID()

	// We decreased the max number of inflight HTLCs therefore we also need
	// do decrease the max fee exposure.
	maxFeeExposure := lnwire.NewMSatFromSatoshis(74500)
	n.aliceChannelLink.cfg.MaxFeeExposure = maxFeeExposure
	n.firstBobChannelLink.cfg.MaxFeeExposure = maxFeeExposure

	// Alice will send 50 HTLC's of 700sats. Bob will also send 50 HTLC's
	// of 700sats.
	sendDustHtlcs(t, n, true, amt, aliceBobFirstHop, numHTLCs)
	sendDustHtlcs(t, n, false, amt, aliceBobFirstHop, numHTLCs)

	// Generate the parameters needed for Bob to send another dust HTLC.
	_, timelock, hops := generateHops(
		amt, testStartingHeight, n.aliceChannelLink,
	)

	blob, err := generateRoute(hops...)
	require.NoError(t, err)

	// Assert that if Bob sends a dust HTLC it will fail.
	failingPreimage := lntypes.Preimage{0, 0, 3}
	failingHash := failingPreimage.Hash()
	failingHtlc := &lnwire.UpdateAddHTLC{
		PaymentHash: failingHash,
		Amount:      amt,
		Expiry:      timelock,
		OnionBlob:   blob,
	}

	// This is the expected dust without taking the commitfee into account.
	expectedDust := maxInflightHtlcs * 2 * amt

	assertAlmostDust := func(link *channelLink, mbox MailBox,
		whoseCommit lntypes.ChannelParty) {

		err := wait.NoError(func() error {
			linkDust := link.getDustSum(
				whoseCommit, fn.None[chainfee.SatPerKWeight](),
			)
			localMailDust, remoteMailDust := mbox.DustPackets()

			totalDust := linkDust
			if whoseCommit.IsRemote() {
				totalDust += remoteMailDust
			} else {
				totalDust += localMailDust
			}

			if totalDust == expectedDust {
				return nil
			}

			return fmt.Errorf("got totalDust=%v, expectedDust=%v",
				totalDust, expectedDust)
		}, 15*time.Second)
		require.NoError(t, err, "timeout checking dust")
	}

	// Wait until Bob is almost at the fee threshold.
	bobMbox := n.bobServer.htlcSwitch.mailOrchestrator.GetOrCreateMailBox(
		n.firstBobChannelLink.ChanID(),
		n.firstBobChannelLink.ShortChanID(),
	)
	assertAlmostDust(n.firstBobChannelLink, bobMbox, lntypes.Local)

	// Sending one more HTLC should fail. SendHTLC won't error, but the
	// HTLC should be failed backwards. When sending we only check for the
	// dust amount without the commitment fee. When the HTLC is added to the
	// commitment state (link) we also take into account the commitment fee
	// and with a fee of 6000 sat/kw and a commitment size of 724 (non
	// anchor channel) we are overexposed in fees (maxFeeExposure) that's
	// why the HTLC is failed back.
	err = n.bobServer.htlcSwitch.SendHTLC(
		aliceBobFirstHop, uint64(bobAttemptID), failingHtlc,
	)
	require.Nil(t, err)

	// Use the network result store to ensure the HTLC was failed
	// backwards.
	bobResultChan, err := n.bobServer.htlcSwitch.GetAttemptResult(
		uint64(bobAttemptID), failingHash, newMockDeobfuscator(),
	)
	require.NoError(t, err)

	result, ok := <-bobResultChan
	require.True(t, ok)
	assertFailureCode(
		t, result.Error, lnwire.CodeTemporaryChannelFailure,
	)

	bobAttemptID++

	// Generate the parameters needed for bob to send a non-dust HTLC.
	nondustAmt := lnwire.NewMSatFromSatoshis(10_000)
	_, _, hops = generateHops(
		nondustAmt, testStartingHeight, n.aliceChannelLink,
	)

	blob, err = generateRoute(hops...)
	require.NoError(t, err)

	// Now attempt to send an HTLC above Bob's dust limit. Even though this
	// is not a dust HTLC, it should fail because the increase in weight
	// pushes us over the threshold.
	nondustPreimage := lntypes.Preimage{0, 0, 4}
	nondustHash := nondustPreimage.Hash()
	nondustHtlc := &lnwire.UpdateAddHTLC{
		PaymentHash: nondustHash,
		Amount:      nondustAmt,
		Expiry:      timelock,
		OnionBlob:   blob,
	}

	err = n.bobServer.htlcSwitch.SendHTLC(
		aliceBobFirstHop, uint64(bobAttemptID), nondustHtlc,
	)
	require.NoError(t, err)
	assertAlmostDust(n.firstBobChannelLink, bobMbox, lntypes.Local)

	// Check that the HTLC failed.
	bobResultChan, err = n.bobServer.htlcSwitch.GetAttemptResult(
		uint64(bobAttemptID), nondustHash, newMockDeobfuscator(),
	)
	require.NoError(t, err)

	result, ok = <-bobResultChan
	require.True(t, ok)
	assertFailureCode(
		t, result.Error, lnwire.CodeTemporaryChannelFailure,
	)

	// Introduce Carol into the mix and assert that sending a multi-hop
	// dust HTLC to Alice will fail. Bob should fail back the HTLC with a
	// temporary channel failure.
	carolAmt, carolTimelock, carolHops := generateHops(
		amt, testStartingHeight, n.secondBobChannelLink,
		n.aliceChannelLink,
	)

	carolBlob, err := generateRoute(carolHops...)
	require.NoError(t, err)

	carolPreimage := lntypes.Preimage{0, 0, 5}
	carolHash := carolPreimage.Hash()
	carolHtlc := &lnwire.UpdateAddHTLC{
		PaymentHash: carolHash,
		Amount:      carolAmt,
		Expiry:      carolTimelock,
		OnionBlob:   carolBlob,
	}

	// Initialize Carol's attempt ID.
	carolAttemptID := 0

	err = n.carolServer.htlcSwitch.SendHTLC(
		n.carolChannelLink.ShortChanID(), uint64(carolAttemptID),
		carolHtlc,
	)
	require.NoError(t, err)

	carolResultChan, err := n.carolServer.htlcSwitch.GetAttemptResult(
		uint64(carolAttemptID), carolHash, newMockDeobfuscator(),
	)
	require.NoError(t, err)

	result, ok = <-carolResultChan
	require.True(t, ok)
	assertFailureCode(
		t, result.Error, lnwire.CodeTemporaryChannelFailure,
	)

	// Send an HTLC from Alice to Carol and assert that it gets failed.
	htlcAmt, totalTimelock, aliceHops := generateHops(
		amt, testStartingHeight, n.firstBobChannelLink,
		n.carolChannelLink,
	)

	blob, err = generateRoute(aliceHops...)
	require.NoError(t, err)

	aliceMultihopPreimage := lntypes.Preimage{0, 0, 6}
	aliceMultihopHash := aliceMultihopPreimage.Hash()
	aliceMultihopHtlc := &lnwire.UpdateAddHTLC{
		PaymentHash: aliceMultihopHash,
		Amount:      htlcAmt,
		Expiry:      totalTimelock,
		OnionBlob:   blob,
	}

	// Wait until Alice's expected dust for the remote commitment is just
	// under the fee threshold.
	aliceOrch := n.aliceServer.htlcSwitch.mailOrchestrator
	aliceMbox := aliceOrch.GetOrCreateMailBox(
		n.aliceChannelLink.ChanID(), n.aliceChannelLink.ShortChanID(),
	)
	assertAlmostDust(n.aliceChannelLink, aliceMbox, lntypes.Remote)

	err = n.aliceServer.htlcSwitch.SendHTLC(
		n.aliceChannelLink.ShortChanID(), uint64(aliceAttemptID),
		aliceMultihopHtlc,
	)
	require.Nil(t, err)

	aliceResultChan, err := n.aliceServer.htlcSwitch.GetAttemptResult(
		uint64(aliceAttemptID), aliceMultihopHash,
		newMockDeobfuscator(),
	)
	require.NoError(t, err)

	result, ok = <-aliceResultChan
	require.True(t, ok)
	assertFailureCode(
		t, result.Error, lnwire.CodeTemporaryChannelFailure,
	)

	// Check that there are numHTLCs circuits open for both Alice and Bob.
	require.Equal(t, numHTLCs, n.aliceServer.htlcSwitch.circuits.NumOpen())
	require.Equal(t, numHTLCs, n.bobServer.htlcSwitch.circuits.NumOpen())
}

// sendDustHtlcs is a helper function used to send many dust HTLC's to test the
// Switch's channel-max-fee-exposure logic. It takes a boolean denoting whether
// or not Alice is the sender.
func sendDustHtlcs(t *testing.T, n *threeHopNetwork, alice bool,
	amt lnwire.MilliSatoshi, sid lnwire.ShortChannelID, numHTLCs int) {

	t.Helper()

	// Extract the destination into a variable. If alice is the sender, the
	// destination is Bob.
	destLink := n.aliceChannelLink
	if alice {
		destLink = n.firstBobChannelLink
	}

	// Create hops that will be used in the onion payload.
	htlcAmt, totalTimelock, hops := generateHops(
		amt, testStartingHeight, destLink,
	)

	// Convert the hops to a blob that will be put in the Add message.
	blob, err := generateRoute(hops...)
	require.NoError(t, err)

	// Create a slice to store the preimages.
	preimages := make([]lntypes.Preimage, numHTLCs)

	// Initialize the attempt ID used in SendHTLC calls.
	attemptID := uint64(0)

	// Deterministically generate preimages. Avoid the all-zeroes preimage
	// because that will be rejected by the database. We'll use a different
	// third byte for Alice and Bob.
	endByte := byte(2)
	if alice {
		endByte = byte(3)
	}

	for i := 0; i < numHTLCs; i++ {
		preimages[i] = lntypes.Preimage{byte(i >> 8), byte(i), endByte}
	}

	sendingSwitch := n.bobServer.htlcSwitch
	if alice {
		sendingSwitch = n.aliceServer.htlcSwitch
	}

	// Call SendHTLC in a loop for numHTLCs.
	for i := 0; i < numHTLCs; i++ {
		// Construct the htlc packet.
		hash := preimages[i].Hash()

		htlc := &lnwire.UpdateAddHTLC{
			PaymentHash: hash,
			Amount:      htlcAmt,
			Expiry:      totalTimelock,
			OnionBlob:   blob,
		}

		for {
			// It may be the case that the fee threshold is hit
			// before all numHTLCs*2 HTLC's are sent due to double
			// counting. Get around this by continuing to send
			// until successful.
			err = sendingSwitch.SendHTLC(sid, attemptID, htlc)
			if err == nil {
				break
			}
		}

		attemptID++
	}
}

// TestSwitchMailboxDust tests that the switch takes into account the mailbox
// dust when evaluating the fee threshold. The mockChannelLink does not have
// channel state, so this only tests the switch-mailbox interaction.
func TestSwitchMailboxDust(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	carolPeer, err := newMockServer(
		t, "carol", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err)
	err = s.Start()
	require.NoError(t, err)
	defer func() {
		_ = s.Stop()
	}()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	chanID3, carolChanID := genID()

	aliceLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	err = s.AddLink(aliceLink)
	require.NoError(t, err)

	bobLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	err = s.AddLink(bobLink)
	require.NoError(t, err)

	carolLink := newMockChannelLink(
		s, chanID3, carolChanID, emptyScid, carolPeer, true, false,
		false, false,
	)
	err = s.AddLink(carolLink)
	require.NoError(t, err)

	// mockChannelLink sets the local and remote dust limits of the mailbox
	// to 400 satoshis and the feerate to 0. We'll fill the mailbox up with
	// dust packets and assert that calls to SendHTLC will fail.
	preimage, err := genPreimage()
	require.NoError(t, err)
	rhash := sha256.Sum256(preimage[:])
	amt := lnwire.NewMSatFromSatoshis(350)
	addMsg := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      amt,
		ChanID:      chanID1,
	}

	// Initialize the carolHTLCID.
	var carolHTLCID uint64

	// It will take aliceCount HTLC's of 350sats to fill up Alice's mailbox
	// to the point where another would put Alice over the fee threshold.
	aliceCount := 1428

	mailbox := s.mailOrchestrator.GetOrCreateMailBox(chanID1, aliceChanID)

	for i := 0; i < aliceCount; i++ {
		alicePkt := &htlcPacket{
			incomingChanID: carolChanID,
			incomingHTLCID: carolHTLCID,
			outgoingChanID: aliceChanID,
			obfuscator:     NewMockObfuscator(),
			incomingAmount: amt,
			amount:         amt,
			htlc:           addMsg,
		}

		err = mailbox.AddPacket(alicePkt)
		require.NoError(t, err)

		carolHTLCID++
	}

	// Sending one more HTLC to Alice should result in the fee threshold
	// being breached.
	err = s.SendHTLC(aliceChanID, 0, addMsg)
	require.ErrorIs(t, err, errFeeExposureExceeded)

	// We'll now call ForwardPackets from Bob to ensure that the mailbox
	// sum is also accounted for in the forwarding case.
	packet := &htlcPacket{
		incomingChanID: bobChanID,
		incomingHTLCID: 0,
		outgoingChanID: aliceChanID,
		obfuscator:     NewMockObfuscator(),
		incomingAmount: amt,
		amount:         amt,
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      amt,
			ChanID:      chanID1,
		},
	}

	err = s.ForwardPackets(nil, packet)
	require.NoError(t, err)

	// Bob should receive a failure from the switch.
	select {
	case p := <-bobLink.packets:
		require.NotEmpty(t, p.linkFailure)
		assertFailureCode(
			t, p.linkFailure, lnwire.CodeTemporaryChannelFailure,
		)

	case <-time.After(5 * time.Second):
		t.Fatal("no timely reply from switch")
	}
}

// TestSwitchResolution checks the ability of the switch to persist and handle
// resolution messages.
func TestSwitchResolution(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err)

	// Even though we intend to Stop s later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	t.Cleanup(func() { var _ = s.Stop() })

	err = s.Start()
	require.NoError(t, err)

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	err = s.AddLink(aliceChannelLink)
	require.NoError(t, err)
	err = s.AddLink(bobChannelLink)
	require.NoError(t, err)

	// Create an add htlcPacket that Alice will send to Bob.
	preimage, err := genPreimage()
	require.NoError(t, err)

	rhash := sha256.Sum256(preimage[:])
	packet := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	err = s.ForwardPackets(nil, packet)
	require.NoError(t, err)

	// Bob will receive the packet and open the circuit.
	select {
	case <-bobChannelLink.packets:
		err = bobChannelLink.completeCircuit(packet)
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	// Check that only one circuit is open.
	require.Equal(t, 1, s.circuits.NumOpen())

	// We'll send a settle resolution to Switch that should go to Alice.
	settleResMsg := contractcourt.ResolutionMsg{
		SourceChan: bobChanID,
		HtlcIndex:  0,
		PreImage:   &preimage,
	}

	// Before the resolution is sent, remove alice's link so we can assert
	// that the resolution is actually stored. Otherwise, it would be
	// deleted shortly after being sent.
	s.RemoveLink(chanID1)

	// Send the resolution message.
	err = s.ProcessContractResolution(settleResMsg)
	require.NoError(t, err)

	// Assert that the resolution store contains the settle reoslution.
	resMsgs, err := s.resMsgStore.fetchAllResolutionMsg()
	require.NoError(t, err)

	require.Equal(t, 1, len(resMsgs))
	require.Equal(t, settleResMsg.SourceChan, resMsgs[0].SourceChan)
	require.Equal(t, settleResMsg.HtlcIndex, resMsgs[0].HtlcIndex)
	require.Nil(t, resMsgs[0].Failure)
	require.Equal(t, preimage, *resMsgs[0].PreImage)

	// Now we'll restart Alice's link and delete the circuit.
	err = s.AddLink(aliceChannelLink)
	require.NoError(t, err)

	// Alice will receive the packet and open the circuit.
	select {
	case alicePkt := <-aliceChannelLink.packets:
		err = aliceChannelLink.completeCircuit(alicePkt)
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	// Assert that there are no more circuits.
	require.Equal(t, 0, s.circuits.NumOpen())

	// We'll restart the Switch and assert that Alice does not receive
	// another packet.
	switchDB := s.cfg.DB.(*channeldb.DB)
	err = s.Stop()
	require.NoError(t, err)

	s, err = initSwitchWithDB(testStartingHeight, switchDB)
	require.NoError(t, err)

	err = s.Start()
	require.NoError(t, err)
	defer func() {
		_ = s.Stop()
	}()

	err = s.AddLink(aliceChannelLink)
	require.NoError(t, err)
	err = s.AddLink(bobChannelLink)
	require.NoError(t, err)

	// Alice should not receive a packet since the Switch should have
	// deleted the resolution message since the circuit was closed.
	select {
	case alicePkt := <-aliceChannelLink.packets:
		t.Fatalf("received erroneous packet: %v", alicePkt)
	case <-time.After(time.Second * 5):
	}

	// Check that the resolution message no longer exists in the store.
	resMsgs, err = s.resMsgStore.fetchAllResolutionMsg()
	require.NoError(t, err)
	require.Equal(t, 0, len(resMsgs))
}

// TestSwitchForwardFailAlias tests that if ForwardPackets returns a failure
// before actually forwarding, the ChannelUpdate uses the SCID from the
// incoming channel and does not leak private information like the UTXO.
func TestSwitchForwardFailAlias(t *testing.T) {
	tests := []struct {
		name string

		// Whether or not Alice will be a zero-conf channel or an
		// option-scid-alias channel (feature-bit).
		zeroConf bool
	}{
		{
			name:     "option-scid-alias forwarding failure",
			zeroConf: false,
		},
		{
			name:     "zero-conf forwarding failure",
			zeroConf: true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			testSwitchForwardFailAlias(t, test.zeroConf)
		})
	}
}

func testSwitchForwardFailAlias(t *testing.T, zeroConf bool) {
	t.Parallel()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	tempPath := t.TempDir()

	cdb := channeldb.OpenForTesting(t, tempPath)

	s, err := initSwitchWithDB(testStartingHeight, cdb)
	require.NoError(t, err)

	err = s.Start()
	require.NoError(t, err)

	// Make Alice's channel zero-conf or option-scid-alias (feature bit).
	aliceAlias := lnwire.ShortChannelID{
		BlockHeight: 16_000_000,
		TxIndex:     5,
		TxPosition:  5,
	}

	var aliceLink *mockChannelLink
	if zeroConf {
		aliceLink = newMockChannelLink(
			s, chanID1, aliceAlias, aliceChanID, alicePeer, true,
			true, true, false,
		)
	} else {
		aliceLink = newMockChannelLink(
			s, chanID1, aliceChanID, emptyScid, alicePeer, true,
			true, false, true,
		)
		aliceLink.addAlias(aliceAlias)
	}
	err = s.AddLink(aliceLink)
	require.NoError(t, err)

	bobLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	err = s.AddLink(bobLink)
	require.NoError(t, err)

	// Create a packet that will be sent from Alice to Bob via the switch.
	preimage := [sha256.Size]byte{1}
	rhash := sha256.Sum256(preimage[:])
	ogPacket := &htlcPacket{
		incomingChanID: aliceLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Forward the packet and check that Bob's channel link received it.
	err = s.ForwardPackets(nil, ogPacket)
	require.NoError(t, err)

	// Assert that the circuits are in the expected state.
	require.Equal(t, 1, s.circuits.NumPending())
	require.Equal(t, 0, s.circuits.NumOpen())

	// Pull packet from Bob's link, and do nothing with it.
	select {
	case <-bobLink.packets:
	case <-s.quit:
		t.Fatal("switch shutting down, failed to forward packet")
	}

	// Now we will restart the Switch to trigger the LoadedFromDisk logic.
	err = s.Stop()
	require.NoError(t, err)

	err = cdb.Close()
	require.NoError(t, err)

	cdb2 := channeldb.OpenForTesting(t, tempPath)

	s2, err := initSwitchWithDB(testStartingHeight, cdb2)
	require.NoError(t, err)

	err = s2.Start()
	require.NoError(t, err)

	defer func() {
		_ = s2.Stop()
	}()

	var aliceLink2 *mockChannelLink
	if zeroConf {
		aliceLink2 = newMockChannelLink(
			s2, chanID1, aliceAlias, aliceChanID, alicePeer, true,
			true, true, false,
		)
	} else {
		aliceLink2 = newMockChannelLink(
			s2, chanID1, aliceChanID, emptyScid, alicePeer, true,
			true, false, true,
		)
		aliceLink2.addAlias(aliceAlias)
	}
	err = s2.AddLink(aliceLink2)
	require.NoError(t, err)

	bobLink2 := newMockChannelLink(
		s2, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	err = s2.AddLink(bobLink2)
	require.NoError(t, err)

	// Reforward the ogPacket and wait for Alice to receive a failure
	// packet.
	err = s2.ForwardPackets(nil, ogPacket)
	require.NoError(t, err)

	select {
	case failPacket := <-aliceLink2.packets:
		// Assert that the failPacket does not leak UTXO information.
		// This means checking that aliceChanID was not returned.
		msg := failPacket.linkFailure.msg
		failMsg, ok := msg.(*lnwire.FailTemporaryChannelFailure)
		require.True(t, ok)
		require.Equal(t, aliceAlias, failMsg.Update.ShortChannelID)
	case <-s2.quit:
		t.Fatal("switch shutting down, failed to forward packet")
	}
}

// TestSwitchAliasFailAdd tests that the mailbox does not leak UTXO information
// when failing back an HTLC due to the 5-second timeout. This is tested in the
// switch rather than the mailbox because the mailbox tests do not have the
// proper context (e.g. the Switch's failAliasUpdate function). The caveat here
// is that if the private UTXO is already known, it is fine to send a failure
// back. This tests option-scid-alias (feature-bit) and zero-conf channels.
func TestSwitchAliasFailAdd(t *testing.T) {
	tests := []struct {
		name string

		// Denotes whether the opened channel will be zero-conf.
		zeroConf bool

		// Denotes whether the opened channel will be private.
		private bool

		// Denotes whether an alias was used during forwarding.
		useAlias bool
	}{
		{
			name:     "public zero-conf using alias",
			zeroConf: true,
			private:  false,
			useAlias: true,
		},
		{
			name:     "public zero-conf using real",
			zeroConf: true,
			private:  false,
			useAlias: true,
		},
		{
			name:     "private zero-conf using alias",
			zeroConf: true,
			private:  true,
			useAlias: true,
		},
		{
			name:     "public option-scid-alias using alias",
			zeroConf: false,
			private:  false,
			useAlias: true,
		},
		{
			name:     "public option-scid-alias using real",
			zeroConf: false,
			private:  false,
			useAlias: false,
		},
		{
			name:     "private option-scid-alias using alias",
			zeroConf: false,
			private:  true,
			useAlias: true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			testSwitchAliasFailAdd(
				t, test.zeroConf, test.private, test.useAlias,
			)
		})
	}
}

func testSwitchAliasFailAdd(t *testing.T, zeroConf, private, useAlias bool) {
	t.Parallel()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	tempPath := t.TempDir()

	cdb := channeldb.OpenForTesting(t, tempPath)

	s, err := initSwitchWithDB(testStartingHeight, cdb)
	require.NoError(t, err)

	// Change the mailOrchestrator's expiry to a second.
	s.mailOrchestrator.cfg.expiry = time.Second

	err = s.Start()
	require.NoError(t, err)

	defer func() {
		_ = s.Stop()
	}()

	// Make Alice's channel zero-conf or option-scid-alias (feature bit).
	aliceAlias := lnwire.ShortChannelID{
		BlockHeight: 16_000_000,
		TxIndex:     5,
		TxPosition:  5,
	}
	aliceAlias2 := aliceAlias
	aliceAlias2.TxPosition = 6

	var aliceLink *mockChannelLink
	if zeroConf {
		aliceLink = newMockChannelLink(
			s, chanID1, aliceAlias, aliceChanID, alicePeer, true,
			private, true, false,
		)
		aliceLink.addAlias(aliceAlias2)
	} else {
		aliceLink = newMockChannelLink(
			s, chanID1, aliceChanID, emptyScid, alicePeer, true,
			private, false, true,
		)
		aliceLink.addAlias(aliceAlias)
		aliceLink.addAlias(aliceAlias2)
	}
	err = s.AddLink(aliceLink)
	require.NoError(t, err)

	bobLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, true, false,
		false,
	)
	err = s.AddLink(bobLink)
	require.NoError(t, err)

	// Create a packet that Bob will send to Alice via ForwardPackets.
	preimage := [sha256.Size]byte{1}
	rhash := sha256.Sum256(preimage[:])
	ogPacket := &htlcPacket{
		incomingChanID: bobLink.ShortChanID(),
		incomingHTLCID: 0,
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Determine which outgoingChanID to set based on the useAlias boolean.
	outgoingChanID := aliceChanID
	if useAlias {
		// Choose randomly from the 2 possible aliases.
		aliases := aliceLink.getAliases()
		idx := mrand.Intn(len(aliases))

		outgoingChanID = aliases[idx]
	}

	ogPacket.outgoingChanID = outgoingChanID

	// Forward the packet so Alice's mailbox fails it backwards.
	err = s.ForwardPackets(nil, ogPacket)
	require.NoError(t, err)

	// Assert that the circuits are in the expected state.
	require.Equal(t, 1, s.circuits.NumPending())
	require.Equal(t, 0, s.circuits.NumOpen())

	// Wait to receive the packet from Bob's mailbox.
	select {
	case failPacket := <-bobLink.packets:
		// Assert that failPacket returns the expected SCID in the
		// ChannelUpdate.
		msg := failPacket.linkFailure.msg
		failMsg, ok := msg.(*lnwire.FailTemporaryChannelFailure)
		require.True(t, ok)
		require.Equal(t, outgoingChanID, failMsg.Update.ShortChannelID)
	case <-s.quit:
		t.Fatal("switch shutting down, failed to receive fail packet")
	}
}

// TestSwitchHandlePacketForwardAlias checks that handlePacketForward (which
// calls CheckHtlcForward) does not leak the UTXO in a failure message for
// alias channels. This test requires us to have a REAL link, which we also
// must modify in order to test it properly (e.g. making it a private channel).
// This doesn't lead to good code, but short of refactoring the link-generation
// code there is not a good alternative.
func TestSwitchHandlePacketForward(t *testing.T) {
	tests := []struct {
		name string

		// Denotes whether or not the channel will be zero-conf.
		zeroConf bool

		// Denotes whether or not the channel will have negotiated the
		// option-scid-alias feature-bit and is not zero-conf.
		optionFeature bool

		// Denotes whether or not the channel will be private.
		private bool

		// Denotes whether or not the alias will be used for
		// forwarding.
		useAlias bool
	}{
		{
			name:     "public zero-conf using alias",
			zeroConf: true,
			private:  false,
			useAlias: true,
		},
		{
			name:     "public zero-conf using real",
			zeroConf: true,
			private:  false,
			useAlias: false,
		},
		{
			name:     "private zero-conf using alias",
			zeroConf: true,
			private:  true,
			useAlias: true,
		},
		{
			name:          "public option-scid-alias using alias",
			zeroConf:      false,
			optionFeature: true,
			private:       false,
			useAlias:      true,
		},
		{
			name:          "public option-scid-alias using real",
			zeroConf:      false,
			optionFeature: true,
			private:       false,
			useAlias:      false,
		},
		{
			name:          "private option-scid-alias using alias",
			zeroConf:      false,
			optionFeature: true,
			private:       true,
			useAlias:      true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			testSwitchHandlePacketForward(
				t, test.zeroConf, test.private, test.useAlias,
				test.optionFeature,
			)
		})
	}
}

func testSwitchHandlePacketForward(t *testing.T, zeroConf, private,
	useAlias, optionFeature bool) {

	t.Parallel()

	// Create a link for Alice that we'll add to the switch.
	harness, err :=
		newSingleLinkTestHarness(t, btcutil.SatoshiPerBitcoin, 0)
	require.NoError(t, err)

	aliceLink := harness.aliceLink

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer func() {
		_ = s.Stop()
	}()

	// Change Alice's ShortChanID and OtherShortChanID here.
	aliceAlias := lnwire.ShortChannelID{
		BlockHeight: 16_000_000,
		TxIndex:     5,
		TxPosition:  5,
	}
	aliceAlias2 := aliceAlias
	aliceAlias2.TxPosition = 6

	aliceChannelLink := aliceLink.(*channelLink)
	aliceChannelState := aliceChannelLink.channel.State()

	// Set the link's GetAliases function.
	aliceChannelLink.cfg.GetAliases = func(
		base lnwire.ShortChannelID) []lnwire.ShortChannelID {

		return []lnwire.ShortChannelID{aliceAlias, aliceAlias2}
	}

	if !private {
		// Change the channel to public depending on the test.
		aliceChannelState.ChannelFlags = lnwire.FFAnnounceChannel
	}

	// If this is an option-scid-alias feature-bit non-zero-conf channel,
	// we'll mark the channel as such.
	if optionFeature {
		aliceChannelState.ChanType |= channeldb.ScidAliasFeatureBit
	}

	// This is the ShortChannelID field in the OpenChannel struct.
	aliceScid := aliceLink.ShortChanID()
	if zeroConf {
		// Store the alias in the shortChanID field and mark the real
		// scid in the database.
		err = aliceChannelState.MarkRealScid(aliceScid)
		require.NoError(t, err)

		aliceChannelState.ChanType |= channeldb.ZeroConfBit
	}

	err = s.AddLink(aliceLink)
	require.NoError(t, err)

	// Add a mockChannelLink for Bob.
	bobChanID, bobScid := genID()
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	bobLink := newMockChannelLink(
		s, bobChanID, bobScid, emptyScid, bobPeer, true, false, false,
		false,
	)
	err = s.AddLink(bobLink)
	require.NoError(t, err)

	preimage := [sha256.Size]byte{1}
	rhash := sha256.Sum256(preimage[:])
	ogPacket := &htlcPacket{
		incomingChanID: bobLink.ShortChanID(),
		incomingHTLCID: 0,
		incomingAmount: 1000,
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Determine which outgoingChanID to set based on the useAlias bool.
	outgoingChanID := aliceScid
	if useAlias {
		// Choose from the possible aliases.
		aliases := aliceLink.getAliases()
		idx := mrand.Intn(len(aliases))

		outgoingChanID = aliases[idx]
	}

	ogPacket.outgoingChanID = outgoingChanID

	// Forward the packet to Alice and she should fail it back with an
	// AmountBelowMinimum FailureMessage.
	err = s.ForwardPackets(nil, ogPacket)
	require.NoError(t, err)

	select {
	case failPacket := <-bobLink.packets:
		// Assert that failPacket returns the expected ChannelUpdate.
		msg := failPacket.linkFailure.msg
		failMsg, ok := msg.(*lnwire.FailAmountBelowMinimum)
		require.True(t, ok)
		require.Equal(t, outgoingChanID, failMsg.Update.ShortChannelID)
	case <-s.quit:
		t.Fatal("switch shutting down, failed to receive failure")
	}
}

// TestSwitchAliasInterceptFail tests that when the InterceptableSwitch fails
// an incoming HTLC, it does not leak the on-chain UTXO for option-scid-alias
// (feature bit) or zero-conf channels.
func TestSwitchAliasInterceptFail(t *testing.T) {
	tests := []struct {
		name string

		// Denotes whether or not the incoming channel is a zero-conf
		// channel or an option-scid-alias channel instead (feature
		// bit).
		zeroConf bool
	}{
		{
			name:     "option-scid-alias",
			zeroConf: false,
		},
		{
			name:     "zero-conf",
			zeroConf: true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			testSwitchAliasInterceptFail(t, test.zeroConf)
		})
	}
}

func testSwitchAliasInterceptFail(t *testing.T, zeroConf bool) {
	t.Parallel()

	chanID, aliceScid := genID()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err)

	tempPath := t.TempDir()

	cdb := channeldb.OpenForTesting(t, tempPath)

	s, err := initSwitchWithDB(testStartingHeight, cdb)
	require.NoError(t, err)

	err = s.Start()
	require.NoError(t, err)

	defer func() {
		_ = s.Stop()
	}()

	// Make Alice's alias here.
	aliceAlias := lnwire.ShortChannelID{
		BlockHeight: 16_000_000,
		TxIndex:     5,
		TxPosition:  5,
	}
	aliceAlias2 := aliceAlias
	aliceAlias2.TxPosition = 6

	var aliceLink *mockChannelLink
	if zeroConf {
		aliceLink = newMockChannelLink(
			s, chanID, aliceAlias, aliceScid, alicePeer, true,
			true, true, false,
		)
		aliceLink.addAlias(aliceAlias2)
	} else {
		aliceLink = newMockChannelLink(
			s, chanID, aliceScid, emptyScid, alicePeer, true,
			true, false, true,
		)
		aliceLink.addAlias(aliceAlias)
		aliceLink.addAlias(aliceAlias2)
	}
	err = s.AddLink(aliceLink)
	require.NoError(t, err)

	// Now we'll create the packet that will be sent from the Alice link.
	preimage := [sha256.Size]byte{1}
	rhash := sha256.Sum256(preimage[:])
	ogPacket := &htlcPacket{
		incomingChanID:  aliceLink.ShortChanID(),
		incomingTimeout: 1000,
		incomingHTLCID:  0,
		outgoingChanID:  lnwire.ShortChannelID{},
		obfuscator:      NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Now setup the interceptable switch so that we can reject this
	// packet.
	forwardInterceptor := &mockForwardInterceptor{
		t:               t,
		interceptedChan: make(chan InterceptedPacket),
	}

	notifier := &mock.ChainNotifier{
		EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
	}
	notifier.EpochChan <- &chainntnfs.BlockEpoch{Height: testStartingHeight}

	interceptSwitch, err := NewInterceptableSwitch(
		&InterceptableSwitchConfig{
			Switch:             s,
			Notifier:           notifier,
			CltvRejectDelta:    10,
			CltvInterceptDelta: 13,
		},
	)
	require.NoError(t, err)
	require.NoError(t, interceptSwitch.Start())
	interceptSwitch.SetInterceptor(forwardInterceptor.InterceptForwardHtlc)

	err = interceptSwitch.ForwardPackets(nil, false, ogPacket)
	require.NoError(t, err)

	inCircuit := forwardInterceptor.getIntercepted().IncomingCircuit
	require.NoError(t, interceptSwitch.resolve(&FwdResolution{
		Action:      FwdActionFail,
		Key:         inCircuit,
		FailureCode: lnwire.CodeTemporaryChannelFailure,
	}))

	select {
	case failPacket := <-aliceLink.packets:
		// Assert that failPacket returns the expected ChannelUpdate.
		failHtlc, ok := failPacket.htlc.(*lnwire.UpdateFailHTLC)
		require.True(t, ok)

		fwdErr, err := newMockDeobfuscator().DecryptError(
			failHtlc.Reason,
		)
		require.NoError(t, err)

		failure := fwdErr.WireMessage()

		failureMsg, ok := failure.(*lnwire.FailTemporaryChannelFailure)
		require.True(t, ok)

		failScid := failureMsg.Update.ShortChannelID
		isAlias := failScid == aliceAlias || failScid == aliceAlias2
		require.True(t, isAlias)

	case <-s.quit:
		t.Fatalf("switch shutting down, failed to receive failure")
	}

	require.NoError(t, interceptSwitch.Stop())
}
