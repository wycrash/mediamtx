package recorder

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsDiskUnavailable(t *testing.T) {
	require.False(t, isDiskUnavailable(nil))
	require.False(t, isDiskUnavailable(fmt.Errorf("permission denied")))
	require.True(t, isDiskUnavailable(fmt.Errorf(
		"mkdir F:\\records\\cam1\\2026-09-09\\15: A device which does not exist was specified.")))
	require.True(t, isDiskUnavailable(fmt.Errorf("mkdir /mnt/dvr: input/output error")))
	require.True(t, isNoSpace(fmt.Errorf("no space left on device")))
	require.True(t, shouldFailoverDisk(fmt.Errorf("no space left on device")))
	require.True(t, shouldFailoverDisk(fmt.Errorf("the device is not ready")))
	require.False(t, shouldFailoverDisk(fmt.Errorf("permission denied")))
}
