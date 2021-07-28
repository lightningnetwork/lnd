package healthcheck

import "fmt"

// AvailableDiskSpaceRatio returns ratio of available disk space to total
// capacity.
func AvailableDiskSpaceRatio(_ string) (float64, error) {
	return 0, fmt.Errorf("disk space check not supported in WebAssembly")
}

// AvailableDiskSpace returns the available disk space in bytes of the given
// file system.
func AvailableDiskSpace(_ string) (uint64, error) {
	return 0, fmt.Errorf("disk space check not supported in WebAssembly")
}
