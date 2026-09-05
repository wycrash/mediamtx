package mpegts

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/packetdumper"
	ptls "github.com/bluenviron/mediamtx/internal/protocols/tls"
)

type httpConn struct {
	body      io.ReadCloser
	transport *http.Transport
	mutex     sync.Mutex
	deadline  time.Time
	closeOnce sync.Once
	closeErr  error
}

func (c *httpConn) Read(p []byte) (int, error) {
	c.mutex.Lock()
	deadline := c.deadline
	c.mutex.Unlock()

	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			c.Close()
			return 0, os.ErrDeadlineExceeded
		}

		t := time.AfterFunc(remaining, func() {
			c.Close()
		})
		defer t.Stop()
	}

	return c.body.Read(p)
}

func (c *httpConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.body.Close()
		c.transport.CloseIdleConnections()
	})
	return c.closeErr
}

func (c *httpConn) SetReadDeadline(t time.Time) error {
	c.mutex.Lock()
	c.deadline = t
	c.mutex.Unlock()
	return nil
}

func (s *Source) createHTTPConn(params defs.StaticSourceRunParams, u *url.URL) (*httpConn, error) {
	httpURL := *u
	httpURL.Scheme = strings.TrimSuffix(u.Scheme, "+mpegts")

	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: time.Duration(s.ReadTimeout),
		}).DialContext,
		ResponseHeaderTimeout: time.Duration(s.ReadTimeout),
		DisableCompression:    true,
	}

	tlsConfig := ptls.MakeConfig(params.Conf.SourceFingerprint)

	if s.DumpPackets {
		prefix := "mpegts_source_http_conn"
		if httpURL.Scheme == "https" {
			prefix = "mpegts_source_https_conn"
		}

		tr.DialContext = (&packetdumper.DialContext{
			Prefix: prefix,
		}).Do

		tr.DialTLSContext = (&packetdumper.DialTLSContext{
			DialContext: tr.DialContext,
			TLSConfig:   tlsConfig,
		}).Do
	} else {
		tr.TLSClientConfig = tlsConfig
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Transport: tr,
		Jar:       jar,
	}

	req, err := http.NewRequestWithContext(params.Context, http.MethodGet, httpURL.String(), nil)
	if err != nil {
		return nil, err
	}

	res, err := client.Do(req)
	if err != nil {
		tr.CloseIdleConnections()
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		tr.CloseIdleConnections()
		return nil, fmt.Errorf("bad status code: %d", res.StatusCode)
	}

	return &httpConn{
		body:      res.Body,
		transport: tr,
	}, nil
}
