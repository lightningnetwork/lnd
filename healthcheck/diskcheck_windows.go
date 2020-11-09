package healthcheck

import "golang.org/x/sys/windows"

// AvailableDiskSpace returns ratio of available disk space to total capacity
// for windows.
func AvailableDiskSpace(path string) (float64, error) {
	var free, total, avail uint64

	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		panic(err)
	}
	err = windows.GetDiskFreeSpaceEx(pathPtr, &free, &total, &avail)

	return float64(avail) / float64(total), nil
}
