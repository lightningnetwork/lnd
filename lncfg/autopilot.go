package lncfg

// AutoPilot holds the configuration options for the daemon's autopilot.
//
//nolint:ll
type AutoPilot struct {
	Active         bool               `long:"active" description:"If the autopilot agent should be active or not."`
	Heuristic      map[string]float64 `long:"heuristic" description:"Heuristic to activate, and the weight to give it during scoring."`
	MaxChannels    int                `long:"maxchannels" description:"The maximum number of channels that should be created"`
	Allocation     float64            `long:"allocation" description:"The percentage of total funds that should be committed to automatic channel establishment"`
	MinChannelSize int64              `long:"minchansize" description:"The smallest channel that the autopilot agent should create"`
	MaxChannelSize int64              `long:"maxchansize" description:"The largest channel that the autopilot agent should create"`
	Private        bool               `long:"private" description:"Whether the channels created by the autopilot agent should be private or not. Private channels won't be announced to the network."`
	MinConfs       int32              `long:"minconfs" description:"The minimum number of confirmations each of your inputs in funding transactions created by the autopilot agent must have."`
	ConfTarget     uint32             `long:"conftarget" description:"The confirmation target (in blocks) for channels opened by autopilot."`
}
