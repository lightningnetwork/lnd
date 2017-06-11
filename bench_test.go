package sphinx

import (
	"bytes"
	"testing"

	"github.com/roasbeef/btcd/btcec"
)

var (
	s *OnionPacket
	p *ProcessedPacket
)

func BenchmarkPathPacketConstruction(b *testing.B) {
	b.StopTimer()
	route := make([]*btcec.PublicKey, NumMaxHops)
	for i := 0; i < NumMaxHops; i++ {
		privKey, err := btcec.NewPrivateKey(btcec.S256())
		if err != nil {
			b.Fatalf("unable to generate key: %v", privKey)
		}

		route[i] = privKey.PubKey()
	}

	var (
		err          error
		sphinxPacket *OnionPacket
	)

	var hopsData []HopData
	for i := 0; i < len(route); i++ {
		hopsData = append(hopsData, HopData{
			Realm:         0x00,
			ForwardAmount: uint64(i),
			OutgoingCltv:  uint32(i),
		})
		copy(hopsData[i].NextAddress[:], bytes.Repeat([]byte{byte(i)}, 8))
	}

	d, _ := btcec.PrivKeyFromBytes(btcec.S256(), bytes.Repeat([]byte{'A'}, 32))
	b.StartTimer()

	for i := 0; i < b.N; i++ {
		sphinxPacket, err = NewOnionPacket(route, d, hopsData, nil)
		if err != nil {
			b.Fatalf("unable to create packet: %v", err)
		}
	}

	s = sphinxPacket
}

func BenchmarkProcessPacket(b *testing.B) {
	b.StopTimer()
	path, _, sphinxPacket, err := newTestRoute(1)
	if err != nil {
		b.Fatalf("unable to create test route: %v", err)
	}
	b.StartTimer()

	var (
		pkt *ProcessedPacket
	)
	for i := 0; i < b.N; i++ {
		pkt, err = path[0].ProcessOnionPacket(sphinxPacket, nil)
		if err != nil {
			b.Fatalf("unable to process packet: %v", err)
		}

		b.StopTimer()
		path[0].seenSecrets = make(map[[sharedSecretSize]byte]struct{})
		b.StartTimer()
	}

	p = pkt
}
