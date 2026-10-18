//go:build windows

package fixer

import (
	"errors"
	"syscall"
)

// ERROR_NOT_SAME_DEVICE
const errorNotSameDevice syscall.Errno = 17

func isCrossDevice(err error) bool {
	return errors.Is(err, errorNotSameDevice)
}

// Windows has no cheap device check, MoveFile reports a cross-volume move when it happens
func sameVolume(pathA string, pathB string) (bool, error) {
	return true, nil
}
