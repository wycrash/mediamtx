package conf

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIndexEngineUnmarshal(t *testing.T) {
	t.Run("inram", func(t *testing.T) {
		var d IndexEngine
		require.NoError(t, d.UnmarshalJSON([]byte(`"inram"`)))
		require.Equal(t, IndexEngineInRAM, d)
	})
	t.Run("hybrid", func(t *testing.T) {
		var d IndexEngine
		require.NoError(t, d.UnmarshalJSON([]byte(`"hybrid"`)))
		require.Equal(t, IndexEngineHybrid, d)
	})
	t.Run("empty is hybrid", func(t *testing.T) {
		var d IndexEngine
		require.NoError(t, d.UnmarshalJSON([]byte(`""`)))
		require.Equal(t, IndexEngineHybrid, d)
	})
	t.Run("invalid", func(t *testing.T) {
		var d IndexEngine
		require.EqualError(t, d.UnmarshalJSON([]byte(`"lazy"`)), "invalid 'compatAPIIndexEngine': 'lazy'")
	})
}
