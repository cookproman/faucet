package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dpapathanasiou/go-recaptcha"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/tendermint/tmlibs/bech32"
	"github.com/tomasen/realip"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keys/hd"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	bip39 "github.com/cosmos/go-bip39"


	"github.com/terra-project/core/app"
	core "github.com/terra-project/core/types"

	"github.com/tendermint/tendermint/crypto"
	"github.com/tendermint/tendermint/crypto/secp256k1"
)

var mnemonic string
var lcdUrl string
var chainID string
var privKey crypto.PrivKey
var address string
var sequence uint64
var accountNumber uint64
var cdc *codec.Codec

var amountTable = map[string]int64{
	core.MicroLunaDenom: 1000 * core.MicroUnit,
	core.MicroKRWDenom:  1000 * core.MicroUnit,
	core.MicroUSDDenom:  1000 * core.MicroUnit,
	core.MicroSDRDenom:  1000 * core.MicroUnit,
	core.MicroMNTDenom:  1000 * core.MicroUnit,
}

const (
	requestLimitSecs = 30
	mnemonicVar = "mnemonic"
)

// Claim wraps a faucet claim
type Claim struct {
    ChainID  string `json:"chain_id"`
    LcdUrl   string `json:"lcd_url"`
	Address  string `json:"address"`
	Response string `json:"response"`
	Denom    string `json:"denom"`
}

// Coin is the same as sdk.Coin
type Coin struct {
	Denom  string `json:"denom"`
	Amount int64  `json:"amount"`
}

// Env wraps env variables stored in env.json
type Env struct {
	Mnemonic string `json:"mnemonic"`
}

func newCodec() *codec.Codec {
	cdc := app.MakeCodec()

	config := sdk.GetConfig()
	config.SetCoinType(core.CoinType)
	config.SetFullFundraiserPath(core.FullFundraiserPath)
	config.SetBech32PrefixForAccount(core.Bech32PrefixAccAddr, core.Bech32PrefixAccPub)
	config.SetBech32PrefixForValidator(core.Bech32PrefixValAddr, core.Bech32PrefixValPub)
	config.SetBech32PrefixForConsensusNode(core.Bech32PrefixConsAddr, core.Bech32PrefixConsPub)
	config.Seal()

	return cdc
}

func readEnvFile() {
	data, err := ioutil.ReadFile("./env.json")
	if err != nil {
		fmt.Print(err)
	}

	var env Env
	err = json.Unmarshal(data, &env)
	if err != nil {
		fmt.Println("error:", err)
	}

	os.Setenv(mnemonicVar, env.Mnemonic)
}

func main() {
	db, err := leveldb.OpenFile("db/ipdb", nil)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	readEnvFile()
	mnemonic = os.Getenv(mnemonicVar)
	if mnemonic == "" {
		mnemonic = "faucet"
	}

	cdc = newCodec()

	seed := bip39.NewSeed(mnemonic, "")
	masterPriv, ch := hd.ComputeMastersFromSeed(seed)
	derivedPriv, err := hd.DerivePrivateKeyForPath(masterPriv, ch, core.FullFundraiserPath)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	privKey = secp256k1.PrivKeySecp256k1(derivedPriv)
	pubk := privKey.PubKey()
	address, err = bech32.ConvertAndEncode("terra", pubk.Address())
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	fmt.Println(address)

	if len(os.Args) != 2 {
		fmt.Printf("usage: %s <reCaptcha private key>\n", filepath.Base(os.Args[0]))
		os.Exit(1)
	} else {
		recaptcha.Init(os.Args[1])

		http.Handle("/", http.FileServer(http.Dir("./frontend/build/")))
		http.HandleFunc("/claim", createGetCoinsHandler(db))

		if err := http.ListenAndServe(":3000", nil); err != nil {
			log.Fatal("failed to start server", err)
		}
	}
}

func loadAccountInfo() {
	// Query current faucet sequence
	url := fmt.Sprintf("%v/auth/accounts/%v", lcdUrl, address)
	response, err := http.Get(url)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	sequence, _ = strconv.ParseUint(parseRegexp(`"sequence":"(\d+)"`, string(body)), 10, 64)
	accountNumber, _ = strconv.ParseUint(parseRegexp(`"account_number":"(\d+)"`, string(body)), 10, 64)
	return
}

func parseRegexp(regexpStr string, target string) (data string) {
	// Capture seqeunce string from json
	r := regexp.MustCompile(regexpStr)
	groups := r.FindStringSubmatch(string(target))

	if len(groups) != 2 {
		fmt.Printf("cannot find data")
		os.Exit(1)
	}

	// Convert sequence string to int64
	data = groups[1]
	return
}

// RequestLog stores the Log of a Request
type RequestLog struct {
	Coins     []Coin    `json:"coin"`
	Requested time.Time `json:"updated"`
}

func (requestLog *RequestLog) dripCoin(denom string) error {
	amount := amountTable[denom]

	// try to update coin
	for idx, coin := range requestLog.Coins {
		if coin.Denom == denom {
			if (coin.Amount + amount) > amountTable[denom]*10 {
				return errors.New("amount limit exceeded")
			}

			requestLog.Coins[idx].Amount += amount
			return nil
		}
	}

	// first drip for denom
	requestLog.Coins = append(requestLog.Coins, Coin{Denom: denom, Amount: amount})
	return nil
}

