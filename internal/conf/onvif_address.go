package conf

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ParseOnvifAddress checks an ONVIF device service URL.
// A missing path becomes /onvif/device_service. The key ignores the path so
// http://host:port and the full service URL name one camera.
func ParseOnvifAddress(raw string) (endpoint string, key string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("onvif address is empty")
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", fmt.Errorf("onvif address must be http(s)://host:port")
	}
	if u.User != nil {
		return "", "", fmt.Errorf("onvif address must not contain userinfo")
	}

	host := u.Hostname()
	if host == "" {
		return "", "", fmt.Errorf("onvif address must be http(s)://host:port")
	}

	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if _, err = net.LookupPort("tcp", port); err != nil {
		return "", "", fmt.Errorf("invalid onvif address port")
	}

	if u.Path == "" || u.Path == "/" {
		u.Path = "/onvif/device_service"
	}
	u.Host = net.JoinHostPort(host, port)
	u.RawQuery = ""
	u.Fragment = ""

	endpoint = u.String()
	key = u.Scheme + "://" + strings.ToLower(host) + ":" + port
	return endpoint, key, nil
}
