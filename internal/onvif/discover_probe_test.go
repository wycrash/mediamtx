package onvif

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mickeyzzc/onvif-go/v2/wsdiscovery"
)

func TestFoundFromDatagram(t *testing.T) {
	raw := wsdiscovery.BuildProbeMatches("1", wsdiscovery.Match{
		EndpointRef:     "urn:uuid:abc",
		Types:           "dn:NetworkVideoTransmitter",
		Scopes:          "onvif://www.onvif.org/name/Gate onvif://www.onvif.org/hardware/DS-2CD",
		XAddrs:          "http://10.0.0.5/onvif/device_service http://10.0.0.5:8000/onvif/device_service",
		MetadataVersion: 1,
	})

	found, ok, err := foundFromDatagram(raw)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "http://10.0.0.5/onvif/device_service", found.Address)
	require.Equal(t, "Gate", found.Name)
	require.Equal(t, "DS-2CD", found.Hardware)
	require.Equal(t, "camera", found.Type)
	require.Equal(t, []string{
		"http://10.0.0.5/onvif/device_service",
		"http://10.0.0.5:8000/onvif/device_service",
	}, found.XAddrs)

	_, ok, err = foundFromDatagram([]byte(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body></s:Body></s:Envelope>`))
	require.Error(t, err)
	require.False(t, ok)

	note := datagramNote([]byte("<Hello>\nworld</Hello>"), err, false)
	require.Contains(t, note, "payload=")
	require.NotContains(t, note, "\n")
}

func TestDiscoverySourceIP(t *testing.T) {
	ip, err := discoverySourceIP("10.90.51.240", nil)
	require.NoError(t, err)
	require.Equal(t, "10.90.51.240", ip.String())
	require.Len(t, ip, net.IPv4len)

	_, err = discoverySourceIP("fe80::1", nil)
	require.Error(t, err)
}
