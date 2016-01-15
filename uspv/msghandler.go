package uspv

import (
	"fmt"
	"log"

	"github.com/btcsuite/btcd/wire"
)

func (e3c *SPVCon) incomingMessageHandler() {
	for {
		n, xm, _, err := wire.ReadMessageN(e3c.con, e3c.localVersion, e3c.netType)
		if err != nil {
			log.Printf("ReadMessageN error.  Disconnecting: %s\n", err.Error())
			return
		}
		e3c.RBytes += uint64(n)
		//		log.Printf("Got %d byte %s message\n", n, xm.Command())
		switch m := xm.(type) {
		case *wire.MsgVersion:
			log.Printf("Got version message.  Agent %s, version %d, at height %d\n",
				m.UserAgent, m.ProtocolVersion, m.LastBlock)
			e3c.remoteVersion = uint32(m.ProtocolVersion) // weird cast! bug?
		case *wire.MsgVerAck:
			log.Printf("Got verack.  Whatever.\n")
		case *wire.MsgAddr:
			log.Printf("got %d addresses.\n", len(m.AddrList))
		case *wire.MsgPing:
			log.Printf("Got a ping message.  We should pong back or they will kick us off.")
			e3c.PongBack(m.Nonce)
		case *wire.MsgPong:
			log.Printf("Got a pong response. OK.\n")
		case *wire.MsgMerkleBlock:
			log.Printf("Got merkle block message. Will verify.\n")
			fmt.Printf("%d flag bytes, %d txs, %d hashes",
				len(m.Flags), m.Transactions, len(m.Hashes))
			txids, err := checkMBlock(m)
			if err != nil {
				log.Printf("Merkle block error: %s\n", err.Error())
				return
				//				continue
			}
			fmt.Printf(" = got %d txs from block %s\n",
				len(txids), m.Header.BlockSha().String())
			//			nextReq <- true
		case *wire.MsgTx:

			log.Printf("Got tx %s\n", m.TxSha().String())
		default:
			log.Printf("Got unknown message type %s\n", m.Command())
		}
	}
	return
}

// this one seems kindof pointless?  could get ridf of it and let
// functions call WriteMessageN themselves...
func (e3c *SPVCon) outgoingMessageHandler() {
	for {
		msg := <-e3c.outMsgQueue
		n, err := wire.WriteMessageN(e3c.con, msg, e3c.localVersion, e3c.netType)
		if err != nil {
			log.Printf("Write message error: %s", err.Error())
		}
		e3c.WBytes += uint64(n)
	}
	return
}
