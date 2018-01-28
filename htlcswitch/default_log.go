// + build !stdlog

package htlcswitch

// The default amount of logging is none.
func init() {
	DisableLog()
}
