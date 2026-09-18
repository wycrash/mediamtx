package conf

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIndexEngineUnmarshal(t *testing.T) {
	t.Run("old", func(t *testing.T) {
		var d IndexEngine
		require.NoError(t, d.UnmarshalJSON([]byte(`"old"`)))
		require.Equal(t, IndexEngineOld, d)
	})
	t.Run("new", func(t *testing.T) {
		var d IndexEngine
		require.NoError(t, d.UnmarshalJSON([]byte(`"new"`)))
		require.Equal(t, IndexEngineNew, d)
	})
	t.Run("empty is old", func(t *testing.T) {
		var d IndexEngine
		require.NoError(t, d.UnmarshalJSON([]byte(`""`)))
		require.Equal(t, IndexEngineOld, d)
	})
	t.Run("invalid", func(t *testing.T) {
		var d IndexEngine
		require.EqualError(t, d.UnmarshalJSON([]byte(`"lazy"`)), "invalid 'compatAPIIndexEngine': 'lazy'")
	})
}
