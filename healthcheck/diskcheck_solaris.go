package healthcheck

import "golang.org/x/sys/unix"

// AvailableDiskSpaceRatio returns ratio of available disk space to total
// capacity for solaris.
func AvailableDiskSpaceRatio(path string) (float64, error) {
	s := unix.Statvfs_t{}
	err := unix.Statvfs(path, &s)
	if err != nil {
		return 0, err
	}

	// Calculate our free blocks/total blocks to get our total ratio of
	// free blocks.
	return float64(s.Bfree) / float64(s.Blocks), nil
}

// AvailableDiskSpace returns the available disk space in bytes of the given
// file system for solaris.
func AvailableDiskSpace(path string) (uint64, error) {
	s := unix.Statvfs_t{}
	err := unix.Statvfs(path, &s)
	if err != nil {
		return 0, err
	}

	return s.Bavail * uint64(s.Bsize), nil
}
