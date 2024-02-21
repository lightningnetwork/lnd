package wtdb_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/stretchr/testify/require"
)

func randPubKey() (*btcec.PublicKey, error) {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}

	return priv.PubKey(), nil
}

func randTCP4Addr(r *rand.Rand) (*net.TCPAddr, error) {
	var ip [4]byte
	if _, err := r.Read(ip[:]); err != nil {
		return nil, err
	}

	var port [2]byte
	if _, err := r.Read(port[:]); err != nil {
		return nil, err
	}

	addrIP := net.IP(ip[:])
	addrPort := int(binary.BigEndian.Uint16(port[:]))

	return &net.TCPAddr{IP: addrIP, Port: addrPort}, nil
}

func randTCP6Addr(r *rand.Rand) (*net.TCPAddr, error) {
	var ip [16]byte
	if _, err := r.Read(ip[:]); err != nil {
		return nil, err
	}

	var port [2]byte
	if _, err := r.Read(port[:]); err != nil {
		return nil, err
	}

	addrIP := net.IP(ip[:])
	addrPort := int(binary.BigEndian.Uint16(port[:]))

	return &net.TCPAddr{IP: addrIP, Port: addrPort}, nil
}

func randV2OnionAddr(r *rand.Rand) (*tor.OnionAddr, error) {
	var serviceID [tor.V2DecodedLen]byte
	if _, err := r.Read(serviceID[:]); err != nil {
		return nil, err
	}

	var port [2]byte
	if _, err := r.Read(port[:]); err != nil {
		return nil, err
	}

	onionService := tor.Base32Encoding.EncodeToString(serviceID[:])
	onionService += tor.OnionSuffix
	addrPort := int(binary.BigEndian.Uint16(port[:]))

	return &tor.OnionAddr{OnionService: onionService, Port: addrPort}, nil
}

func randV3OnionAddr(r *rand.Rand) (*tor.OnionAddr, error) {
	var serviceID [tor.V3DecodedLen]byte
	if _, err := r.Read(serviceID[:]); err != nil {
		return nil, err
	}

	var port [2]byte
	if _, err := r.Read(port[:]); err != nil {
		return nil, err
	}

	onionService := tor.Base32Encoding.EncodeToString(serviceID[:])
	onionService += tor.OnionSuffix
	addrPort := int(binary.BigEndian.Uint16(port[:]))

	return &tor.OnionAddr{OnionService: onionService, Port: addrPort}, nil
}

func randAddrs(r *rand.Rand) ([]net.Addr, error) {
	tcp4Addr, err := randTCP4Addr(r)
	if err != nil {
		return nil, err
	}

	tcp6Addr, err := randTCP6Addr(r)
	if err != nil {
		return nil, err
	}

	v2OnionAddr, err := randV2OnionAddr(r)
	if err != nil {
		return nil, err
	}

	v3OnionAddr, err := randV3OnionAddr(r)
	if err != nil {
		return nil, err
	}

	return []net.Addr{tcp4Addr, tcp6Addr, v2OnionAddr, v3OnionAddr}, nil
}

// dbObject is abstract object support encoding and decoding.
type dbObject interface {
	Encode(io.Writer) error
	Decode(io.Reader) error
}

// TestCodec serializes and deserializes wtdb objects in order to test that the
// codec understands all of the required field types. The test also asserts that
// decoding an object into another results in an equivalent object.
func TestCodec(tt *testing.T) {

	var t *testing.T
	mainScenario := func(obj dbObject) bool {
		// Ensure encoding the object succeeds.
		var b bytes.Buffer
		err := obj.Encode(&b)
		require.NoError(t, err)

		var obj2 dbObject
		switch obj.(type) {
		case *wtdb.SessionInfo:
			obj2 = &wtdb.SessionInfo{}
		case *wtdb.SessionStateUpdate:
			obj2 = &wtdb.SessionStateUpdate{}
		case *wtdb.ClientSessionBody:
			obj2 = &wtdb.ClientSessionBody{}
		case *wtdb.CommittedUpdateBody:
			obj2 = &wtdb.CommittedUpdateBody{}
		case *wtdb.BackupID:
			obj2 = &wtdb.BackupID{}
		case *wtdb.Tower:
			obj2 = &wtdb.Tower{}
		case *wtdb.ClientChanSummary:
			obj2 = &wtdb.ClientChanSummary{}
		default:
			t.Fatalf("unknown type: %T", obj)
			return false
		}

		// Ensure decoding the object succeeds.
		err = obj2.Decode(bytes.NewReader(b.Bytes()))
		require.NoError(t, err)

		// Assert the original and decoded object match.
		require.Equal(t, obj, obj2)

		return true
	}

	customTypeGen := map[string]func([]reflect.Value, *rand.Rand){
		"Tower": func(v []reflect.Value, r *rand.Rand) {
			pk, err := randPubKey()
			require.NoError(t, err)

			addrs, err := randAddrs(r)
			require.NoError(t, err)

			obj := wtdb.Tower{
				IdentityKey: pk,
				Addresses:   addrs,
				Status:      wtdb.TowerStatus(r.Uint32()),
			}

			v[0] = reflect.ValueOf(obj)
		},
	}

	tests := []struct {
		name     string
		scenario interface{}
	}{
		{
			name: "SessionInfo",
			scenario: func(obj wtdb.SessionInfo) bool {
				return mainScenario(&obj)
			},
		},
		{
			name: "SessionStateUpdate",
			scenario: func(obj wtdb.SessionStateUpdate) bool {
				return mainScenario(&obj)
			},
		},
		{
			name: "ClientSessionBody",
			scenario: func(obj wtdb.ClientSessionBody) bool {
				return mainScenario(&obj)
			},
		},
		{
			name: "CommittedUpdateBody",
			scenario: func(obj wtdb.CommittedUpdateBody) bool {
				return mainScenario(&obj)
			},
		},
		{
			name: "BackupID",
			scenario: func(obj wtdb.BackupID) bool {
				return mainScenario(&obj)
			},
		},
		{
			name: "Tower",
			scenario: func(obj wtdb.Tower) bool {
				return mainScenario(&obj)
			},
		},
		{
			name: "ClientChanSummary",
			scenario: func(obj wtdb.ClientChanSummary) bool {
				return mainScenario(&obj)
			},
		},
	}

	for _, test := range tests {
		tt.Run(test.name, func(h *testing.T) {
			t = h

			var config *quick.Config
			if valueGen, ok := customTypeGen[test.name]; ok {
				config = &quick.Config{
					Values: valueGen,
				}
			}

			err := quick.Check(test.scenario, config)
			require.NoError(h, err)
		})
	}
}
