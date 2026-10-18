//go:build !windows

package fixer

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func isCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}

// Whether two paths are on the same volume. A path that does not exist yet is judged by its
// closest existing parent
func sameVolume(pathA string, pathB string) (bool, error) {
	device := func(path string) (uint64, error) {
		for {
			info, err := os.Stat(path)
			if err == nil {
				stat, ok := info.Sys().(*syscall.Stat_t)
				if !ok {
					return 0, errors.New("no device information")
				}
				return uint64(stat.Dev), nil
			}
			parent := filepath.Dir(path)
			if parent == path {
				return 0, err
			}
			path = parent
		}
	}
	devA, err := device(pathA)
	if err != nil {
		return false, err
	}
	devB, err := device(pathB)
	if err != nil {
		return false, err
	}
	return devA == devB, nil
}
