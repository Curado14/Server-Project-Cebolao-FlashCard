//go:build windows

package orchestrator

import "golang.org/x/sys/windows"

func freeSpace(path string) (uint64, error) {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &free, nil, nil); err != nil {
		return 0, err
	}
	return free, nil
}
