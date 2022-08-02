package nmc_wallet

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
)

// Hex string returns
type Hex struct {
	Result struct {
		hex string
	}
}

type Hex2 struct {
	Result []struct {
		hex string
	}
}

// struct for list unspent
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

// input struct
type Input struct {
	txid string
	vout int
}

//output struct
type Output struct {
	data string
}

// return struct for sign transaction
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

// return struct for decoding transactions
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

// Namecoin test proof generator
func NmcGetProofTest(txid string, wallet string) Hex {
	testRequest := fmt.Sprintf(`{"jsonrpc": "2.0", "id":"", "method": "gettxoutproof", "params": [["%s"]]}`, txid)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://10.10.10.120:8332/wallet/%s", wallet), strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j Hex
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j
	}
	return Hex{}
}

// Namecoin test unspent transaction
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

// Namecoin test proof verifier
func NmcVerifyProofTest(proof string) Hex2 {
	testRequest := fmt.Sprintf(`{"jsonrpc": "2.0", "id":"", "method": "verifytxoutproof", "params": ["%s"]}`, proof)
	req, _ := http.NewRequest("POST", "http://10.10.10.120:8332", strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j Hex2
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j
	}
	return Hex2{}
}

// Namecoin test transaction methods
func NmcCreateRawTransactionTest(in Input, out Output, wal string) Hex {
	testRequest := fmt.Sprint(`{"jsonrpc": "2.0", "id":"", "method": "createrawtransaction", "params": [[{"txid":"`, in.txid, `","vout":`, in.vout, `}], {"data": "`, out.data, `"}]}`)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://10.10.10.120:8332/wallet/%s", wal), strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j Hex
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j
	}
	return Hex{}
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

func NmcSendRawTransactionTest(wal string, hex string) Hex {
	testRequest := fmt.Sprint(`{"jsonrpc": "2.0", "id":"", "method": "sendrawtransaction", "params": ["`, hex, `", 0]}`)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://10.10.10.120:8332/wallet/%s", wal), strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j Hex
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j
	}
	return Hex{}
}

func NmcGetRawTransactionTest(wal string, txid string) Hex {
	testRequest := fmt.Sprint(`{"jsonrpc": "2.0", "id":"", "method": "getrawtransaction", "params": ["`, txid, `"]}`)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://10.10.10.120:8332/wallet/%s", wal), strings.NewReader(testRequest))
	req.SetBasicAuth("bitcoinrpc", "rpc")
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
	} else {
		defer res.Body.Close()
		var j Hex
		body, _ := ioutil.ReadAll(res.Body)
		json.Unmarshal(body, &j)
		return j
	}
	return Hex{}
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
