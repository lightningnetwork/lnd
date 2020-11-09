package healthcheck

import "golang.org/x/sys/windows"

// AvailableDiskSpaceRatio returns ratio of available disk space to total
// capacity for windows.
func AvailableDiskSpaceRatio(path string) (float64, error) {
	var free, total, avail uint64

	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	err = windows.GetDiskFreeSpaceEx(pathPtr, &free, &total, &avail)

	return float64(avail) / float64(total), nil
}

// AvailableDiskSpace returns the available disk space in bytes of the given
// file system for windows.
func AvailableDiskSpace(path string) (uint64, error) {
	var free, total, avail uint64

	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	err = windows.GetDiskFreeSpaceEx(pathPtr, &free, &total, &avail)

	return avail, nil
}
