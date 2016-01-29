package uspv

import (
	"fmt"
	"log"
	"os"

	"github.com/btcsuite/btcd/wire"
)

func (s *SPVCon) incomingMessageHandler() {
	for {
		n, xm, _, err := wire.ReadMessageN(s.con, s.localVersion, s.param.Net)
		if err != nil {
			log.Printf("ReadMessageN error.  Disconnecting: %s\n", err.Error())
			return
		}
		s.RBytes += uint64(n)
		//		log.Printf("Got %d byte %s message\n", n, xm.Command())
		switch m := xm.(type) {
		case *wire.MsgVersion:
			log.Printf("Got version message.  Agent %s, version %d, at height %d\n",
				m.UserAgent, m.ProtocolVersion, m.LastBlock)
			s.remoteVersion = uint32(m.ProtocolVersion) // weird cast! bug?
		case *wire.MsgVerAck:
			log.Printf("Got verack.  Whatever.\n")
		case *wire.MsgAddr:
			log.Printf("got %d addresses.\n", len(m.AddrList))
		case *wire.MsgPing:
			// log.Printf("Got a ping message.  We should pong back or they will kick us off.")
			go s.PongBack(m.Nonce)
		case *wire.MsgPong:
			log.Printf("Got a pong response. OK.\n")
		case *wire.MsgMerkleBlock:
			err = s.IngestMerkleBlock(m)
			if err != nil {
				log.Printf("Merkle block error: %s\n", err.Error())
				continue
			}
		case *wire.MsgHeaders:
			go s.HeaderHandler(m)
		case *wire.MsgTx:
			go s.TxHandler(m)
		case *wire.MsgReject:
			log.Printf("Rejected! cmd: %s code: %s tx: %s reason: %s",
				m.Cmd, m.Code.String(), m.Hash.String(), m.Reason)
		case *wire.MsgInv:
			go s.InvHandler(m)
		case *wire.MsgNotFound:
			log.Printf("Got not found response from remote:")
			for i, thing := range m.InvList {
				log.Printf("\t$d) %s: %s", i, thing.Type, thing.Hash)
			}

		default:
			log.Printf("Got unknown message type %s\n", m.Command())
		}
	}
	return
}

// this one seems kindof pointless?  could get ridf of it and let
// functions call WriteMessageN themselves...
func (s *SPVCon) outgoingMessageHandler() {
	for {
		msg := <-s.outMsgQueue
		n, err := wire.WriteMessageN(s.con, msg, s.localVersion, s.param.Net)
		if err != nil {
			log.Printf("Write message error: %s", err.Error())
		}
		s.WBytes += uint64(n)
	}
	return
}

// fPositiveHandler monitors false positives and when it gets enough of them,
//
func (s *SPVCon) fPositiveHandler() {
	var fpAccumulator int32
	for {
		fpAccumulator += <-s.fPositives // blocks here
		if fpAccumulator > 7 {
			filt, err := s.TS.GimmeFilter()
			if err != nil {
				log.Printf("Filter creation error: %s\n", err.Error())
				log.Printf("uhoh, crashing filter handler")
				return
			}

			// send filter
			s.SendFilter(filt)
			fmt.Printf("sent filter %x\n", filt.MsgFilterLoad().Filter)
			// clear the channel
			for len(s.fPositives) != 0 {
				fpAccumulator += <-s.fPositives
			}
			fmt.Printf("reset %d false positives\n", fpAccumulator)
			// reset accumulator
			fpAccumulator = 0
		}
	}
}

func (s *SPVCon) HeaderHandler(m *wire.MsgHeaders) {
	moar, err := s.IngestHeaders(m)
	if err != nil {
		log.Printf("Header error: %s\n", err.Error())
		return
	}
	// if we got post DB syncheight headers, get merkleblocks for them
	// this is always true except for first pre-birthday sync
	syncTip, err := s.TS.GetDBSyncHeight()
	if err != nil {
		log.Printf("Header error: %s", err.Error())
		return
	}
	endPos, err := s.headerFile.Seek(0, os.SEEK_END)
	if err != nil {
		log.Printf("Header error: %s", err.Error())
		return
	}
	tip := int32(endPos/80) - 1 // move back 1 header length to read

	// checked header lenght, start req for more if needed
	if moar {
		s.AskForHeaders()
	}

	if syncTip < tip {
		fmt.Printf("syncTip %d headerTip %d\n", syncTip, tip)
		err = s.AskForMerkBlocks(syncTip, tip)
		if err != nil {
			log.Printf("AskForMerkBlocks error: %s", err.Error())
			return
		}
	}
}

func (s *SPVCon) TxHandler(m *wire.MsgTx) {
	hits, err := s.TS.AckTx(m)
	if err != nil {
		log.Printf("Incoming Tx error: %s\n", err.Error())
	}
	if hits == 0 {
		log.Printf("tx %s had no hits, filter false positive.",
			m.TxSha().String())
		s.fPositives <- 1 // add one false positive to chan
	} else {
		log.Printf("tx %s ingested and matches utxo/adrs.",
			m.TxSha().String())
	}
}

func (s *SPVCon) InvHandler(m *wire.MsgInv) {
	log.Printf("got inv.  Contains:\n")
	for i, thing := range m.InvList {
		log.Printf("\t%d)%s : %s",
			i, thing.Type.String(), thing.Hash.String())
		if thing.Type == wire.InvTypeTx { // new tx, ingest
			s.TS.OKTxids[thing.Hash] = 0 // unconfirmed
			s.AskForTx(thing.Hash)
		}
		if thing.Type == wire.InvTypeBlock { // new block, ingest
			if len(s.mBlockQueue) == 0 {
				// don't ask directly; instead ask for header
				fmt.Printf("asking for headers due to inv block\n")
				//				s.AskForHeaders()
			} else {
				fmt.Printf("inv block but ignoring, not synched\n")
			}
		}
	}
}
