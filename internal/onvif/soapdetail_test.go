package onvif

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeStatus struct {
	Status int
	Body   string
}

func (e *fakeStatus) Error() string {
	body := e.Body
	if len(body) > 40 {
		body = body[:40] + "...(truncated)"
	}
	return "HTTP request failed with status 500: " + body
}

func TestSOAPDetailExtractsFaultPastNamespaceDump(t *testing.T) {
	xmlns := strings.Repeat(` xmlns:n="http://example.test/ns"`, 80)
	body := `<?xml version="1.0" encoding="UTF-8"?> <env:Envelope` + xmlns +
		`><env:Body><env:Fault><env:Code><env:Value>env:Receiver</env:Value>` +
		`<env:Subcode><env:Value>ter:Action</env:Value></env:Subcode></env:Code>` +
		`<env:Reason><env:Text xml:lang="en">Action Not Implemented</env:Text></env:Reason>` +
		`</env:Fault></env:Body></env:Envelope>`
	err := &callError{
		Endpoint: "http://10.0.0.36/onvif/event_service",
		Err:      &fakeStatus{Status: 500, Body: body},
	}
	got := soapDetail(err)
	require.Contains(t, got, "http://10.0.0.36/onvif/event_service")
	require.Contains(t, got, "HTTP 500")
	require.Contains(t, got, "env:Receiver/ter:Action: Action Not Implemented")
	require.NotContains(t, got, "xmlns")
}

func TestSOAPDetailFallsBackWhenBodyHasNoFault(t *testing.T) {
	err := errors.New("dial tcp: connection refused")
	require.Equal(t, "dial tcp: connection refused", soapDetail(err))
}
