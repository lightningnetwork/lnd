package sphinx

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec"
)

var (
	s *OnionPacket
	p *ProcessedPacket
)

func BenchmarkPathPacketConstruction(b *testing.B) {
	b.StopTimer()

	var (
		err          error
		sphinxPacket *OnionPacket
		route        PaymentPath
	)

	for i := 0; i < NumMaxHops; i++ {
		privKey, err := btcec.NewPrivateKey(btcec.S256())
		if err != nil {
			b.Fatalf("unable to generate key: %v", privKey)
		}

		hopData := HopData{
			Realm:         [1]byte{0x00},
			ForwardAmount: uint64(i),
			OutgoingCltv:  uint32(i),
		}
		copy(hopData.NextAddress[:], bytes.Repeat([]byte{byte(i)}, 8))

		route[i] = OnionHop{
			NodePub: *privKey.PubKey(),
			HopData: hopData,
		}
	}

	d, _ := btcec.PrivKeyFromBytes(btcec.S256(), bytes.Repeat([]byte{'A'}, 32))

	b.ReportAllocs()

	b.StartTimer()

	for i := 0; i < b.N; i++ {
		sphinxPacket, err = NewOnionPacket(&route, d, nil)
		if err != nil {
			b.Fatalf("unable to create packet: %v", err)
		}
	}

	s = sphinxPacket
}

func BenchmarkProcessPacket(b *testing.B) {
	b.StopTimer()
	path, _, _, sphinxPacket, err := newTestRoute(1)
	if err != nil {
		b.Fatalf("unable to create test route: %v", err)
	}
	b.ReportAllocs()
	path[0].log.Start()
	defer path[0].log.Stop()
	b.StartTimer()

	var (
		pkt *ProcessedPacket
	)
	for i := 0; i < b.N; i++ {
		pkt, err = path[0].ProcessOnionPacket(sphinxPacket, nil, uint32(i))
		if err != nil {
			b.Fatalf("unable to process packet %d: %v", i, err)
		}

		b.StopTimer()
		router := path[0]
		router.log.Stop()
		path[0] = &Router{
			nodeID:   router.nodeID,
			nodeAddr: router.nodeAddr,
			onionKey: router.onionKey,
			log:      NewMemoryReplayLog(),
		}
		path[0].log.Start()
		b.StartTimer()
	}

	p = pkt
}
