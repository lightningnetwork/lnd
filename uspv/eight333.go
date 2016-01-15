package uspv

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil/bloom"
)

const (
	keyFileName    = "testseed.hex"
	headerFileName = "headers.bin"
	// Except hash-160s, those aren't backwards.  But anything that's 32 bytes is.
	// because, cmon, 32?  Gotta reverse that.  But 20?  20 is OK.
	NETVERSION = wire.TestNetL
	VERSION    = 70011
)

var (
	params = &chaincfg.TestNet3Params
)

type SPVCon struct {
	con        net.Conn // the (probably tcp) connection to the node
	headerFile *os.File // file for SPV headers

	localVersion  uint32 // version we report
	remoteVersion uint32 // version remote node
	remoteHeight  int32  // block height they're on
	netType       wire.BitcoinNet

	// what's the point of the input queue? remove? leave for now...
	inMsgQueue  chan wire.Message // Messages coming in from remote node
	outMsgQueue chan wire.Message // Messages going out to remote node

	WBytes uint64 // total bytes written
	RBytes uint64 // total bytes read
}

func (s *SPVCon) Open(remoteNode string, hfn string) error {
	// open header file
	err := s.openHeaderFile(headerFileName)
	if err != nil {
		return err
	}

	// open TCP connection
	s.con, err = net.Dial("tcp", remoteNode)
	if err != nil {
		return err
	}

	s.localVersion = VERSION
	s.netType = NETVERSION

	myMsgVer, err := wire.NewMsgVersionFromConn(s.con, 0, 0)
	if err != nil {
		return err
	}
	err = myMsgVer.AddUserAgent("test", "zero")
	if err != nil {
		return err
	}
	// must set this to enable SPV stuff
	myMsgVer.AddService(wire.SFNodeBloom)

	// this actually sends
	n, err := wire.WriteMessageN(s.con, myMsgVer, s.localVersion, s.netType)
	if err != nil {
		return err
	}
	s.WBytes += uint64(n)
	log.Printf("wrote %d byte version message to %s\n",
		n, s.con.RemoteAddr().String())

	n, m, b, err := wire.ReadMessageN(s.con, s.localVersion, s.netType)
	if err != nil {
		return err
	}
	s.RBytes += uint64(n)
	log.Printf("got %d byte response %x\n command: %s\n", n, b, m.Command())

	mv, ok := m.(*wire.MsgVersion)
	if ok {
		log.Printf("connected to %s", mv.UserAgent)
	}

	log.Printf("remote reports version %x (dec %d)\n",
		mv.ProtocolVersion, mv.ProtocolVersion)

	mva := wire.NewMsgVerAck()
	n, err = wire.WriteMessageN(s.con, mva, s.localVersion, s.netType)
	if err != nil {
		return err
	}
	s.WBytes += uint64(n)

	s.inMsgQueue = make(chan wire.Message)
	go s.incomingMessageHandler()
	s.outMsgQueue = make(chan wire.Message)
	go s.outgoingMessageHandler()

	return nil
}

func (s *SPVCon) openHeaderFile(hfn string) error {
	_, err := os.Stat(hfn)
	if err != nil {
		if os.IsNotExist(err) {
			var b bytes.Buffer
			err = params.GenesisBlock.Header.Serialize(&b)
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

func (s *SPVCon) PongBack(nonce uint64) {
	mpong := wire.NewMsgPong(nonce)

	s.outMsgQueue <- mpong
	return
}

func (s *SPVCon) SendFilter(f *bloom.Filter) {
	s.outMsgQueue <- f.MsgFilterLoad()
	return
}

func (s *SPVCon) GrabHeaders() error {
	var hdr wire.BlockHeader
	ghdr := wire.NewMsgGetHeaders()
	ghdr.ProtocolVersion = s.localVersion

	info, err := s.headerFile.Stat()
	if err != nil {
		return err
	}
	headerFileSize := info.Size()
	if headerFileSize == 0 || headerFileSize%80 != 0 { // header file broken
		return fmt.Errorf("Header file not a multiple of 80 bytes")
	}

	// seek to 80 bytes from end of file
	ns, err := s.headerFile.Seek(-80, os.SEEK_END)
	if err != nil {
		log.Printf("can't seek\n")
		return err
	}

	log.Printf("suk to offset %d (should be near the end\n", ns)
	// get header from last 80 bytes of file
	err = hdr.Deserialize(s.headerFile)
	if err != nil {
		log.Printf("can't Deserialize")
		return err
	}

	cHash := hdr.BlockSha()
	err = ghdr.AddBlockLocatorHash(&cHash)
	if err != nil {
		return err
	}

	fmt.Printf("get headers message has %d header hashes, first one is %s\n",
		len(ghdr.BlockLocatorHashes), ghdr.BlockLocatorHashes[0].String())

	s.outMsgQueue <- ghdr

	return nil
	// =============================================================

	// ask for headers.  probably will get 2000.
	log.Printf("getheader version %d \n", ghdr.ProtocolVersion)

	n, m, _, err := wire.ReadMessageN(s.con, VERSION, NETVERSION)
	if err != nil {
		return err
	}
	log.Printf("4got %d byte response\n command: %s\n", n, m.Command())
	hdrresponse, ok := m.(*wire.MsgHeaders)
	if !ok {
		log.Printf("got non-header message.")
		return nil
		// this can acutally happen and we should deal with / ignore it
		// also pings, they don't like it when you don't respond to pings.
		// invs and the rest we can ignore for now until filters are up.
	}

	_, err = s.headerFile.Seek(-80, os.SEEK_END)
	if err != nil {
		return err
	}
	var last wire.BlockHeader
	err = last.Deserialize(s.headerFile)
	if err != nil {
		return err
	}
	prevHash := last.BlockSha()

	gotNum := int64(len(hdrresponse.Headers))
	if gotNum > 0 {
		fmt.Printf("got %d headers. Range:\n%s - %s\n",
			gotNum, hdrresponse.Headers[0].BlockSha().String(),
			hdrresponse.Headers[len(hdrresponse.Headers)-1].BlockSha().String())
	}
	_, err = s.headerFile.Seek(0, os.SEEK_END)
	if err != nil {
		return err
	}
	for i, resphdr := range hdrresponse.Headers {
		// check first header returned to make sure it fits on the end
		// of our header file
		if i == 0 && !resphdr.PrevBlock.IsEqual(&prevHash) {
			return fmt.Errorf("header doesn't fit. points to %s, expect %s",
				resphdr.PrevBlock.String(), prevHash.String())
		}

		err = resphdr.Serialize(s.headerFile)
		if err != nil {
			return err
		}
	}

	endPos, _ := s.headerFile.Seek(0, os.SEEK_END)
	tip := endPos / 80

	go CheckRange(s.headerFile, tip-gotNum, tip-1, params)

	return nil
}

func sendMBReq(cn net.Conn, blkhash wire.ShaHash) error {
	iv1 := wire.NewInvVect(wire.InvTypeFilteredBlock, &blkhash)
	gdataB := wire.NewMsgGetData()
	_ = gdataB.AddInvVect(iv1)
	n, err := wire.WriteMessageN(cn, gdataB, VERSION, NETVERSION)
	if err != nil {
		return err
	}
	log.Printf("sent %d byte block request\n", n)
	return nil
}
