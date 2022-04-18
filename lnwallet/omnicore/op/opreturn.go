package op

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/omnicore"
	"github.com/lightningnetwork/lnd/lnwire"
	"log"
)

// maximum 7 receivers
var max_receivers int = 7

type Opreturn struct {
	Ver        uint16
	TranType   uint16
	Outputs    []*Output
	PropertyId uint32
}
type Output struct {
	Index  uint8
	Amount uint64
}

// https://github.com/OmniLayer/omnicore/pull/1252
// example 1:
// omni_createpayload_sendtomany 31
// 				'[
//					{"output": 2, "amount": "10.5"},
//					{"output": 3, "amount": "0.5"},
//				 	{"output": 5, "amount": "15.0"}
//				 ]'
// expected export: 000000070000001f0302000000003e95ba80030000000002faf080050000000059682f00
//
// Payload format is: (https://gist.github.com/dexX7/1138fd1ea084a9db56798e9bce50d0ef)
//	size	|	Field					|		Type			|	Value
// 2 bytes	|	Transaction version		|	Transaction version	|	0
// 2 bytes	|	Transaction type		|	Transaction type	|	7 (= Send-to-Many)
// 4 bytes	|	Token identifier to send|	Token identifier	|	31 (= USDT )
// 1 byte 	|	Number of outputs		|	Integer-one byte	|	3
// 1 byte 	|	Receiver output #		|	Integer-one byte	|	1 (= vout 1)
// 8 bytes	|	Amount to send			|	Number of tokens	|	20 0000 0000 (= 20.0)
// 1 byte 	|	Receiver output #		|	Integer-one byte	|	2 (= vout 2)
// 8 bytes	|	Amount to send			|	Number of tokens	|	15 0000 0000 (= 15.0)
// 1 byte 	|	Receiver output #		|	Integer-one byte	|	4 (= vout 4)
// 8 bytes	|	Amount to send			|	Number of tokens	|	30 0000 0000 (= 30.0)
//
// This format may changes according to the update of omnicore.
//
// cpp code in omnicore:  src/omnicore/createpayload.cpp
// CreatePayload_SendToMany(uint32_t propertyId, std::vector<std::tuple<uint8_t, uint64_t>> outputValues)
//
func (p *Opreturn) Encode(w *bytes.Buffer) error {
	if p.PropertyId==0{
		return fmt.Errorf("miss assetId")
	}
	//maximum 7 receivers
	if len(p.Outputs) > max_receivers {
		return fmt.Errorf("The maximum receiver is 7. Too many receivers, so quit.....")
	}

	//ver
	if err := lnwire.WriteUint16(w, 0); err != nil { //ver
		return err
	}
	//type
	if err := lnwire.WriteUint16(w, 7); err != nil { //type
		return err
	}
	if err := lnwire.WriteUint32(w, p.PropertyId); err != nil {
		return err
	}
	//outputs_count
	if err := lnwire.WriteUint8(w, byte(len(p.Outputs))); err != nil { //type
		return err
	}

	for _, out := range p.Outputs {
		if err := lnwire.WriteUint8(w, byte(out.Index)); err != nil { //type
			return err
		}
		if err := lnwire.WriteUint64(w, out.Amount); err != nil {
			return err
		}
	}
	return nil
}
func (o *Opreturn) Decode(bs []byte) error {
	reader := bytes.NewReader(bs)
	outputs_count := uint8(0)
	if err := lnwire.ReadElements(reader, &o.Ver, &o.TranType, &o.PropertyId, &outputs_count); err != nil {
		return err
	}
	for i := 0; i < int(outputs_count); i++ {
		out := new(Output)
		if err := lnwire.ReadElements(reader, &out.Index, &out.Amount); err != nil {
			return err
		}
		o.Outputs = append(o.Outputs, out)
	}
	return nil
}
func PrintWithDecode(opreutrn_bs []byte)  {
	o:=new(Opreturn)
	if err:=o.Decode(opreutrn_bs);err!=nil{
		log.Println(err)
	}
	jsonOut, _ := json.MarshalIndent(o, "", "  ")
	log.Println("opreutrn payload:")
	log.Println(string(jsonOut))
}

func GetOpReturnOut(assetID uint32,outs []*Output) (*wire.TxOut, error) {
	op := &Opreturn{Outputs: outs,
		PropertyId: uint32(assetID),
	}
	w := bytes.NewBuffer([]byte{})
	if err := op.Encode(w); err != nil {
		return nil, err
	}
	return &wire.TxOut{Value: 0, PkScript: w.Bytes()}, nil
}


