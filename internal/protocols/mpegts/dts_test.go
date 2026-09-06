package mpegts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDtsFromPTS(t *testing.T) {
	t.Run("simple negative offset", func(t *testing.T) {
		require.Equal(t, int64(90000-3600), dtsFromPTS(90000, 90000-3600, 90000))
	})

	t.Run("equal pts dts", func(t *testing.T) {
		require.Equal(t, int64(12345), dtsFromPTS(100, 100, 12345))
	})

	t.Run("wrap around 33-bit", func(t *testing.T) {
		const modulus = int64(1 << 33)
		rawPTS := int64(1000)
		rawDTS := modulus - 2000 // DTS 2000 ticks behind PTS across wrap
		decodedPTS := int64(5_000_000)
		got := dtsFromPTS(rawPTS, rawDTS, decodedPTS)
		require.Equal(t, decodedPTS-3000, got)
	})
}
