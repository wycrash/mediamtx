package onvif

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/bluenviron/mediamtx/internal/conf"
)

// Profile is one media profile returned by a probe.
type Profile struct {
	Token     string
	Name      string
	Role      string
	Width     int
	Height    int
	StreamURI string
}

// ProbeResult is a camera inspected with credentials.
type ProbeResult struct {
	Endpoint     string
	Key          string
	Manufacturer string
	Model        string
	Firmware     string
	Serial       string
	Events       bool
	Profiles     []Profile
	Topics       []string
}

// StreamRequest is one stream the caller asked to create.
type StreamRequest struct {
	Profile string
	Name    string
	Record  *bool
	Events  *bool
}

// AddRequest is POST /v3/onvif/paths/add.
type AddRequest struct {
	Name    string
	Address string
	User    string
	Pass    string
	Streams []StreamRequest
}

// PlannedPath is a path the add call should create.
type PlannedPath struct {
	Name         string
	Source       string
	OnvifAddress string
	OnvifUser    string
	OnvifPass    string
	OnvifEvents  bool
	OnvifPath    string
	Record       bool
}

// PlanAdd chooses path names and which path owns the ONVIF session.
// existingOwner is the path that already has this camera address.
func PlanAdd(req AddRequest, probe ProbeResult, paths map[string]*conf.Path, existingOwner string) ([]PlannedPath, error) {
	if err := conf.IsValidPathName(req.Name); err != nil {
		return nil, fmt.Errorf("invalid name: %w", err)
	}
	if len(req.Streams) == 0 {
		return nil, fmt.Errorf("streams is empty")
	}

	ownerIdx := -1
	if existingOwner == "" {
		ownerIdx = 0
		for i, st := range req.Streams {
			if st.Events != nil && *st.Events {
				ownerIdx = i
				break
			}
		}
	}

	ownerName := existingOwner
	if ownerIdx >= 0 {
		ownerName = req.Streams[ownerIdx].Name
		if ownerName == "" {
			ownerName = req.Name
		}
		if err := conf.IsValidPathName(ownerName); err != nil {
			return nil, fmt.Errorf("invalid name: %w", err)
		}
	}

	extra := 0
	out := make([]PlannedPath, 0, len(req.Streams))
	used := map[string]struct{}{}
	for name := range paths {
		used[name] = struct{}{}
	}

	for i, st := range req.Streams {
		profile, err := pickProfile(probe.Profiles, st.Profile)
		if err != nil {
			return nil, err
		}
		source, err := withRTSPCredentials(profile.StreamURI, req.User, req.Pass)
		if err != nil {
			return nil, fmt.Errorf("stream uri: %w", err)
		}

		name := st.Name
		switch {
		case name != "":
		case i == ownerIdx:
			name = ownerName
		case len(req.Streams) == 1 && existingOwner != "":
			name = req.Name
		default:
			extra++
			suffix := "-sub"
			if extra > 1 {
				suffix = fmt.Sprintf("-sub%d", extra)
			}
			name = ownerName + suffix
		}
		if err := conf.IsValidPathName(name); err != nil {
			return nil, fmt.Errorf("invalid name: %w", err)
		}
		if _, ok := used[name]; ok {
			return nil, fmt.Errorf("path '%s' already exists", name)
		}
		used[name] = struct{}{}

		item := PlannedPath{
			Name:   name,
			Source: source,
			Record: st.Record != nil && *st.Record,
		}
		if i == ownerIdx {
			item.OnvifAddress = probe.Endpoint
			item.OnvifUser = req.User
			item.OnvifPass = req.Pass
			item.OnvifEvents = st.Events == nil || *st.Events
		} else {
			item.OnvifPath = ownerName
		}
		out = append(out, item)
	}
	return out, nil
}

func pickProfile(profiles []Profile, want string) (Profile, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		want = "main"
	}
	switch strings.ToLower(want) {
	case "main", "sub":
		role := strings.ToLower(want)
		for _, p := range profiles {
			if p.Role == role {
				if p.StreamURI == "" {
					return Profile{}, fmt.Errorf("profile %s has no stream uri", role)
				}
				return p, nil
			}
		}
		return Profile{}, fmt.Errorf("profile %s was not found", role)
	default:
		for _, p := range profiles {
			if p.Token == want || p.Name == want {
				if p.StreamURI == "" {
					return Profile{}, fmt.Errorf("profile %s has no stream uri", want)
				}
				return p, nil
			}
		}
		return Profile{}, fmt.Errorf("profile %s was not found", want)
	}
}

func withRTSPCredentials(raw, user, pass string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid stream uri")
	}
	if u.User == nil && user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return u.String(), nil
}

// OwnerName returns the path that owns address, or "".
func OwnerName(paths map[string]*conf.Path, address string) (string, error) {
	_, key, err := conf.ParseOnvifAddress(address)
	if err != nil {
		return "", err
	}
	for name, pathConf := range paths {
		if pathConf == nil || pathConf.OnvifAddress == "" {
			continue
		}
		_, other, err := conf.ParseOnvifAddress(pathConf.OnvifAddress)
		if err != nil {
			continue
		}
		if other == key {
			return name, nil
		}
	}
	return "", nil
}
