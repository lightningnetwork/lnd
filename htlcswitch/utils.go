package htlcswitch

import (
	"fmt"
)

// FIXME: temporary until we can use the global flags
var SPIDER_FLAG bool = true
var DEBUG_FLAG bool = false

func debug_print(str string) {
	if (DEBUG_FLAG) {
		fmt.Printf(str)
	}
}
