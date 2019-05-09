package migrations

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
)

type migration021CoinConfig struct {
	Type             string                 `json:"Type"`
	APIPool          []string               `json:"API"`
	APITestnetPool   []string               `json:"APITestnet"`
	MaxFee           uint64                 `json:"MaxFee"`
	FeeAPI           string                 `json:"FeeAPI"`
	HighFeeDefault   uint64                 `json:"HighFeeDefault"`
	MediumFeeDefault uint64                 `json:"MediumFeeDefault"`
	LowFeeDefault    uint64                 `json:"LowFeeDefault"`
	TrustedPeer      string                 `json:"TrustedPeer"`
	WalletOptions    map[string]interface{} `json:"WalletOptions"`
}

func migration021DefaultXMRCoinConfig() *migration021CoinConfig {
	return &migration021CoinConfig{
		Type:             "API",
		APIPool:          []string{},
		APITestnetPool:   []string{},
		FeeAPI:           "",
		LowFeeDefault:    5,
		MediumFeeDefault: 10,
		HighFeeDefault:   20,
		MaxFee:           200,
		WalletOptions:    nil,
	}
}

type Migration021 struct{}

func (Migration021) Up(repoPath, dbPassword string, testnet bool) error {
	var (
		configMap        = make(map[string]interface{})
		configBytes, err = ioutil.ReadFile(path.Join(repoPath, "config"))
	)
	if err != nil {
		return fmt.Errorf("reading config: %s", err.Error())
	}

	if err = json.Unmarshal(configBytes, &configMap); err != nil {
		return fmt.Errorf("unmarshal config: %s", err.Error())
	}

	c, ok := configMap["Wallets"]
	if !ok {
		return errors.New("invalid config: missing key Wallets")
	}

	walletCfg, ok := c.(map[string]interface{})
	if !ok {
		return errors.New("invalid config: invalid key Wallets")
	}

	_, ok = walletCfg["XMR"]
	if ok {
		return errors.New("invalid config: XMR wallet already present")
	}

	walletCfg["XMR"] = migration021DefaultXMRCoinConfig()
	configMap["Wallets"] = walletCfg

	newConfigBytes, err := json.MarshalIndent(configMap, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal migrated config: %s", err.Error())
	}

	if err := ioutil.WriteFile(path.Join(repoPath, "config"), newConfigBytes, os.ModePerm); err != nil {
		return fmt.Errorf("writing migrated config: %s", err.Error())
	}

	if err := writeRepoVer(repoPath, 22); err != nil {
		return fmt.Errorf("bumping repover to 22: %s", err.Error())
	}
	return nil
}

func (Migration021) Down(repoPath, dbPassword string, testnet bool) error {
	var (
		configMap        = make(map[string]interface{})
		configBytes, err = ioutil.ReadFile(path.Join(repoPath, "config"))
	)
	if err != nil {
		return fmt.Errorf("reading config: %s", err.Error())
	}

	if err = json.Unmarshal(configBytes, &configMap); err != nil {
		return fmt.Errorf("unmarshal config: %s", err.Error())
	}

	c, ok := configMap["Wallets"]
	if !ok {
		return errors.New("invalid config: missing key Wallets")
	}

	walletCfg, ok := c.(map[string]interface{})
	if !ok {
		return errors.New("invalid config: invalid key Wallets")
	}

	_, ok = walletCfg["XMR"]
	if !ok {
		return errors.New("invalid config: missing key XMR")
	}

	delete(walletCfg, "XMR")
	configMap["Wallets"] = walletCfg

	newConfigBytes, err := json.MarshalIndent(configMap, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal migrated config: %s", err.Error())
	}

	if err := ioutil.WriteFile(path.Join(repoPath, "config"), newConfigBytes, os.ModePerm); err != nil {
		return fmt.Errorf("writing migrated config: %s", err.Error())
	}

	if err := writeRepoVer(repoPath, 21); err != nil {
		return fmt.Errorf("dropping repover to 21: %s", err.Error())
	}
	return nil
}