func checkAndUpdateLimit(db *leveldb.DB, account []byte, denom string) error {
	var requestLog RequestLog

	logBytes, _ := db.Get(account, nil)
	now := time.Now()

	if logBytes != nil {
		jsonErr := json.Unmarshal(logBytes, &requestLog)
		if jsonErr != nil {
			return jsonErr
		}

		// check interval limt
		intervalSecs := now.Sub(requestLog.Requested).Seconds()
		if intervalSecs < requestLimitSecs {
			return errors.New("please wait a while for another tap")
		}

		// reset log if date was changed
		if requestLog.Requested.Day() != now.Day() {
			requestLog.Coins = []Coin{}
		}

		// check amount limit
		dripErr := requestLog.dripCoin(denom)
		if dripErr != nil {
			return dripErr
		}
	}

	// update requested time
	requestLog.Requested = now
	logBytes, _ = json.Marshal(requestLog)
	updateErr := db.Put(account, logBytes, nil)
	if updateErr != nil {
		return updateErr
	}

	return nil
}

func createGetCoinsHandler(db *leveldb.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {

		defer func() {
			if err := recover(); err != nil {
				http.Error(w, err.(error).Error(), 400)
			}
		}()

		var claim Claim
		// decode JSON response from front end
		decoder := json.NewDecoder(request.Body)
		decoderErr := decoder.Decode(&claim)
		if decoderErr != nil {
			panic(decoderErr)
		}

		chainID = claim.ChainID
        lcdUrl = claim.LcdUrl

		loadAccountInfo()

		amount, ok := amountTable[claim.Denom]
		if !ok {
			panic(fmt.Errorf("Invalid Denom; %v", claim.Denom))
		}

		// make sure address is bech32
		readableAddress, decodedAddress, decodeErr := bech32.DecodeAndConvert(claim.Address)
		if decodeErr != nil {
			panic(decodeErr)
		}
		// re-encode the address in bech32
		encodedAddress, encodeErr := bech32.ConvertAndEncode(readableAddress, decodedAddress)
		if encodeErr != nil {
			panic(encodeErr)
		}

		// make sure captcha is valid
		clientIP := realip.FromRequest(request)
		captchaResponse := claim.Response
		captchaPassed, captchaErr := recaptcha.Confirm(clientIP, captchaResponse)
		if captchaErr != nil {
			panic(captchaErr)
		}

		// Limiting request speed
		limitErr := checkAndUpdateLimit(db, decodedAddress, claim.Denom)
		if limitErr != nil {
			panic(limitErr)
		}

		// send the coins!
		if captchaPassed {
			url := fmt.Sprintf("%v/bank/accounts/%v/transfers", lcdUrl, encodedAddress)
			data := strings.TrimSpace(fmt.Sprintf(`{
				"base_req": {
					"from": "%v",
					"memo": "%v",
					"chain_id": "%v",
					"sequence": "%v",
					"gas": "auto",
					"gas_adjustment": "1.4",
					"gas_prices": [
						{
							"denom": "ukrw",
							"amount": "0.015"
						}
					]
				},
				"coins": [
					{
						"denom": "%v",
						"amount": "%v"
					}
				]

			}`, address, "faucet", chainID, sequence, claim.Denom, amount))

			response, err := http.Post(url, "application/json", bytes.NewReader([]byte(data)))
			if err != nil {
				panic(err)
			}

			if response.StatusCode != 200 {
				err := errors.New(response.Status)
				panic(err)
			}

			defer response.Body.Close()

			body, err := ioutil.ReadAll(response.Body)
			if err != nil {
				panic(err)
			}

			resJSON := signAndBroadcast(body)

			fmt.Println(time.Now().UTC().Format(time.RFC3339), encodedAddress, "[1] ", amount, claim.Denom)
			fmt.Println(resJSON)

			sequence = sequence + 1

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"amount": %v, "response": %v}`, amount, resJSON)
		} else {
			err := errors.New("captcha failed, please refresh page and try again")
			panic(err)
		}

		return
	}
}

// BroadcastReq defines a tx broadcasting request.
type BroadcastReq struct {
	Tx   auth.StdTx `json:"tx"`
	Mode string     `json:"mode"`
}

func signAndBroadcast(txJSON []byte) string {
	var broadcastReq BroadcastReq
	var stdTx auth.StdTx

	cdc.MustUnmarshalJSON(txJSON, &stdTx)

	// Sort denom
	for _, msg := range stdTx.Msgs {
		msg, ok := msg.(bank.MsgSend)
		if ok {
			msg.Amount.Sort()
		}
	}

	signBytes := auth.StdSignBytes(chainID, accountNumber, sequence, stdTx.Fee, stdTx.Msgs, stdTx.Memo)
	sig, err := privKey.Sign(signBytes)
	if err != nil {
		panic(err)
	}

	sigs := []auth.StdSignature{{
		PubKey:    privKey.PubKey(),
		Signature: sig}}
	tx := auth.NewStdTx(stdTx.Msgs, stdTx.Fee, sigs, stdTx.Memo)
	broadcastReq.Tx = tx
	broadcastReq.Mode = "block"

	bz := cdc.MustMarshalJSON(broadcastReq)

	url := fmt.Sprintf("%v/txs", lcdUrl)
	response, err := http.Post(url, "application/json", bytes.NewReader(bz))
	if err != nil {
		panic(err)
	}

	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		panic(err)
	}

	if response.StatusCode != 200 {
		err := fmt.Errorf("status: %v, message: %v", response.Status, string(body))
		panic(err)
	}

	return string(body)
}
