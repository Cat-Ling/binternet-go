package pinterest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

type UserAgentTransport struct {
	http.RoundTripper
	UserAgent string
}

func (t *UserAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.UserAgent != "" {
		req.Header.Set("User-Agent", t.UserAgent)
	}
	return t.RoundTripper.RoundTrip(req)
}

func CreatePinterestTransport(fallbackDNS string) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	var fallbackResolver *net.Resolver
	if fallbackDNS != "" {
		fallbackResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: time.Second * 10}
				dnsServer := fallbackDNS
				if !strings.Contains(dnsServer, ":") {
					dnsServer += ":53"
				}
				return d.DialContext(ctx, "udp", dnsServer)
			},
		}
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          5000,
		MaxIdleConnsPerHost:   2000,
		IdleConnTimeout:       300 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ReadBufferSize:        65536,
		WriteBufferSize:       65536,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err == nil {
				return conn, nil
			}

			if fallbackResolver == nil {
				return nil, err
			}

			host, port, _ := net.SplitHostPort(addr)
			ips, err := fallbackResolver.LookupHost(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("fallback DNS lookup failed: %w", err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("fallback DNS returned no IPs for %s", host)
			}

			for _, ip := range ips {
				target := net.JoinHostPort(ip, port)
				conn, err = dialer.DialContext(ctx, network, target)
				if err == nil {
					return conn, nil
				}
			}
			return nil, fmt.Errorf("failed to dial all resolved IPs via fallback DNS: %w", err)
		},
	}
}
