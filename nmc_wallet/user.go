package nmc_wallet

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
)

type Res struct {
	Result string
}
type Res2 struct {
	Result []string
}
type ListUnspent struct {
	Result []struct {
		Txid          string  `json:"txid"`
		Vout          int     `json:"vout"`
		Generated     bool    `json:"generated"`
		Address       string  `json:"address"`
		ScriptPubKey  string  `json:"scriptPubKey"`
		Amount        float64 `json:"amount"`
		Interest      float64 `json:"interest"`
		Confirmations int     `json:"confirmations"`
		Spendable     bool    `json:"spendable"`
	} `json:"result"`
	Error error  `json:"error"`
	ID    string `json:"id"`
}

type Input struct {
	txid string
	vout int
}
type Output struct {
	data string
}

type SignRawTransaction struct {
	Result struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
		Errors   []struct {
			Txid      string `json:"txid"`
			Vout      int    `json:"vout"`
			ScriptSig string `json:"scriptSig"`
			Sequence  int64  `json:"sequence"`
			Error     string `json:"error"`
		} `json:"errors"`
	} `json:"result"`
	Error error  `json:"error"`
	ID    string `json:"id"`
}

type DecodeRawTransaction struct {
	Result struct {
		Txid     string `json:"txid"`
		Size     int    `json:"size"`
		Version  int    `json:"version"`
		Locktime int    `json:"locktime"`
		Vin      []struct {
			Txid      string `json:"txid"`
			Vout      int    `json:"vout"`
			ScriptSig struct {
				Asm string `json:"asm"`
				Hex string `json:"hex"`
			} `json:"scriptSig"`
			Sequence int64 `json:"sequence"`
		} `json:"vin"`
		Vout []struct {
			Value        float64 `json:"value"`
			ValueSat     int     `json:"valueSat"`
			N            int     `json:"n"`
			ScriptPubKey struct {
				Asm       string   `json:"asm"`
				Hex       string   `json:"hex"`
				ReqSigs   int      `json:"reqSigs"`
				Type      string   `json:"type"`
				Addresses []string `json:"addresses"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
		Vjoinsplit []interface{} `json:"vjoinsplit"`
	} `json:"result"`
	Error error  `json:"error"`
	ID    string `json:"id"`
}

// func main() {
// 	tx := "fcc512ce4af6092e3a812425635bf68e3d97403fe11461acad466e673a37f203"
// 	wal := "brandon"
// 	proof := NmcGetProofTest(tx, wal)
// 	fmt.Println(proof)
// 	fmt.Println(NmcVerifyProofTest(proof))
// 	list := NmcUnlistTest(wal)
// 	in := Input{list.Result[0].Txid, 0}
// 	out := Output{"68656C6C6F20776F726C64"}
// 	createtx := NmcCreateRawTransactionTest(in, out, wal)
// 	signtx := NmcSignRawTransactionTest(createtx, wal)
// 	fmt.Println(signtx)
// 	sendtx := NmcSendRawTransactionTest(wal, signtx.Result.Hex)
// 	fmt.Println(sendtx)
// 	gettx := NmcGetRawTransactionTest(wal, list.Result[0].Txid)
// 	fmt.Println(gettx)
// 	decodetx := NmcDecodeRawTransactionTest(gettx)
// 	fmt.Println(decodetx)
// 	fmt.Println(decodetx.Result.Vout[0].ScriptPubKey.Asm)
// }

func NmcGetProofTest(txid string, wallet string) string {
	testRequest := fmt.Sprintf(`{"jsonrpc": "2.0", "id":"", "method": "gettxoutproof", "params": [["%s"]]}`, txid)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://10.10.10.120:8332/wallet/%s", wallet), strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j Res
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j.Result
	}
	return ""
}

func NmcUnlistTest(wallet string) ListUnspent {
	testRequest := `{"jsonrpc": "2.0", "id":"", "method": "listunspent", "params": []}`
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://10.10.10.120:8332/wallet/%s", wallet), strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j ListUnspent
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j
	}
	return ListUnspent{}
}

func NmcVerifyProofTest(proof string) []string {
	testRequest := fmt.Sprintf(`{"jsonrpc": "2.0", "id":"", "method": "verifytxoutproof", "params": ["%s"]}`, proof)
	req, _ := http.NewRequest("POST", "http://10.10.10.120:8332", strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j Res2
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j.Result
	}
	a := []string{""}
	return a
}

func NmcCreateRawTransactionTest(in Input, out Output, wal string) string {
	testRequest := fmt.Sprint(`{"jsonrpc": "2.0", "id":"", "method": "createrawtransaction", "params": [[{"txid":"`, in.txid, `","vout":`, in.vout, `}], {"data": "`, out.data, `"}]}`)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://10.10.10.120:8332/wallet/%s", wal), strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j Res
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j.Result
	}
	return ""
}

func NmcSignRawTransactionTest(hex string, wal string) SignRawTransaction {
	testRequest := fmt.Sprint(`{"jsonrpc": "2.0", "id":"", "method": "signrawtransactionwithwallet", "params": ["`, hex, `"]}`)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://10.10.10.120:8332/wallet/%s", wal), strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)

	} else {
		defer res.Body.Close()
		var j SignRawTransaction
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		fmt.Println(j.Result)
		return j
	}
	return SignRawTransaction{}
}

func NmcSendRawTransactionTest(wal string, hex string) string {
	testRequest := fmt.Sprint(`{"jsonrpc": "2.0", "id":"", "method": "sendrawtransaction", "params": ["`, hex, `", 0]}`)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://10.10.10.120:8332/wallet/%s", wal), strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j Res
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j.Result
	}
	return ""
}

func NmcGetRawTransactionTest(wal string, txid string) string {
	testRequest := fmt.Sprint(`{"jsonrpc": "2.0", "id":"", "method": "getrawtransaction", "params": ["`, txid, `"]}`)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://10.10.10.120:8332/wallet/%s", wal), strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j Res
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j.Result
	}
	return ""
}

func NmcDecodeRawTransactionTest(hex string) DecodeRawTransaction {
	testRequest := fmt.Sprint(`{"jsonrpc": "2.0", "id":"", "method": "decoderawtransaction", "params": ["`, hex, `"]}`)
	req, _ := http.NewRequest("POST", "http://10.10.10.120:8332", strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j DecodeRawTransaction
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j
	}
	return DecodeRawTransaction{}
}
