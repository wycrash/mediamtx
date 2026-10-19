package onvif

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	ov "github.com/mickeyzzc/onvif-go/v2/onvif"
)

// Hikvision dispatches SOAP 1.2 by the Content-Type action parameter.
// onvif-go sends application/soap+xml with an empty action, and the camera
// answers CreatePullPointSubscription with Optional Action Not Implemented.
var soapActions = map[string]string{
	"CreatePullPointSubscription": "http://www.onvif.org/ver10/events/wsdl/EventPortType/CreatePullPointSubscription",
	"GetEventProperties":          "http://www.onvif.org/ver10/events/wsdl/EventPortType/GetEventProperties",
	"GetServiceCapabilities":      "http://www.onvif.org/ver10/events/wsdl/EventPortType/GetServiceCapabilities",
	"PullMessages":                "http://www.onvif.org/ver10/events/wsdl/PullPointSubscription/PullMessages",
	"Seek":                        "http://www.onvif.org/ver10/events/wsdl/PullPointSubscription/Seek",
	"SetSynchronizationPoint":     "http://www.onvif.org/ver10/events/wsdl/PullPointSubscription/SetSynchronizationPoint",
	"Renew":                       "http://docs.oasis-open.org/wsn/bw-2/SubscriptionManager/RenewRequest",
	"Unsubscribe":                 "http://docs.oasis-open.org/wsn/bw-2/SubscriptionManager/UnsubscribeRequest",
}

// concreteSetDialect is the ONVIF topic-expression dialect for a list joined by "|".
const concreteSetDialect = "http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet"

var (
	reTopicExpr = regexp.MustCompile(`(?s)<((?:[\w.-]+:)?TopicExpression)\b([^>]*)>(.*?)</((?:[\w.-]+:)?TopicExpression)>`)
	reTopicPref = regexp.MustCompile(`\b([A-Za-z_][\w.-]*):`)
	topicNS     = map[string]string{
		"tns1":   "http://www.onvif.org/ver10/topics",
		"tnshik": "http://www.hikvision.com/2011/event/topics",
	}
)

type soapActionTransport struct {
	base http.RoundTripper
}

func (t soapActionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = fixTopicExpression(body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		req.ContentLength = int64(len(body))
		if strings.Contains(req.Header.Get("Content-Type"), "action=") {
			return base.RoundTrip(req)
		}
		if action := soapActionFor(body); action != "" {
			ct := req.Header.Get("Content-Type")
			if ct == "" {
				ct = "application/soap+xml; charset=utf-8"
			}
			req.Header.Set("Content-Type", ct+`; action="`+action+`"`)
			if req.Header.Get("SOAPAction") == "" {
				req.Header.Set("SOAPAction", `"`+action+`"`)
			}
		}
	}
	return base.RoundTrip(req)
}

// fixTopicExpression adds the ConcreteSet dialect and the topic namespace.
// onvif-go sends a bare TopicExpression, which Dahua rejects as invalid.
func fixTopicExpression(body []byte) []byte {
	return reTopicExpr.ReplaceAllFunc(body, func(match []byte) []byte {
		parts := reTopicExpr.FindSubmatch(match)
		if parts == nil {
			return match
		}
		attrs := string(parts[2])
		if strings.Contains(strings.ToLower(attrs), "dialect=") {
			return match
		}
		var extra strings.Builder
		extra.WriteString(` Dialect="`)
		extra.WriteString(concreteSetDialect)
		extra.WriteByte('"')
		seen := map[string]bool{}
		for _, p := range reTopicPref.FindAllSubmatch(parts[3], -1) {
			prefix := string(p[1])
			if seen[prefix] {
				continue
			}
			seen[prefix] = true
			ns, ok := topicNS[prefix]
			if !ok || strings.Contains(attrs, "xmlns:"+prefix+"=") {
				continue
			}
			extra.WriteString(` xmlns:`)
			extra.WriteString(prefix)
			extra.WriteString(`="`)
			extra.WriteString(ns)
			extra.WriteByte('"')
		}
		var b bytes.Buffer
		b.WriteByte('<')
		b.Write(parts[1])
		b.WriteString(extra.String())
		b.Write(parts[2])
		b.WriteByte('>')
		b.Write(parts[3])
		b.WriteString("</")
		b.Write(parts[4])
		b.WriteByte('>')
		return b.Bytes()
	})
}

func soapActionFor(body []byte) string {
	var env struct {
		Body struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(body, &env); err != nil || len(env.Body.Inner) == 0 {
		return ""
	}
	dec := xml.NewDecoder(bytes.NewReader(env.Body.Inner))
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		return soapActions[start.Name.Local]
	}
}

func onvifHTTPClient() *http.Client {
	return &http.Client{
		Transport: soapActionTransport{base: &http.Transport{
			MaxIdleConns:        ov.DefaultMaxIdleConns,
			MaxIdleConnsPerHost: ov.DefaultMaxIdleConnsPerHost,
			IdleConnTimeout:     90 * time.Second,
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
