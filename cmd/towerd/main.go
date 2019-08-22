package main

import (
	"fmt"
	"path/filepath"

	"github.com/btcsuite/btcutil"
	flags "github.com/jessevdk/go-flags"

	"github.com/lightningnetwork/lnd"
)

var (
	defaultTowerdDir        = btcutil.AppDataDir("towerd", false)
	defaultTowerdConfigFile = filepath.Join(defaultTowerdDir, "towerd.conf")
)

func main() {
	// First need to load the watchtower config
	towerCfg, err := loadTowerConfig()
	if err != nil {
		fmt.Println("Failed to load watchtower config ", err)
	}

	if towerCfg.LightningNode == "lnd" {
		activeChainControl, cfg, err := lnd.GenTowerChainControl()
		if err != nil {
			fmt.Println("Error setting up infrastructure for "+
				"the watchtower to live on top of  ", err)
		}

		towerDir := lnd.CleanAndExpandPath(towerCfg.TowerdDir)
		cfg.Watchtower.TowerDir = towerDir

		fmt.Printf("cfg.Watchtower after everything: %+v",
			cfg.Watchtower)

		err = lnd.CreateTower(activeChainControl, cfg)
		if err != nil {
			fmt.Println("Failed to create watchtower", err)
		}

	} else {
		fmt.Println("Only LND is currently supported as a backend")
	}
}

type towerConfig struct {
	TowerdDir     string
	ConfigFile    string
	LightningNode string
}

func loadTowerConfig() (*towerConfig, error) {
	defaultTowerCfg := towerConfig{
		TowerdDir:     defaultTowerdDir,
		ConfigFile:    defaultTowerdConfigFile,
		LightningNode: "lnd",
	}

	towerCfgFilePath := lnd.CleanAndExpandPath(defaultTowerCfg.ConfigFile)

	cfg := defaultTowerCfg
	if err := flags.IniParse(towerCfgFilePath, &cfg); err != nil {
		// If it's a parsing related error, then we'll return
		// immediately, otherwise we can proceed as possibly the config
		// file doesn't exist which is OK.
		if _, ok := err.(*flags.IniError); ok {
			return nil, err
		}
	}

	return &cfg, nil
}
