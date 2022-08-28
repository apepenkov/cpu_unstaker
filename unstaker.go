package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/eoscanada/eos-go"
	"github.com/eoscanada/eos-go/ecc"
)

var config = &botConfiguration{}
var api *eos.API

type botConfiguration struct {
	Config struct {
		Pkey         string  `toml:"pkey"`
		Account      string  `toml:"account"`
		WaxNode      string  `toml:"wax_node"`
		ChunkSize    int     `toml:"chunk_size"`
		NetUnstakeTo float64 `toml:"net_unstake_to"`
		CpuUnstakeTo float64 `toml:"cpu_unstake_to"`
	} `toml:"config"`
}

func (b *botConfiguration) NetUnstakeToInt64() int64 {
	return int64(b.Config.NetUnstakeTo * 100_000_000.0)
}

func (b *botConfiguration) CpuUnstakeToInt64() int64 {
	return int64(b.Config.CpuUnstakeTo * 100_000_000.0)
}

func loadCfg() {
	if _, err := toml.DecodeFile("config.toml", config); err != nil {
		fmt.Println(fmt.Errorf("loading config.toml: %w", err))
		os.Exit(1)
	}
}

func remove(s []string, e string) []string {
	for i, a := range s {
		if a == e {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

func wrapAct(account eos.AccountName, name eos.ActionName, caller eos.AccountName) *eos.Action {
	authorization := make([]eos.PermissionLevel, 0)
	authorization = append(
		authorization, eos.PermissionLevel{
			Actor:      caller,
			Permission: "cpustake",
		},
	)
	return &eos.Action{
		Account:       account,
		Name:          name,
		Authorization: authorization,
	}
}

func FullActionUnDelegateBW(from eos.AccountName, Receiver eos.AccountName, usntakeNetQuantity eos.Asset, unstakeCpuQantity eos.Asset) *eos.Action {
	act := wrapAct(eos.AN("eosio"), eos.ActN("delegatebw"), from)
	act.ActionData = eos.NewActionData(
		struct {
			From     eos.AccountName `json:"from"`
			Receiver eos.AccountName `json:"reciever"`
			Net      eos.Asset       `json:"unstake_net_quantity"`
			Cpu      eos.Asset       `json:"unstake_cpu_quantity"`
		}{
			From:     from,
			Receiver: Receiver,
			Net:      usntakeNetQuantity,
			Cpu:      unstakeCpuQantity,
		},
	)
	return act
}

func MakeAndSignTransaction(actions []*eos.Action, keys []string) *eos.PackedTransaction {
	var txOpts *eos.TxOptions
	for {
		txOpts = &eos.TxOptions{}
		if err := txOpts.FillFromChain(context.Background(), api); err != nil {
			fmt.Println(fmt.Errorf("filling tx opts: %w", err))
			time.Sleep(time.Millisecond * 5)
			continue
		}
		break
	}
	tx := eos.NewTransaction(actions, txOpts)
	tx.SetExpiration(time.Minute * 55)
	stx := eos.NewSignedTransaction(tx)
	packed, err := stx.Pack(eos.CompressionNone)
	if err != nil {
		panic(err)
	}
	for _, key := range keys {
		pkey, _ := ecc.NewPrivateKey(key)
		txdata, cfd, _ := stx.PackedTransactionAndCFD()
		sigDigest := eos.SigDigest(txOpts.ChainID, txdata, cfd)
		sig, _ := pkey.Sign(sigDigest)
		packed.Signatures = append(packed.Signatures, sig)
	}
	return packed
}

type StakedAccount struct {
	Account   eos.AccountName
	CpuWeight eos.Asset
	NetWeight eos.Asset
}

func main() {
	loadCfg()
	api = eos.New(config.Config.WaxNode)
	// load accounts from accounts.txt
	accountsLoaded := make([]string, 0)
	file, err := os.Open("accounts.txt")
	if err != nil {
		fmt.Println(fmt.Errorf("opening accounts.txt: %w", err))
		os.Exit(1)
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if len(scanner.Text()) > 2 {
			accountsLoaded = append(accountsLoaded, scanner.Text())
		}
	}
	_ = file.Close()
	fmt.Println("Checking stakes from this account and comparing with provided accounts")
	accounts := make([]StakedAccount, 0)

	bodyJson := map[string]interface{}{
		"json":           true,
		"code":           "eosio",
		"scope":          config.Config.Account,
		"table":          "delband",
		"lower_bound":    "0",
		"upper_bound":    "",
		"index_position": 1,
		"key_type":       "",
		"limit":          "10000",
		"reverse":        false,
		"show_payer":     false,
		"index":          1,
	}
	for {
		bodyMarshal, _ := json.Marshal(bodyJson)
		bodyBytes := bytes.NewBuffer(bodyMarshal)
		resp, err := http.Post(api.BaseURL+"/v1/chain/get_table_rows", "application/json", bodyBytes)
		if err != nil {
			fmt.Println(fmt.Sprintln("Error fetching rows - making request ", err))
			time.Sleep(time.Second * 1)
			continue
		}
		if resp.StatusCode != 200 {
			continue
		}
		body, err := ioutil.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			fmt.Println(fmt.Sprintln("Error fetching rows - reading response ", err))
			time.Sleep(time.Second * 1)
			continue
		}
		var response map[string]interface{}
		err = json.Unmarshal(body, &response)
		if err != nil {
			fmt.Println(fmt.Sprintln("Error fetching rows - unmarshalling response ", err))
			time.Sleep(time.Second * 1)
			continue
		}
		if len(response["rows"].([]interface{})) == 0 {
			break
		}
		for _, row := range response["rows"].([]interface{}) {
			accname := row.(map[string]interface{})["to"].(string)
			if !contains(accountsLoaded, accname) {
				continue
			}
			cpuAss, err := eos.NewAssetFromString(row.(map[string]interface{})["cpu_weight"].(string))
			if err != nil {
				panic(err)
			}
			netAss, err := eos.NewAssetFromString(row.(map[string]interface{})["net_weight"].(string))
			if err != nil {
				panic(err)
			}
			acc := StakedAccount{
				Account:   eos.AccountName(accname),
				CpuWeight: cpuAss,
				NetWeight: netAss,
			}
			if int64(acc.CpuWeight.Amount) < config.CpuUnstakeToInt64() && int64(acc.NetWeight.Amount) < config.NetUnstakeToInt64() {
				// if this account does not have enough stake to unstake, skip it
				continue
			}
			accounts = append(accounts, acc)
		}
		if response["more"].(bool) {
			bodyJson["lower_bound"] = response["next_key"].(string)
		} else {
			break
		}
	}
	fmt.Println("Found ", len(accounts), " accounts to unstake")

	// split accounts into chunks
	chunks := make([][]StakedAccount, 0)
	for i := 0; i < len(accounts); i += config.Config.ChunkSize {
		if i+config.Config.ChunkSize > len(accounts) {
			chunks = append(chunks, accounts[i:])
		} else {
			chunks = append(chunks, accounts[i:i+config.Config.ChunkSize])
		}
	}
	fmt.Println("Chunks: ", len(chunks))

	if len(chunks) == 0 {
		fmt.Println("No chunks to process.")
		os.Exit(0)
	}

	for i, chunk := range chunks {
		firstAcc := chunk[0]
		fmt.Println("Processing chunk #", i)
		wasFirstAcc, err := api.GetAccount(context.Background(), firstAcc.Account)
		if err != nil {
			fmt.Println(fmt.Errorf("getting account %s: %w", firstAcc, err))
			os.Exit(1)
		}
	retry:
		actions := make([]*eos.Action, 0)
		for _, account := range chunk {
			cpuAsset := account.CpuWeight
			netAsset := account.NetWeight
			if int64(cpuAsset.Amount) > config.CpuUnstakeToInt64() {
				cpuAsset.Amount = cpuAsset.Amount - eos.Int64(config.CpuUnstakeToInt64())
			} else {
				cpuAsset.Amount = 0
			}
			if int64(netAsset.Amount) > config.NetUnstakeToInt64() {
				netAsset.Amount = netAsset.Amount - eos.Int64(config.NetUnstakeToInt64())
			} else {
				netAsset.Amount = 0
			}
			if cpuAsset.Amount == 0 && netAsset.Amount == 0 {
				continue
			}
			actions = append(
				actions,
				FullActionUnDelegateBW(eos.AN(config.Config.Account), account.Account, netAsset, cpuAsset),
			)
		}
		packed := MakeAndSignTransaction(actions, []string{config.Config.Pkey})
		resp, err := api.PushTransaction(context.Background(), packed)
		if err != nil {
			fmt.Println(fmt.Errorf("pushing transaction: %w", err))
			os.Exit(1)
		}
		fmt.Println("Transaction ID: ", resp.TransactionID, ", waiting 1.5 seconds and validating...")
		time.Sleep(time.Millisecond * 1500)
		firstValidation := true
	retryValidate:
		fmt.Println("Validating...")
		nowFirstAcc, err := api.GetAccount(context.Background(), firstAcc.Account)
		if err != nil {
			fmt.Println(fmt.Errorf("getting account %s: %w", firstAcc, err))
			os.Exit(1)
		}
		if nowFirstAcc.CPUWeight == wasFirstAcc.CPUWeight && nowFirstAcc.NetWeight == wasFirstAcc.NetWeight {
			fmt.Println("Transaction failed (CPU/NET weight not changed)")
			if firstValidation {
				fmt.Println("Retrying validation in 3.5 seconds...")
				time.Sleep(time.Millisecond * 3500)
				firstValidation = false
				goto retryValidate
			}
			fmt.Println("Could not validate transaction. Re-sending it.")
			goto retry
		}
		fmt.Println("Transaction validated.")
	}
}

func contains[T comparable](elems []T, v T) bool {
	for _, s := range elems {
		if v == s {
			return true
		}
	}
	return false
}
