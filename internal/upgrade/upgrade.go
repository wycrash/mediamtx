// Package upgrade contains functions to upgrade the executable.
package upgrade

import (
	"bytes"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"sort"

	"github.com/Masterminds/semver/v3"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/minio/selfupdate"
)

const (
	gitRepo     = "https://github.com/wycrash/mediamtx"
	downloadURL = "https://github.com/wycrash/mediamtx/releases/download/%s/mediamtx_%s_%s_%s.%s"
	executable  = "mediamtx"
)

var (
	tagsRegexp    = regexp.MustCompile(`^refs/tags/(v1\.[0-9]+\.[0-9]+)$`)
	currentRegexp = regexp.MustCompile(`^(v1\.[0-9]+\.[0-9]+)$`)

	lookupLatest = latestRemoteVersion
	fetchBinary  = defaultFetchBinary
	applyBinary  = defaultApplyBinary
)

// ErrUnofficial is returned when the running binary is not an official release.
type ErrUnofficial struct {
	Version string
}

func (e *ErrUnofficial) Error() string {
	return fmt.Sprintf("current version (%v) is not official and cannot be upgraded", e.Version)
}

// Info is the result of a version check or upgrade.
type Info struct {
	Current   string
	Latest    string
	Available bool
}

func latestRemoteVersion() (*semver.Version, error) {
	rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		URLs: []string{gitRepo},
	})

	refs, err := rem.List(&git.ListOptions{})
	if err != nil {
		return nil, err
	}

	var versions []*semver.Version

	for _, ref := range refs {
		matches := tagsRegexp.FindStringSubmatch(ref.Name().String())
		if matches != nil {
			v, _ := semver.NewVersion(matches[1])
			versions = append(versions, v)
		}
	}

	if len(versions) == 0 {
		return nil, fmt.Errorf("no versions found")
	}

	sort.Sort(sort.Reverse(semver.Collection(versions)))

	return versions[0], nil
}

func inspect(version string) (*Info, error) {
	if !currentRegexp.MatchString(version) {
		return nil, &ErrUnofficial{Version: version}
	}

	latest, err := lookupLatest()
	if err != nil {
		return nil, err
	}

	current, _ := semver.NewVersion(version)

	return &Info{
		Current:   "v" + current.String(),
		Latest:    "v" + latest.String(),
		Available: latest.GreaterThan(current),
	}, nil
}

func defaultFetchBinary(latest, arch string) ([]byte, error) {
	ur := fmt.Sprintf(downloadURL, latest, latest, runtime.GOOS, arch, extension)

	res, err := http.Get(ur)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close() //nolint:errcheck

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status code: %v", res.StatusCode)
	}

	return extractExecutable(res.Body)
}

func defaultApplyBinary(bin []byte) error {
	return selfupdate.Apply(bytes.NewReader(bin), selfupdate.Options{})
}

// Upgrade downloads the latest executable and replaces the current one with it.
func Upgrade(version, arch string) (*Info, error) {
	info, err := inspect(version)
	if err != nil {
		return nil, err
	}

	if !info.Available {
		return info, nil
	}

	bin, err := fetchBinary(info.Latest, arch)
	if err != nil {
		return nil, err
	}

	err = applyBinary(bin)
	if err != nil {
		return nil, err
	}

	return info, nil
}
