package wtwire_test

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

func randRawFeatureVector(r *rand.Rand) *lnwire.RawFeatureVector {
	featureVec := lnwire.NewRawFeatureVector()
	for i := 0; i < 10000; i++ {
		if r.Int31n(2) == 0 {
			featureVec.Set(lnwire.FeatureBit(i))
		}
	}
	return featureVec
}

func randChainHash(r *rand.Rand) chainhash.Hash {
	var hash chainhash.Hash
	r.Read(hash[:])
	return hash
}

// TestWatchtowerWireProtocol uses the testing/quick package to create a series
// of fuzz tests to attempt to break a primary scenario which is implemented as
// property based testing scenario.
func TestWatchtowerWireProtocol(t *testing.T) {
	t.Parallel()

	// mainScenario is the primary test that will programmatically be
	// executed for all registered wire messages. The quick-checker within
	// testing/quick will attempt to find an input to this function, s.t
	// the function returns false, if so then we've found an input that
	// violates our model of the system.
	mainScenario := func(msg wtwire.Message) bool {
		// Give a new message, we'll serialize the message into a new
		// bytes buffer.
		var b bytes.Buffer
		if _, err := wtwire.WriteMessage(&b, msg, 0); err != nil {
			t.Fatalf("unable to write msg: %v", err)
			return false
		}

		// Next, we'll ensure that the serialized payload (subtracting
		// the 2 bytes for the message type) is _below_ the specified
		// max payload size for this message.
		payloadLen := uint32(b.Len()) - 2
		if payloadLen > msg.MaxPayloadLength(0) {
			t.Fatalf("msg payload constraint violated: %v > %v",
				payloadLen, msg.MaxPayloadLength(0))
			return false
		}

		// Finally, we'll deserialize the message from the written
		// buffer, and finally assert that the messages are equal.
		newMsg, err := wtwire.ReadMessage(&b, 0)
		if err != nil {
			t.Fatalf("unable to read msg: %v", err)
			return false
		}
		if !reflect.DeepEqual(msg, newMsg) {
			t.Fatalf("messages don't match after re-encoding: %v "+
				"vs %v", spew.Sdump(msg), spew.Sdump(newMsg))
			return false
		}

		return true
	}

	customTypeGen := map[wtwire.MessageType]func([]reflect.Value, *rand.Rand){
		wtwire.MsgInit: func(v []reflect.Value, r *rand.Rand) {
			req := wtwire.NewInitMessage(
				randRawFeatureVector(r),
				randChainHash(r),
			)

			v[0] = reflect.ValueOf(*req)
		},
	}

	// With the above types defined, we'll now generate a slice of
	// scenarios to feed into quick.Check. The function scans in input
	// space of the target function under test, so we'll need to create a
	// series of wrapper functions to force it to iterate over the target
	// types, but re-use the mainScenario defined above.
	tests := []struct {
		msgType  wtwire.MessageType
		scenario interface{}
	}{
		{
			msgType: wtwire.MsgInit,
			scenario: func(m wtwire.Init) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: wtwire.MsgCreateSession,
			scenario: func(m wtwire.CreateSession) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: wtwire.MsgCreateSessionReply,
			scenario: func(m wtwire.CreateSessionReply) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: wtwire.MsgStateUpdate,
			scenario: func(m wtwire.StateUpdate) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: wtwire.MsgStateUpdateReply,
			scenario: func(m wtwire.StateUpdateReply) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: wtwire.MsgDeleteSession,
			scenario: func(m wtwire.DeleteSession) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: wtwire.MsgDeleteSessionReply,
			scenario: func(m wtwire.DeleteSessionReply) bool {
				return mainScenario(&m)
			},
		},
		{
			msgType: wtwire.MsgError,
			scenario: func(m wtwire.Error) bool {
				return mainScenario(&m)
			},
		},
	}
	for _, test := range tests {
		var config *quick.Config

		// If the type defined is within the custom type gen map above,
		// the we'll modify the default config to use this Value
		// function that knows how to generate the proper types.
		if valueGen, ok := customTypeGen[test.msgType]; ok {
			config = &quick.Config{
				Values: valueGen,
			}
		}

		t.Logf("Running fuzz tests for msgType=%v", test.msgType)
		if err := quick.Check(test.scenario, config); err != nil {
			t.Fatalf("fuzz checks for msg=%v failed: %v",
				test.msgType, err)
		}
	}

}

func init() {
	rand.Seed(time.Now().Unix())
}
