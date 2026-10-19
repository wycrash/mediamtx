package onvif

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
)

var (
	reFaultText   = regexp.MustCompile(`(?i)<(?:[\w.-]+:)?Text\b[^>]*>([^<]*)</`)
	reFaultValue  = regexp.MustCompile(`(?i)<(?:[\w.-]+:)?Value\b[^>]*>([^<]*)</`)
	reFaultString = regexp.MustCompile(`(?i)<(?:[\w.-]+:)?faultstring\b[^>]*>([^<]*)</`)
	reFaultCode   = regexp.MustCompile(`(?i)<(?:[\w.-]+:)?faultcode\b[^>]*>([^<]*)</`)
)

// callError keeps the events XAddr next to a SOAP failure. Error() stays the
// library text; logs use soapDetail so the fault is not buried under xmlns.
type callError struct {
	Endpoint string
	Err      error
}

func (e *callError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	if e.Endpoint == "" {
		return e.Err.Error()
	}
	return "endpoint " + e.Endpoint + ": " + e.Err.Error()
}

func (e *callError) Unwrap() error { return e.Err }

// soapDetail is the log line for a camera SOAP failure: HTTP status, fault
// code and reason. Hikvision prefixes the envelope with more than 2048 bytes
// of xmlns, and the library truncates the error string before env:Body.
func soapDetail(err error) string {
	if err == nil {
		return ""
	}
	status, body, endpoint := soapResponse(err)
	summary := faultSummary(body)
	if summary == "" {
		summary = compactError(err)
	}
	var b strings.Builder
	if endpoint != "" {
		b.WriteString(endpoint)
		b.WriteString(": ")
	}
	if status != 0 {
		b.WriteString("HTTP ")
		b.WriteString(itoa(status))
		b.WriteString(": ")
	}
	b.WriteString(summary)
	return b.String()
}

func soapResponse(err error) (status int, body, endpoint string) {
	var call *callError
	if errors.As(err, &call) {
		endpoint = call.Endpoint
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		v := reflect.ValueOf(e)
		if v.Kind() == reflect.Pointer && !v.IsNil() {
			v = v.Elem()
		}
		if !v.IsValid() || v.Kind() != reflect.Struct {
			continue
		}
		if body == "" {
			if f := v.FieldByName("Body"); f.IsValid() && f.Kind() == reflect.String && f.CanInterface() {
				body = f.String()
			}
		}
		if status == 0 {
			if f := v.FieldByName("Status"); f.IsValid() && f.CanInt() && f.CanInterface() {
				status = int(f.Int())
			}
		}
	}
	return status, body, endpoint
}

func faultSummary(body string) string {
	if body == "" || !strings.Contains(strings.ToLower(body), "fault") {
		return ""
	}
	codes := xmlTexts(reFaultValue, body)
	if len(codes) == 0 {
		codes = xmlTexts(reFaultCode, body)
	}
	reason := firstText(reFaultText, body)
	if reason == "" {
		reason = firstText(reFaultString, body)
	}
	reason = strings.TrimSpace(reason)
	code := strings.Join(codes, "/")
	switch {
	case code != "" && reason != "":
		return code + ": " + reason
	case reason != "":
		return reason
	case code != "":
		return code
	default:
		return ""
	}
}

func xmlTexts(re *regexp.Regexp, body string) []string {
	matches := re.FindAllStringSubmatch(body, 8)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		text := strings.TrimSpace(m[1])
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}

func firstText(re *regexp.Regexp, body string) string {
	m := re.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

func compactError(err error) string {
	s := err.Error()
	if i := strings.Index(s, "<?xml"); i >= 0 {
		s = strings.TrimSpace(s[:i])
		s = strings.TrimRight(s, ": ")
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
