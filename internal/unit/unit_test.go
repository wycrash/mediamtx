package unit

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemuxDTS(t *testing.T) {
	t.Run("pes dts", func(t *testing.T) {
		u := &Unit{PTS: 1000, DTS: 400, HasDTS: true}
		dts, err := u.RemuxDTS(func() (int64, error) {
			t.Fatal("extract should not be called")
			return 0, nil
		})
		require.NoError(t, err)
		require.Equal(t, int64(400), dts)
	})

	t.Run("bitstream", func(t *testing.T) {
		u := &Unit{PTS: 1000}
		dts, err := u.RemuxDTS(func() (int64, error) {
			return 700, nil
		})
		require.NoError(t, err)
		require.Equal(t, int64(700), dts)
	})

	t.Run("bitstream error", func(t *testing.T) {
		u := &Unit{PTS: 1000}
		_, err := u.RemuxDTS(func() (int64, error) {
			return 0, fmt.Errorf("unable to extract DTS")
		})
		require.Error(t, err)
	})
}

func TestTimingTS(t *testing.T) {
	require.Equal(t, int64(1000), (&Unit{PTS: 1000}).TimingTS())
	require.Equal(t, int64(400), (&Unit{PTS: 1000, DTS: 400, HasDTS: true}).TimingTS())
}
