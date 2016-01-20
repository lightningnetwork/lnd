package uspv

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"

	"github.com/boltdb/bolt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil/bloom"
)

const (
	keyFileName    = "testseed.hex"
	headerFileName = "headers.bin"
	// Except hash-160s, those aren't backwards.  But anything that's 32 bytes is.
	// because, cmon, 32?  Gotta reverse that.  But 20?  20 is OK.

	// version hardcoded for now, probably ok...?
	VERSION = 70011
)

type SPVCon struct {
	con        net.Conn // the (probably tcp) connection to the node
	headerFile *os.File // file for SPV headers

	localVersion  uint32 // version we report
	remoteVersion uint32 // version remote node
	remoteHeight  int32  // block height they're on

	// what's the point of the input queue? remove? leave for now...
	inMsgQueue  chan wire.Message // Messages coming in from remote node
	outMsgQueue chan wire.Message // Messages going out to remote node

	WBytes uint64 // total bytes written
	RBytes uint64 // total bytes read

	TS    *TxStore         // transaction store to write to
	param *chaincfg.Params // network parameters (testnet3, testnetL)

	// mBlockQueue is for keeping track of what height we've requested.
	mBlockQueue chan RootAndHeight
}

func OpenSPV(remoteNode string, hfn string,
	inTs *TxStore, p *chaincfg.Params) (SPVCon, error) {
	// create new SPVCon
	var s SPVCon

	// assign network parameters to SPVCon
	s.param = p

	// open header file
	err := s.openHeaderFile(headerFileName)
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
	err = inTs.OpenDB("utxo.db")
	if err != nil {
		return s, err
	}
	s.TS = inTs

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
	n, err := wire.WriteMessageN(s.con, myMsgVer, s.localVersion, s.param.Net)
	if err != nil {
		return s, err
	}
	s.WBytes += uint64(n)
	log.Printf("wrote %d byte version message to %s\n",
		n, s.con.RemoteAddr().String())

	n, m, b, err := wire.ReadMessageN(s.con, s.localVersion, s.param.Net)
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
	n, err = wire.WriteMessageN(s.con, mva, s.localVersion, s.param.Net)
	if err != nil {
		return s, err
	}
	s.WBytes += uint64(n)

	s.inMsgQueue = make(chan wire.Message, 1)
	go s.incomingMessageHandler()
	s.outMsgQueue = make(chan wire.Message, 1)
	go s.outgoingMessageHandler()
	s.mBlockQueue = make(chan RootAndHeight, 32) // queue depth 32 is a thing

	return s, nil
}

func (ts *TxStore) OpenDB(filename string) error {
	var err error
	ts.StateDB, err = bolt.Open(filename, 0644, nil)
	if err != nil {
		return err
	}
	// create buckets if they're not already there
	return ts.StateDB.Update(func(tx *bolt.Tx) error {
		_, err = tx.CreateBucketIfNotExists(BKTUtxos)
		if err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(BKTOld)
		if err != nil {
			return err
		}
		return nil
	})
}

