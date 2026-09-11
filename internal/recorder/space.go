package recorder

import (
	"os"
	"strings"
)

var (
	osMkdirAll = os.MkdirAll
	osCreate   = os.Create
)

func isNoSpace(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no space") || strings.Contains(msg, "disk full")
}

// isDiskUnavailable reports errors that mean the volume itself is gone or
// hung, so another disk in the storage pool should be used instead.
func isDiskUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, n := range diskUnavailableNeedles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

func shouldFailoverDisk(err error) bool {
	return isNoSpace(err) || isDiskUnavailable(err)
}

var diskUnavailableNeedles = []string{
	"device which does not exist",
	"the device is not ready",
	"cannot find the drive specified",
	"cannot find the path specified",
	"network name is no longer available",
	"network path was not found",
	"semaphore timeout period has expired",
	"device not connected",
	"no such device",
	"input/output error",
	"i/o error",
	"read-only file system",
	"media is write protected",
	"unrecognized file system",
	"the volume does not contain a recognized file system",
}
