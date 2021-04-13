package lncfg

// Routing holds the configuration options for routing.
type Routing struct {
	AssumeChannelValid bool `long:"assumechanvalid" description:"DEPRECATED: This is now turned on by default for Neutrino (use neutrino.validatechannels=true to turn off) and shouldn't be used for any other backend! (default: false)"`
}
