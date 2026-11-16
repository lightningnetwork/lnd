//go:build !linux && !js

package kvdb

// mmapPopulateFlag is zero on platforms that have no equivalent of Linux'
// MAP_POPULATE, which turns prefaulting database files into a no-op.
const mmapPopulateFlag = 0
