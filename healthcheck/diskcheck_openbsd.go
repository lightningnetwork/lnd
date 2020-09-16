package healthcheck

import "golang.org/x/sys/unix"

// AvailableDiskSpace returns ratio of available disk space to total capacity
// for solaris.
func AvailableDiskSpace(path string) (float64, error) {
	s := unix.Statfs_t{}
	err := unix.Statfs(path, &s)
	if err != nil {
		return 0, err
	}

	// Calculate our free blocks/total blocks to get our total ratio of
	// free blocks.
	return float64(s.F_bfree) / float64(s.F_blocks), nil
}
