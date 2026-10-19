package onvif

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSOAPActionForCreatePullPoint(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://www.w3.org/2003/05/soap-envelope">
  <Body>
    <CreatePullPointSubscription xmlns="http://www.onvif.org/ver10/events/wsdl"></CreatePullPointSubscription>
  </Body>
</Envelope>`)
	require.Equal(t,
		"http://www.onvif.org/ver10/events/wsdl/EventPortType/CreatePullPointSubscription",
		soapActionFor(body),
	)
}

func TestSOAPActionTransportSetsContentType(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
  <s:Body><tev:PullMessages xmlns:tev="http://www.onvif.org/ver10/events/wsdl"/></s:Body>
</s:Envelope>`
	req, err := http.NewRequest(http.MethodPost, "http://camera/onvif/Events", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")

	var gotAction, gotCT string
	tr := soapActionTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotCT = r.Header.Get("Content-Type")
		gotAction = r.Header.Get("SOAPAction")
		sent, _ := io.ReadAll(r.Body)
		require.Equal(t, body, string(sent))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	})}
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Contains(t, gotCT, `action="http://www.onvif.org/ver10/events/wsdl/PullPointSubscription/PullMessages"`)
	require.Equal(t, `"http://www.onvif.org/ver10/events/wsdl/PullPointSubscription/PullMessages"`, gotAction)
}

func TestFixTopicExpressionAddsDialect(t *testing.T) {
	body := []byte(`<tev:Filter><wsnt:TopicExpression>tns1:VideoSource/MotionAlarm|tns1:RuleEngine/LineDetector/Crossed</wsnt:TopicExpression></tev:Filter>`)
	got := string(fixTopicExpression(body))
	require.Contains(t, got, `Dialect="http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet"`)
	require.Contains(t, got, `xmlns:tns1="http://www.onvif.org/ver10/topics"`)
	require.Contains(t, got, `tns1:VideoSource/MotionAlarm|tns1:RuleEngine/LineDetector/Crossed`)
	require.Equal(t, got, string(fixTopicExpression([]byte(got))))
}

func TestEventsUnsupportedHTTP500(t *testing.T) {
	body := `<env:Body><env:Fault><env:Code><env:Value>env:Receiver</env:Value>` +
		`<env:Subcode><env:Value>ter:ActionNotSupported</env:Value>` +
		`<env:Subcode><env:Value>ter:InvalidOperation</env:Value></env:Subcode></env:Subcode></env:Code>` +
		`<env:Reason><env:Text>Optional Action Not Implemented</env:Text></env:Reason></env:Fault></env:Body>`
	err := &fakeStatus{Status: 500, Body: body}
	require.True(t, EventsUnsupported(err))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
