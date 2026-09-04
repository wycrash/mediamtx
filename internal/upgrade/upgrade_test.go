package upgrade

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/require"
)

func TestCheckVersionUnofficial(t *testing.T) {
	_, err := CheckVersion("v1.2.3-dirty")
	require.Equal(t, &ErrUnofficial{Version: "v1.2.3-dirty"}, err)
}

func TestUpgradeUnofficial(t *testing.T) {
	_, err := Upgrade("v0.0.0", "amd64")
	require.Equal(t, &ErrUnofficial{Version: "v0.0.0"}, err)
}

func TestCheckVersionAvailable(t *testing.T) {
	orig := lookupLatest
	t.Cleanup(func() { lookupLatest = orig })
	lookupLatest = func() (*semver.Version, error) {
		return semver.NewVersion("1.9.0")
	}

	info, err := CheckVersion("v1.8.0")
	require.NoError(t, err)
	require.Equal(t, &Info{
		Current:   "v1.8.0",
		Latest:    "v1.9.0",
		Available: true,
	}, info)
}

func TestUpgradeUpToDate(t *testing.T) {
	orig := lookupLatest
	t.Cleanup(func() { lookupLatest = orig })
	lookupLatest = func() (*semver.Version, error) {
		return semver.NewVersion("1.8.0")
	}

	info, err := Upgrade("v1.8.0", "amd64")
	require.NoError(t, err)
	require.Equal(t, &Info{
		Current:   "v1.8.0",
		Latest:    "v1.8.0",
		Available: false,
	}, info)
}

func TestUpgradeApplies(t *testing.T) {
	origLookup := lookupLatest
	origFetch := fetchBinary
	origApply := applyBinary
	t.Cleanup(func() {
		lookupLatest = origLookup
		fetchBinary = origFetch
		applyBinary = origApply
	})

	lookupLatest = func() (*semver.Version, error) {
		return semver.NewVersion("1.9.0")
	}
	fetchBinary = func(latest, arch string) ([]byte, error) {
		require.Equal(t, "v1.9.0", latest)
		require.Equal(t, "amd64", arch)
		return []byte("bin"), nil
	}
	applied := false
	applyBinary = func(bin []byte) error {
		require.Equal(t, []byte("bin"), bin)
		applied = true
		return nil
	}

	info, err := Upgrade("v1.8.0", "amd64")
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, &Info{
		Current:   "v1.8.0",
		Latest:    "v1.9.0",
		Available: true,
	}, info)
}

func TestUpgradeFetchError(t *testing.T) {
	origLookup := lookupLatest
	origFetch := fetchBinary
	t.Cleanup(func() {
		lookupLatest = origLookup
		fetchBinary = origFetch
	})

	lookupLatest = func() (*semver.Version, error) {
		return semver.NewVersion("1.9.0")
	}
	fetchBinary = func(string, string) ([]byte, error) {
		return nil, fmt.Errorf("network down")
	}

	_, err := Upgrade("v1.8.0", "amd64")
	require.EqualError(t, err, "network down")
}

func TestErrUnofficialAs(t *testing.T) {
	err := error(&ErrUnofficial{Version: "dev"})
	var unofficial *ErrUnofficial
	require.True(t, errors.As(err, &unofficial))
	require.Equal(t, "dev", unofficial.Version)
}
