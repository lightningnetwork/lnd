// +build !windows,!solaris

package healthcheck

import "syscall"

// AvailableDiskSpace returns ratio of available disk space to total capacity.
func AvailableDiskSpace(path string) (float64, error) {
	s := syscall.Statfs_t{}
	err := syscall.Statfs(path, &s)
	if err != nil {
		return 0, err
	}

	// Calculate our free blocks/total blocks to get our total ratio of
	// free blocks.
	return float64(s.Bfree) / float64(s.Blocks), nil
}
