package pinterest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	BaseURL = "https://www.pinterest.com/resource/BaseSearchResource/get/"
)

type SearchResult struct {
	Images    []string `json:"images"`
	Bookmark  string   `json:"bookmark,omitempty"`
	CSRFToken string   `json:"-"` // Internal use
}

type PinterestClient struct {
	HTTPClient *http.Client
}

func NewClient(fallbackDNS string) *PinterestClient {
	// No cookiejar, mimicking PHP manual approach
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          500,
		MaxIdleConnsPerHost:   500,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			// If default failed and it looks like a DNS issue (or any issue really, simple fallback), try custom resolver
			// fmt.Printf("Default dial failed for %s: %v. Retrying with fallback DNS %s...\n", addr, err, fallbackDNS)

			host, port, _ := net.SplitHostPort(addr)

			r := &net.Resolver{
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

			ips, err := r.LookupHost(ctx, host)
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
			return nil, fmt.Errorf("failed to dial all resolved IPs via fallback DNS")
		},
	}

	return &PinterestClient{
		HTTPClient: &http.Client{
			Transport: &UserAgentTransport{
				RoundTripper: transport,
				UserAgent:    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36",
			},
			// No Jar
			Timeout: 30 * time.Second,
		},
	}
}

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

// SearchWeb mirrors search.php behavior
func (c *PinterestClient) SearchWeb(query string, bookmark string, csrfToken string) (*SearchResult, error) {
	dataParamObj := map[string]interface{}{
		"options": map[string]interface{}{
			"query": query,
		},
	}

	if bookmark != "" {
		dataParamObj["options"].(map[string]interface{})["bookmarks"] = []string{bookmark}
	}

	if bookmark != "" && csrfToken == "" {
		var err error
		csrfToken, err = c.FetchCSRFToken()
		if err != nil {
			// Fail silently or maybe return error? User asked for less output.
		}
	}

	return c.executeRequest(dataParamObj, bookmark, csrfToken, true)
}

// SearchAPI mirrors api.php behavior
func (c *PinterestClient) SearchAPI(query string, bookmark string, csrfToken string) (*SearchResult, error) {
	options := map[string]interface{}{
		"query": query,
	}

	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}

	if bookmark != "" && csrfToken == "" {
		var err error
		csrfToken, err = c.FetchCSRFToken()
		if err != nil {
			// Fail silently
		}
	}

	return c.executeRequest(options, bookmark, csrfToken, false)
}

func (c *PinterestClient) executeRequest(data interface{}, bookmark string, csrfToken string, isWeb bool) (*SearchResult, error) {
	dataParamBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal data param: %w", err)
	}

	dataParam := url.QueryEscape(string(dataParamBytes))

	var req *http.Request
	var finalURL string

	if bookmark == "" {
		finalURL = fmt.Sprintf("%s?data=%s", BaseURL, dataParam)
		req, err = http.NewRequest("GET", finalURL, nil)
	} else {
		finalURL = BaseURL
		req, err = http.NewRequest("POST", finalURL, strings.NewReader("data="+dataParam))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if isWeb {
		req.Header.Set("x-pinterest-pws-handler", "www/search/[scope].js")
		req.Header.Set("Referer", "https://www.pinterest.com/")
	}

	if csrfToken != "" {
		req.Header.Set("x-csrftoken", csrfToken)
		req.Header.Set("Cookie", fmt.Sprintf("csrftoken=%s", csrfToken))
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pinterest API returned status: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Manual CSRF extraction from response headers if present?
	// PHP script didn't seem to do this for the search response itself,
	// only for the initial fetch? Wait, the PHP snippet provided in `search.php`
	// has a `header_function` that updates `$csrftoken` from `Set-Cookie`.
	// So we should update it too.

	newCSRFToken := csrfToken
	cookies := resp.Cookies()
	for _, cookie := range cookies {
		if cookie.Name == "csrftoken" {
			newCSRFToken = cookie.Value
			break
		}
	}

	type PinterestResponse struct {
		ResourceResponse struct {
			Data struct {
				Results []struct {
					Images struct {
						Orig struct {
							URL string `json:"url"`
						} `json:"orig"`
					} `json:"images"`
				} `json:"results"`
			} `json:"data"`
			Bookmark string `json:"bookmark"`
		} `json:"resource_response"`
	}

	var pResp PinterestResponse
	if err := json.Unmarshal(bodyBytes, &pResp); err != nil {
		snippet := string(bodyBytes)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("failed to parse JSON response: %w. Body snippet: %s", err, snippet)
	}

	var images []string
	if pResp.ResourceResponse.Data.Results != nil {
		for _, result := range pResp.ResourceResponse.Data.Results {
			if result.Images.Orig.URL != "" {
				images = append(images, result.Images.Orig.URL)
			}
		}
	}

	return &SearchResult{
		Images:    images,
		Bookmark:  pResp.ResourceResponse.Bookmark,
		CSRFToken: newCSRFToken,
	}, nil
}

// FetchCSRFToken visits the base URL to manually extract a CSRF token.
func (c *PinterestClient) FetchCSRFToken() (string, error) {
	// Try root domain with HEAD request, similar to curl -I
	req, err := http.NewRequest("HEAD", "https://www.pinterest.com/", nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request for csrf token: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch base url for csrf token: %w", err)
	}
	defer resp.Body.Close()

	// Manually check Set-Cookie headers
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "csrftoken" {
			return cookie.Value, nil
		}
	}

	return "", fmt.Errorf("csrftoken cookie not found in response headers from root")
}
