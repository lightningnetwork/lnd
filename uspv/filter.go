package uspv

import (
	"log"
	"net"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil/bloom"
)

//func (w *Eight3Con) SendFilter() error {

//	filter := bloom.NewFilter(10, 0, 0.001, wire.BloomUpdateAll)

//	// add addresses.
//	filter.Add(adrBytes1)
//	filter.Add(adrBytes2)
//	filter.Add(adrBytes3)
//	//	filter.Add(adrBytes4)
//	fmt.Printf("hf: %d filter %d bytes: %x\n",
//		filter.MsgFilterLoad().HashFuncs,
//		len(filter.MsgFilterLoad().Filter), filter.MsgFilterLoad().Filter)

//	n, err := wire.WriteMessageN(cn,
//		filter.MsgFilterLoad(), myversion, whichnet)
//	if err != nil {
//		return err
//	}
//	log.Printf("sent %d byte filter message\n", n)

//	return nil
//}

func sendFilter(cn net.Conn) error {
	//	adrBytes1, _ := hex.DecodeString(adrHex1)
	//	adrBytes2, _ := hex.DecodeString(adrHex2)
	//	adrBytes3, _ := hex.DecodeString(adrHex3)
	// ------------------- load a filter
	// make a new filter.  floats ew.  hardcode.

	filter := bloom.NewFilter(10, 0, 0.001, wire.BloomUpdateNone)

	// add addresses.
	//	filter.Add(adrBytes1)
	//	filter.Add(adrBytes2)
	//	filter.Add(adrBytes3)
	//	filter.Add(adrBytes4)

	n, err := wire.WriteMessageN(cn,
		filter.MsgFilterLoad(), VERSION, NETVERSION)
	if err != nil {
		return err
	}
	log.Printf("sent %d byte filter message\n", n)
	return nil
}
