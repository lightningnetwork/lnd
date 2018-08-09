// Copyright (C) 2018 The Lightning Network Developers

package extpoints

import (
	"github.com/lightningnetwork/lnd/config"
)

// EventListener is an extension point that exposes events to listeners
type EventListener interface {
	LndStart(cfg *config.Config) error
}
