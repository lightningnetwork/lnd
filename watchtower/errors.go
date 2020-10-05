package watchtower

import "errors"

var (
	// ErrNoListeners signals that no listening ports were provided,
	// rendering the tower unable to receive client requests.
	ErrNoListeners = errors.New("no listening ports were specified")

	// ErrNoNetwork signals that no tor.Net is provided in the Config, which
	// prevents resolution of listening addresses.
	ErrNoNetwork = errors.New("no network specified, must be tor or clearnet")
)
