package uspv

import (
	"fmt"
	"log"

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
			log.Printf("Got a ping message.  We should pong back or they will kick us off.")
			s.PongBack(m.Nonce)
		case *wire.MsgPong:
			log.Printf("Got a pong response. OK.\n")
		case *wire.MsgMerkleBlock:

			//			log.Printf("Got merkle block message. Will verify.\n")
			//			fmt.Printf("%d flag bytes, %d txs, %d hashes",
			//				len(m.Flags), m.Transactions, len(m.Hashes))
			txids, err := checkMBlock(m)
			if err != nil {
				log.Printf("Merkle block error: %s\n", err.Error())
				return
				//				continue
			}
			fmt.Printf(" got %d txs ", len(txids))
			//			fmt.Printf(" = got %d txs from block %s\n",
			//				len(txids), m.Header.BlockSha().String())
			for _, txid := range txids {
				err := s.TS.AddTxid(txid)
				if err != nil {
					log.Printf("Txid store error: %s\n", err.Error())
				}
			}
			//			nextReq <- true

		case *wire.MsgHeaders:
			moar, err := s.IngestHeaders(m)
			if err != nil {
				log.Printf("Header error: %s\n", err.Error())
				return
			}
			if moar {
				s.AskForHeaders()
			}
		case *wire.MsgTx:
			err := s.TS.IngestTx(m)
			if err != nil {
				log.Printf("Incoming Tx error: %s\n", err.Error())
			}
			//			log.Printf("Got tx %s\n", m.TxSha().String())
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
