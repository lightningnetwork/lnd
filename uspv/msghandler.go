package uspv

import (
	"fmt"
	"log"

	"github.com/btcsuite/btcd/wire"
)

func (s *SPVCon) incomingMessageHandler() {
	for {
		n, xm, _, err := wire.ReadMessageN(s.con, s.localVersion, s.TS.Param.Net)
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
		case *wire.MsgBlock:
			s.IngestBlock(m)
		case *wire.MsgMerkleBlock:
			s.IngestMerkleBlock(m)
		case *wire.MsgHeaders: // concurrent because we keep asking for blocks
			go s.HeaderHandler(m)
		case *wire.MsgTx: // not concurrent! txs must be in order
			s.TxHandler(m)
		case *wire.MsgReject:
			log.Printf("Rejected! cmd: %s code: %s tx: %s reason: %s",
				m.Cmd, m.Code.String(), m.Hash.String(), m.Reason)
		case *wire.MsgInv:
			s.InvHandler(m)
		case *wire.MsgNotFound:
			log.Printf("Got not found response from remote:")
			for i, thing := range m.InvList {
				log.Printf("\t$d) %s: %s", i, thing.Type, thing.Hash)
			}
		case *wire.MsgGetData:
			s.GetDataHandler(m)

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
		n, err := wire.WriteMessageN(s.con, msg, s.localVersion, s.TS.Param.Net)
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
		finClear:
			for {
				select {
				case x := <-s.fPositives:
					fpAccumulator += x
				default:
					break finClear
				}
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
	// more to get? if so, ask for them and return
	if moar {
		err = s.AskForHeaders()
		if err != nil {
			log.Printf("AskForHeaders error: %s", err.Error())
		}
		return
	}
	// no moar, done w/ headers, get blocks
	err = s.AskForBlocks()
	if err != nil {
		log.Printf("AskForBlocks error: %s", err.Error())
		return
	}
}

// TxHandler takes in transaction messages that come in from either a request
// after an inv message or after a merkle block message.
func (s *SPVCon) TxHandler(m *wire.MsgTx) {
	s.TS.OKMutex.Lock()
	height, ok := s.TS.OKTxids[m.TxSha()]
	s.TS.OKMutex.Unlock()
	if !ok {
		log.Printf("Tx %s unknown, will not ingest\n", m.TxSha().String())
		return
	}

	// check for double spends
	allTxs, err := s.TS.GetAllTxs()
	if err != nil {
		log.Printf("Can't get txs from db: %s", err.Error())
		return
	}
	dubs, err := CheckDoubleSpends(m, allTxs)
	if err != nil {
		log.Printf("CheckDoubleSpends error: %s", err.Error())
		return
	}
	if len(dubs) > 0 {
		for i, dub := range dubs {
			fmt.Printf("dub %d known tx %s and new tx %s are exclusive!!!\n",
				i, dub.String(), m.TxSha().String())
		}
	}
	hits, err := s.TS.Ingest(m, height)
	if err != nil {
		log.Printf("Incoming Tx error: %s\n", err.Error())
		return
	}
	if hits == 0 && !s.HardMode {
		log.Printf("tx %s had no hits, filter false positive.",
			m.TxSha().String())
		s.fPositives <- 1 // add one false positive to chan
		return
	}
	log.Printf("tx %s ingested and matches %d utxo/adrs.",
		m.TxSha().String(), hits)
}

// GetDataHandler responds to requests for tx data, which happen after
// advertising our txs via an inv message
func (s *SPVCon) GetDataHandler(m *wire.MsgGetData) {
	log.Printf("got GetData.  Contains:\n")
	var sent int32
	for i, thing := range m.InvList {
		log.Printf("\t%d)%s : %s",
			i, thing.Type.String(), thing.Hash.String())

		// separate wittx and tx.  needed / combine?
		// does the same thing right now
		if thing.Type == wire.InvTypeWitnessTx {
			tx, err := s.TS.GetTx(&thing.Hash)
			if err != nil {
				log.Printf("error getting tx %s: %s",
					thing.Hash.String(), err.Error())
			}
			s.outMsgQueue <- tx
			sent++
			continue
		}
		if thing.Type == wire.InvTypeTx {
			tx, err := s.TS.GetTx(&thing.Hash)
			if err != nil {
				log.Printf("error getting tx %s: %s",
					thing.Hash.String(), err.Error())
			}
			s.outMsgQueue <- tx
			sent++
			continue
		}
		// didn't match, so it's not something we're responding to
		log.Printf("We only respond to tx requests, ignoring")

	}
	log.Printf("sent %d of %d requested items", sent, len(m.InvList))
}

func (s *SPVCon) InvHandler(m *wire.MsgInv) {
	log.Printf("got inv.  Contains:\n")
	for i, thing := range m.InvList {
		log.Printf("\t%d)%s : %s",
			i, thing.Type.String(), thing.Hash.String())
		if thing.Type == wire.InvTypeTx {
			if !s.Ironman { // ignore tx invs in ironman mode
				// new tx, OK it at 0 and request
				s.TS.AddTxid(&thing.Hash, 0) // unconfirmed
				s.AskForTx(thing.Hash)
			}
		}
		if thing.Type == wire.InvTypeBlock { // new block what to do?
			select {
			case <-s.inWaitState:
				// start getting headers
				fmt.Printf("asking for headers due to inv block\n")
				err := s.AskForHeaders()
				if err != nil {
					log.Printf("AskForHeaders error: %s", err.Error())
				}
			default:
				// drop it as if its component particles had high thermal energies
				fmt.Printf("inv block but ignoring; not synched\n")
			}
		}
	}
}