func (s *SPVCon) openHeaderFile(hfn string) error {
	_, err := os.Stat(hfn)
	if err != nil {
		if os.IsNotExist(err) {
			var b bytes.Buffer
			err = s.param.GenesisBlock.Header.Serialize(&b)
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

// HeightFromHeader gives you the block height given a 80 byte block header
// seems like looking for the merkle root is the best way to do this
func (s *SPVCon) HeightFromHeader(query wire.BlockHeader) (uint32, error) {
	// start from the most recent and work back in time; even though that's
	// kind of annoying it's probably a lot faster since things tend to have
	// happened recently.

	// seek to last header
	lastPos, err := s.headerFile.Seek(-80, os.SEEK_END)
	if err != nil {
		return 0, err
	}
	height := lastPos / 80

	var current wire.BlockHeader

	for height > 0 {
		// grab header from disk
		err = current.Deserialize(s.headerFile)
		if err != nil {
			return 0, err
		}
		// check if merkle roots match
		if current.MerkleRoot.IsEqual(&query.MerkleRoot) {
			// if they do, great, return height
			return uint32(height), nil
		}
		// skip back one header (2 because we just read one)
		_, err = s.headerFile.Seek(-160, os.SEEK_CUR)
		if err != nil {
			return 0, err
		}
		// decrement height
		height--
	}
	// finished for loop without finding match
	return 0, fmt.Errorf("Header not found on disk")
}

func (s *SPVCon) AskForHeaders() error {
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
}

func (s *SPVCon) IngestHeaders(m *wire.MsgHeaders) (bool, error) {
	var err error
	// seek to last header
	_, err = s.headerFile.Seek(-80, os.SEEK_END)
	if err != nil {
		return false, err
	}
	var last wire.BlockHeader
	err = last.Deserialize(s.headerFile)
	if err != nil {
		return false, err
	}
	prevHash := last.BlockSha()

	gotNum := int64(len(m.Headers))
	if gotNum > 0 {
		fmt.Printf("got %d headers. Range:\n%s - %s\n",
			gotNum, m.Headers[0].BlockSha().String(),
			m.Headers[len(m.Headers)-1].BlockSha().String())
	} else {
		log.Printf("got 0 headers, we're probably synced up")
		return false, nil
	}

	endPos, err := s.headerFile.Seek(0, os.SEEK_END)
	if err != nil {
		return false, err
	}

	// check first header returned to make sure it fits on the end
	// of our header file
	if !m.Headers[0].PrevBlock.IsEqual(&prevHash) {
		// delete 100 headers if this happens!  Dumb reorg.
		log.Printf("possible reorg; header msg doesn't fit. points to %s, expect %s",
			m.Headers[0].PrevBlock.String(), prevHash.String())
		if endPos < 8080 {
			// jeez I give up, back to genesis
			s.headerFile.Truncate(80)
		} else {
			err = s.headerFile.Truncate(endPos - 8000)
			if err != nil {
				return false, fmt.Errorf("couldn't truncate header file")
			}
		}
		return false, fmt.Errorf("Truncated header file to try again")
	}

	tip := endPos / 80
	tip-- // move back header length so it can read last header
	for _, resphdr := range m.Headers {
		// write to end of file
		err = resphdr.Serialize(s.headerFile)
		if err != nil {
			return false, err
		}

		// advance chain tip
		tip++
		// check last header
		worked := CheckHeader(s.headerFile, tip, s.param)
		if !worked {
			if endPos < 8080 {
				// jeez I give up, back to genesis
				s.headerFile.Truncate(80)
			} else {
				err = s.headerFile.Truncate(endPos - 8000)
				if err != nil {
					return false, fmt.Errorf("couldn't truncate header file")
				}
			}
			// probably should disconnect from spv node at this point,
			// since they're giving us invalid headers.
			return false, fmt.Errorf(
				"Header %d - %s doesn't fit, dropping 100 headers.",
				resphdr.BlockSha().String(), tip)
		}
	}
	log.Printf("Headers to height %d OK.", tip)
	return true, nil
}

// RootAndHeight is needed instead of just height in case a fullnode
// responds abnormally (?) by sending out of order merkleblocks.
// we cache a merkleroot:height pair in the queue so we don't have to
// look them up from the disk.
type RootAndHeight struct {
	root   wire.ShaHash
	height int32
}

// NewRootAndHeight saves like 2 lines.
func NewRootAndHeight(r wire.ShaHash, h int32) (rah RootAndHeight) {
	rah.root = r
	rah.height = h
	return
}

// AskForMerkBlocks requests blocks from current to last
// right now this asks for 1 block per getData message.
// Maybe it's faster to ask for many in a each message?
func (s *SPVCon) AskForMerkBlocks(current, last int32) error {
	var hdr wire.BlockHeader
	// if last is 0, that means go as far as we can
	if last == 0 {
		n, err := s.headerFile.Seek(-80, os.SEEK_END)
		if err != nil {
			return err
		}
		last = int32(n / 80)
	}

	_, err := s.headerFile.Seek(int64(current*80), os.SEEK_SET)
	if err != nil {
		return err
	}
	// loop through all heights where we want merkleblocks.
	for current < last {
		// load header from file
		err = hdr.Deserialize(s.headerFile)
		if err != nil {
			return err
		}

		bHash := hdr.BlockSha()
		// create inventory we're asking for
		iv1 := wire.NewInvVect(wire.InvTypeFilteredBlock, &bHash)
		gdataMsg := wire.NewMsgGetData()
		// add inventory
		err = gdataMsg.AddInvVect(iv1)
		if err != nil {
			return err
		}
		rah := NewRootAndHeight(hdr.MerkleRoot, current)
		s.outMsgQueue <- gdataMsg
		s.mBlockQueue <- rah // push height and mroot of requested block on queue
		current++
	}

	return nil
}
