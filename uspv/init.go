package uspv

import (
	"bytes"
	"io/ioutil"
	"log"
	"net"
	"os"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
)

// OpenPV starts a
func OpenSPV(remoteNode string, hfn, dbfn string,
	inTs *TxStore, hard bool, iron bool, p *chaincfg.Params) (SPVCon, error) {
	// create new SPVCon
	var s SPVCon
	s.HardMode = hard
	s.Ironman = iron
	// I should really merge SPVCon and TxStore, they're basically the same
	inTs.Param = p
	s.TS = inTs // copy pointer of txstore into spvcon

	// open header file
	err := s.openHeaderFile(hfn)
	if err != nil {
		return s, err
	}
	// open TCP connection
	s.con, err = net.Dial("tcp", remoteNode)
	if err != nil {
		return s, err
	}
	// assign version bits for local node
	s.localVersion = VERSION
	// transaction store for this SPV connection
	err = inTs.OpenDB(dbfn)
	if err != nil {
		return s, err
	}
	myMsgVer, err := wire.NewMsgVersionFromConn(s.con, 0, 0)
	if err != nil {
		return s, err
	}
	err = myMsgVer.AddUserAgent("test", "zero")
	if err != nil {
		return s, err
	}
	// must set this to enable SPV stuff
	myMsgVer.AddService(wire.SFNodeBloom)
	// this actually sends
	n, err := wire.WriteMessageN(s.con, myMsgVer, s.localVersion, s.TS.Param.Net)
	if err != nil {
		return s, err
	}
	s.WBytes += uint64(n)
	log.Printf("wrote %d byte version message to %s\n",
		n, s.con.RemoteAddr().String())
	n, m, b, err := wire.ReadMessageN(s.con, s.localVersion, s.TS.Param.Net)
	if err != nil {
		return s, err
	}
	s.RBytes += uint64(n)
	log.Printf("got %d byte response %x\n command: %s\n", n, b, m.Command())

	mv, ok := m.(*wire.MsgVersion)
	if ok {
		log.Printf("connected to %s", mv.UserAgent)
	}
	log.Printf("remote reports version %x (dec %d)\n",
		mv.ProtocolVersion, mv.ProtocolVersion)

	// set remote height
	s.remoteHeight = mv.LastBlock
	mva := wire.NewMsgVerAck()
	n, err = wire.WriteMessageN(s.con, mva, s.localVersion, s.TS.Param.Net)
	if err != nil {
		return s, err
	}
	s.WBytes += uint64(n)

	s.inMsgQueue = make(chan wire.Message)
	go s.incomingMessageHandler()
	s.outMsgQueue = make(chan wire.Message)
	go s.outgoingMessageHandler()
	s.blockQueue = make(chan HashAndHeight, 32) // queue depth 32 is a thing
	s.fPositives = make(chan int32, 4000)       // a block full, approx
	s.inWaitState = make(chan bool, 1)
	go s.fPositiveHandler()

	return s, nil
}

func (s *SPVCon) openHeaderFile(hfn string) error {
	_, err := os.Stat(hfn)
	if err != nil {
		if os.IsNotExist(err) {
			var b bytes.Buffer
			err = s.TS.Param.GenesisBlock.Header.Serialize(&b)
			if err != nil {
				return err
			}
			err = ioutil.WriteFile(hfn, b.Bytes(), 0600)
			if err != nil {
				return err
			}
			log.Printf("created hardcoded genesis header at %s\n",
				hfn)
		}
	}
	s.headerFile, err = os.OpenFile(hfn, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	log.Printf("opened header file %s\n", s.headerFile.Name())
	return nil
}