type pksAmount struct{
	//pkScirpt
	pks []byte
	amount omnicore.Amount
}
//collection pkScirpt and assetAmount
type PksAmounts struct{
	assetID uint32
	pksAmounts []*pksAmount
}
func NewPksAmounts(assetId uint32) *PksAmounts{
	return &PksAmounts{assetID: assetId}
}
func (p *PksAmounts)Len()int {
	return len(p.pksAmounts)
}
func (p *PksAmounts)Add(pks []byte, amount omnicore.Amount){
	if p.assetID==0{
		panic(fmt.Errorf("miss assetId"))
	}
	p.pksAmounts=append(p.pksAmounts,&pksAmount{pks,amount})
}
func AddOpReturnToTx(tx *wire.MsgTx ,p *PksAmounts  )error{
	if p.assetID==0{
		return fmt.Errorf("miss assetId")
	}
	opOutPuts:=[]*Output{}
	if len(p.pksAmounts)==0{
		return nil
	}
	for _, pa := range p.pksAmounts {
		pkScirpt := pa.pks
		found, assetOutIndex := input.FindScriptOutputIndex(
			tx, pkScirpt,
		)
		if !found {
			return fmt.Errorf("FindScriptOutputIndex not found: %x %v", pa.pks, pa.amount)
		}
		opOutPuts=append(opOutPuts,&Output{uint8(assetOutIndex)+1,uint64(pa.amount)})
	}
	txOUt, err := GetOpReturnOut(p.assetID, opOutPuts)
	//PrintWithDecode(txOUt.PkScript)
	if err != nil {
		return  err
	}
	outs := tx.TxOut
	tx.TxOut = append([]*wire.TxOut{txOUt}, outs...)
	return nil
}

// Address can be empty "" when constructs payload
//type PayloadOutput struct {
//	Output  byte   `json:"output"`
//	Address string `json:"address"`
//	Amount  string `json:"amount"`
//}

//
//func OmniCreatePayloadSendToMany(property_id uint32, receivers_array []PayloadOutput, divisible bool) ([]byte, string) {
//	//property_id, _ := ParsePropertyId(property_id_str)
//	// Type 7 is sendToMany transaction type.
//	var messageType uint16 = 7
//	var messageVer uint16 = 0
//	messageType = SwapByteOrder16(messageType)
//	messageVer = SwapByteOrder16(messageVer)
//	property_id = SwapByteOrder32(property_id)
//
//	var outputs_count byte = 0
//	var total_value_out int64 = 0
//
//	/*
//		receivers := make([][]byte, 0)
//		decoder := json.NewDecoder(strings.NewReader(output_list))
//		for {
//			var each_receiver PayloadOutput
//			if err := decoder.Decode(&each_receiver); err == io.EOF {
//				break
//			} else if err != nil {
//				log.Fatal(err)
//			}
//			outputs_count += 1
//			if outputs_count > max_receivers {
//				fmt.Println("The maximum receiver is 7. Too many receivers, so quit.....")
//				return nil, ""
//			}
//			amount := StrToInt64(each_receiver.Amount, divisible)
//			amount_uint64 := uint64(amount)
//			size := 2
//			one_receiver := make([][]byte, size)
//			one_receiver[0] = OneByte(each_receiver.Output)
//			one_receiver[1] = Uint64ToBytes(SwapByteOrder64(amount_uint64))
//			seperator := []byte("")
//			one_receiver_in_byts := bytes.Join(one_receiver, seperator)
//			receivers = append(receivers, one_receiver_in_byts)
//			total_value_out = total_value_out + AmountFromValue(each_receiver.Amount)
//		}
//	*/
//	receivers := make([][]byte, 0)
//
//	for j := 0; j < len(receivers_array); j++ {
//		outputs_count += 1
//		amount := StrToInt64(receivers_array[j].Amount, divisible)
//		amount_uint64 := uint64(amount)
//		size := 2
//		one_receiver := make([][]byte, size)
//		one_receiver[0] = OneByte(receivers_array[j].Output)
//		one_receiver[1] = Uint64ToBytes(SwapByteOrder64(amount_uint64))
//
//		seperator := []byte("")
//		one_receiver_in_byts := bytes.Join(one_receiver, seperator)
//
//		receivers = append(receivers, one_receiver_in_byts)
//		total_value_out = total_value_out + AmountFromValue(receivers_array[j].Amount)
//	}
//
//	len := 4 + outputs_count
//	s := make([][]byte, len)
//
//	s[0] = Uint16ToBytes(messageVer)
//	s[1] = Uint16ToBytes(messageType)
//	s[2] = Uint32ToBytes(property_id)
//	s[3] = OneByte(outputs_count)
//
//	for i := byte(4); i < len; i++ {
//		s[i] = receivers[i-4]
//	}
//
//	sep := []byte("")
//
//	payload := bytes.Join(s, sep)
//	return payload, HexStr(payload)
//}
