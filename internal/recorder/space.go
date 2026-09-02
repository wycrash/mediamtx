package recorder

import (
	"strings"
)

func isNoSpace(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no space") || strings.Contains(msg, "disk full")
}
