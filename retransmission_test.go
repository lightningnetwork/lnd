package main

import (
	"testing"

	"sort"

	"github.com/lightningnetwork/lnd/lnwire"
)

var ackTestVector = []struct {
	name string
	send lnwire.Message
	recv lnwire.Message
}{
	{
		name: "open_channel -> accept_channel",
		send: &lnwire.SingleFundingRequest{},
		recv: &lnwire.SingleFundingResponse{},
	},
	{
		name: "accept_channel -> funding_created",
		send: &lnwire.SingleFundingResponse{},
		recv: &lnwire.SingleFundingComplete{},
	},
	{
		name: "funding_created -> funding_signed",
		send: &lnwire.SingleFundingComplete{},
		recv: &lnwire.SingleFundingSignComplete{},
	},
	{
		name: "funding_signed -> funding_locked",
		send: &lnwire.SingleFundingSignComplete{},
		recv: &lnwire.FundingLocked{},
	},
	{
		name: "funding_locked -> update_add_htlc",
		send: &lnwire.FundingLocked{},
		recv: &lnwire.UpdateAddHTLC{},
	},
	{
		name: "funding_locked -> update_fulfill_htlc",
		send: &lnwire.FundingLocked{},
		recv: &lnwire.UpdateFufillHTLC{},
	},
	{
		name: "funding_locked -> update_fail_htlc",
		send: &lnwire.FundingLocked{},
		recv: &lnwire.UpdateFailHTLC{},
	},
	// TODO(andrew.shvv) uncomment after update_fail_malformed_htlc will
	// be included
	//{
	//	name: "funding_locked -> update_fail_malformed_htlc",
	//	send: &lnwire.SingleFundingOpenProof{},
	//	recv: ,
	//},
	{
		name: "funding_locked -> commitment_signed",
		send: &lnwire.FundingLocked{},
		recv: &lnwire.CommitSig{},
	},
	{
		name: "funding_locked -> revoke_and_ack",
		send: &lnwire.FundingLocked{},
		recv: &lnwire.RevokeAndAck{},
	},
	{
		name: "funding_locked -> shutdown",
		send: &lnwire.FundingLocked{},
		recv: &lnwire.CloseRequest{},
	},
	{
		name: "update_add_htlc -> revoke_and_ack",
		send: &lnwire.UpdateAddHTLC{},
		recv: &lnwire.RevokeAndAck{},
	},
	{
		name: "update_fulfill_htlc -> revoke_and_ack",
		send: &lnwire.UpdateFufillHTLC{},
		recv: &lnwire.RevokeAndAck{},
	},
	{
		name: "update_fail_htlc -> revoke_and_ack",
		send: &lnwire.UpdateFailHTLC{},
		recv: &lnwire.RevokeAndAck{},
	},
	// TODO(andrew.shvv) uncomment after update_fail_malformed_htlc will
	// be included
	//{
	//	name: "update_fail_malformed_htlc -> revoke_and_ack",
	//	send: ,
	//	recv: &lnwire.RevokeAndAck{},
	//},
	{
		name: "commitment_signed -> revoke_and_ack",
		send: &lnwire.CommitSig{},
		recv: &lnwire.RevokeAndAck{},
	},
	{
		name: "revoke_and_ack -> commitment_signed",
		send: &lnwire.RevokeAndAck{},
		recv: &lnwire.CommitSig{},
	},
	{
		name: "revoke_and_ack -> shutdown",
		send: &lnwire.RevokeAndAck{},
		recv: &lnwire.CloseRequest{},
	},

	{
		name: "shutdown -> revoke_and_ack",
		send: &lnwire.CloseRequest{},
		recv: &lnwire.RevokeAndAck{},
	},
	{
		name: "shutdown -> closing_signed",
		send: &lnwire.CloseRequest{},
		recv: &lnwire.CloseComplete{},
	},
}

// MockStore map implementation of message storage which not requires the
// messages to be decoded/encoded which means that we shouldn't populate the
// lnwire message with data.
type MockStore struct {
	sequence uint64
	messages map[uint64]lnwire.Message
}

func (s *MockStore) Get() ([]uint64, []lnwire.Message, error) {
	indexes := make([]int, len(s.messages))
	messages := make([]lnwire.Message, len(s.messages))

	i := 0
	for index := range s.messages {
		indexes[i] = int(index)
		i++
	}
	sort.Ints(indexes)

	uindexes := make([]uint64, len(s.messages))
	for i, index := range indexes {
		messages[i] = s.messages[uint64(index)]
		uindexes[i] = uint64(index)
	}

	return uindexes, messages, nil
}
func (s *MockStore) Add(msg lnwire.Message) (uint64, error) {
	index := s.sequence
	s.messages[index] = msg
	s.sequence++

	return index, nil
}

func (s *MockStore) Remove(indexes []uint64) error {
	for _, index := range indexes {
		delete(s.messages, index)
	}
	return nil
}

// TestRetransmitterSpecVector tests the behaviour of retransmission
// subsystem which is described in specification.
func TestRetransmitterSpecVector(t *testing.T) {

	s := &MockStore{messages: make(map[uint64]lnwire.Message)}

	rt, err := newRetransmitter(s)
	if err != nil {
		t.Fatalf("can't init retransmitter: %v", err)
	}

	for _, test := range ackTestVector {
		if err := rt.Register(test.send); err != nil {
			t.Fatalf("can't register message: %v", err)
		}

		_, messages, _ := s.Get()
		if len(messages) != 1 {
			t.Fatalf("test(%v): message(%v) wasn't registered",
				test.name, test.send.Command())
		}

		if err := rt.Ack(test.recv); err != nil {
			t.Fatalf("can't ack message: %v", err)
		}

		_, messages, _ = s.Get()
		if len(messages) != 0 {
			t.Fatalf("test(%v): message(%v) wasn't acked",
				test.name, test.send.Command())
		}
	}
}
