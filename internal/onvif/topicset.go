package onvif

import (
	"bytes"
	"context"
	"encoding/xml"
	"sort"
	"strings"

	ov "github.com/mickeyzzc/onvif-go/v2/onvif"
)

// eventTopics asks the camera for its topic set and returns the concrete
// topics (wstop:topic="true") as paths such as tns1:VideoSource/MotionAlarm.
// A failure leaves the probe result without a list; profiles still succeed.
func eventTopics(ctx context.Context, client *ov.Client) ([]string, error) {
	endpoint := clientEventEndpoint(client)
	if endpoint == "" {
		endpoint = client.Endpoint()
	}

	var resp struct {
		TopicSet struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"TopicSet"`
	}
	req := struct {
		XMLName xml.Name `xml:"tev:GetEventProperties"`
		Xmlns   string   `xml:"xmlns:tev,attr"`
	}{Xmlns: "http://www.onvif.org/ver10/events/wsdl"}

	if err := client.Call(ctx, endpoint, "", req, &resp); err != nil {
		return nil, err
	}
	return flattenTopicSet(resp.TopicSet.Inner), nil
}

// flattenTopicSet turns a TopicSet fragment into subscribable topic paths.
func flattenTopicSet(raw []byte) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []string{}
	}
	// Prefixes such as tns1 are usually declared on the envelope, not inside
	// the TopicSet fragment the SOAP library returns.
	doc := make([]byte, 0, len(raw)+256)
	doc = append(doc, []byte(`<TopicSet xmlns:tns1="http://www.onvif.org/ver10/topics" xmlns:tnshik="http://www.hikvision.com/2011/event/topics" xmlns:wstop="http://docs.oasis-open.org/wsn/t-1" xmlns:tt="http://www.onvif.org/ver10/schema">`)...)
	doc = append(doc, raw...)
	doc = append(doc, []byte(`</TopicSet>`)...)

	dec := xml.NewDecoder(bytes.NewReader(doc))
	var (
		path  []string
		out   []string
		depth int
	)
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch el := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 || el.Name.Local == "MessageDescription" {
				if el.Name.Local == "MessageDescription" {
					_ = dec.Skip()
					depth--
				}
				continue
			}
			name := startQName(doc, int(dec.InputOffset()))
			if name == "" {
				name = el.Name.Local
			}
			path = append(path, name)
			if topicAttrTrue(el) {
				out = append(out, strings.Join(path, "/"))
			}
		case xml.EndElement:
			if depth > 0 {
				depth--
			}
			if depth == 0 {
				continue
			}
			if len(path) > 0 {
				path = path[:len(path)-1]
			}
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func topicAttrTrue(el xml.StartElement) bool {
	for _, attr := range el.Attr {
		if attr.Name.Local == "topic" && strings.EqualFold(strings.TrimSpace(attr.Value), "true") {
			return true
		}
	}
	return false
}

func startQName(raw []byte, offset int) string {
	if offset < 0 {
		return ""
	}
	if offset > len(raw) {
		offset = len(raw)
	}
	chunk := raw[:offset]
	i := bytes.LastIndex(chunk, []byte("<"))
	if i < 0 || i+1 >= len(chunk) {
		return ""
	}
	rest := chunk[i+1:]
	if rest[0] == '/' || rest[0] == '!' || rest[0] == '?' {
		return ""
	}
	j := 0
	for j < len(rest) {
		switch rest[j] {
		case ' ', '\t', '\n', '\r', '>', '/':
			return string(rest[:j])
		}
		j++
	}
	return string(rest)
}
