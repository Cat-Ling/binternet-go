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
	"sync"
	"time"
)

const (
	BaseURL = "https://www.pinterest.com/resource/BaseSearchResource/get/"
)

// MediaItem represents either an image or a video from search results
type MediaItem struct {
	URL          string `json:"url" msgpack:"u"`                     // Image URL or video thumbnail URL
	IsVideo      bool   `json:"is_video" msgpack:"v"`                // true if this is a video pin
	VideoURL     string `json:"video_url,omitempty" msgpack:"vu"`    // HLS or MP4 video URL
	ThumbnailURL string `json:"thumbnail_url,omitempty" msgpack:"t"` // Video thumbnail
	Duration     int    `json:"duration,omitempty" msgpack:"d"`      // Video duration in milliseconds
	Width        int    `json:"width,omitempty" msgpack:"w"`
	Height       int    `json:"height,omitempty" msgpack:"h"`
}

type SearchResult struct {
	Images    []string    `json:"images" msgpack:"i"`          // Legacy: image-only URLs
	Media     []MediaItem `json:"media,omitempty" msgpack:"m"` // All media (images + videos)
	Bookmark  string      `json:"bookmark,omitempty" msgpack:"b"`
	CSRFToken string      `json:"csrf_token,omitempty" msgpack:"c"`
}

type PinterestClient struct {
	HTTPClient       *http.Client
	FallbackResolver *net.Resolver

	// Cached CSRF token
	csrfMu    sync.Mutex
	csrfToken string
	csrfFetch time.Time
}

const csrfTTL = 30 * time.Minute

func NewClient(fallbackDNS string) *PinterestClient {
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

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2000,
		MaxIdleConnsPerHost:   1000,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
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

	return &PinterestClient{
		HTTPClient: &http.Client{
			Transport: &UserAgentTransport{
				RoundTripper: transport,
				UserAgent:    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36",
			},
			Timeout: 30 * time.Second,
		},
		FallbackResolver: fallbackResolver,
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

func (c *PinterestClient) getCachedCSRFToken(ctx context.Context) (string, error) {
	c.csrfMu.Lock()
	defer c.csrfMu.Unlock()

	if c.csrfToken != "" && time.Since(c.csrfFetch) < csrfTTL {
		return c.csrfToken, nil
	}

	token, err := c.fetchCSRFToken(ctx)
	if err != nil {
		return "", err
	}

	c.csrfToken = token
	c.csrfFetch = time.Now()
	return token, nil
}

func (c *PinterestClient) SearchWeb(ctx context.Context, query string, bookmark string, csrfToken string) (*SearchResult, error) {
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
		csrfToken, err = c.getCachedCSRFToken(ctx)
		if err != nil {
			// Fail silently
		}
	}

	return c.executeRequest(ctx, dataParamObj, bookmark, csrfToken, true)
}

func (c *PinterestClient) SearchAPI(ctx context.Context, query string, bookmark string, csrfToken string) (*SearchResult, error) {
	options := map[string]interface{}{
		"query": query,
	}

	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}

	if bookmark != "" && csrfToken == "" {
		var err error
		csrfToken, err = c.getCachedCSRFToken(ctx)
		if err != nil {
			// Fail silently
		}
	}

	return c.executeRequest(ctx, options, bookmark, csrfToken, false)
}

func (c *PinterestClient) executeRequest(ctx context.Context, data interface{}, bookmark string, csrfToken string, isWeb bool) (*SearchResult, error) {
	dataParamBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal data param: %w", err)
	}

	dataParam := url.QueryEscape(string(dataParamBytes))

	var req *http.Request
	var finalURL string

	if bookmark == "" {
		finalURL = fmt.Sprintf("%s?data=%s", BaseURL, dataParam)
		req, err = http.NewRequestWithContext(ctx, "GET", finalURL, nil)
	} else {
		finalURL = BaseURL
		req, err = http.NewRequestWithContext(ctx, "POST", finalURL, strings.NewReader("data="+dataParam))
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

	// Update cached CSRF token from response cookies
	newCSRFToken := csrfToken
	cookies := resp.Cookies()
	for _, cookie := range cookies {
		if cookie.Name == "csrftoken" {
			newCSRFToken = cookie.Value
			c.csrfMu.Lock()
			c.csrfToken = newCSRFToken
			c.csrfFetch = time.Now()
			c.csrfMu.Unlock()
			break
		}
	}

	// Parse the raw Pinterest API response
	var pResp pinterestResponse
	if err := json.Unmarshal(bodyBytes, &pResp); err != nil {
		snippet := string(bodyBytes)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("failed to parse JSON response: %w. Body snippet: %s", err, snippet)
	}

	var images []string
	var media []MediaItem

	if pResp.ResourceResponse.Data.Results != nil {
		for _, result := range pResp.ResourceResponse.Data.Results {
			item := extractMediaItem(&result)
			media = append(media, item)

			// Legacy images list — use thumbnail for videos, orig URL for images
			if item.IsVideo && item.ThumbnailURL != "" {
				images = append(images, item.ThumbnailURL)
			} else if item.URL != "" {
				images = append(images, item.URL)
			}
		}
	}

	return &SearchResult{
		Images:    images,
		Media:     media,
		Bookmark:  pResp.ResourceResponse.Bookmark,
		CSRFToken: newCSRFToken,
	}, nil
}

// extractMediaItem pulls image or video data from a single Pinterest result
func extractMediaItem(result *pinterestResult) MediaItem {
	item := MediaItem{}

	// Default: extract image URL
	if result.Images.Orig.URL != "" {
		item.URL = result.Images.Orig.URL
	}

	// Check for video content via story_pin_data
	if result.StoryPinData != nil && len(result.StoryPinData.Pages) > 0 {
		for _, page := range result.StoryPinData.Pages {
			for _, block := range page.Blocks {
				if block.Video != nil && block.Video.VideoList != nil {
					item.IsVideo = true

					// Duration from top-level story_pin_data
					if result.StoryPinData.TotalVideoDuration > 0 {
						item.Duration = result.StoryPinData.TotalVideoDuration
					}

					// Try to get the best video URL and thumbnail
					// Priority: Master Playlists (V_HLSV4, V_HLSV3_MOBILE) > Static Resolutions
					vl := block.Video.VideoList
					var bestVariant *videoVariant

					for _, key := range []string{"V_HLSV4", "V_HLSV3_MOBILE", "V_720W", "V_360W", "V_240W"} {
						if v, ok := vl.Variants[key]; ok {
							bestVariant = &v
							break
						}
					}

					if bestVariant != nil {
						item.VideoURL = bestVariant.URL
						item.Width = bestVariant.Width
						item.Height = bestVariant.Height
						if bestVariant.Thumbnail != "" {
							item.ThumbnailURL = bestVariant.Thumbnail
							// For videos, use the thumbnail as the display URL
							if item.URL == "" {
								item.URL = bestVariant.Thumbnail
							}
						}
						if bestVariant.Duration > 0 && item.Duration == 0 {
							item.Duration = bestVariant.Duration
						}
					}

					return item // Found video, done
				}
			}
		}
	}

	// Check for videos field directly on the result (some pins use this)
	if result.Videos != nil && result.Videos.VideoList != nil {
		item.IsVideo = true
		vl := result.Videos.VideoList

		var bestVariant *videoVariant
		for _, key := range []string{"V_HLSV4", "V_HLSV3_MOBILE", "V_720W", "V_360W", "V_240W"} {
			if v, ok := vl.Variants[key]; ok {
				bestVariant = &v
				break
			}
		}

		if bestVariant != nil {
			item.VideoURL = bestVariant.URL
			item.Width = bestVariant.Width
			item.Height = bestVariant.Height
			if bestVariant.Thumbnail != "" {
				item.ThumbnailURL = bestVariant.Thumbnail
				if item.URL == "" {
					item.URL = bestVariant.Thumbnail
				}
			}
			if bestVariant.Duration > 0 {
				item.Duration = bestVariant.Duration
			}
		}
	}

	return item
}

// --- Internal JSON response types for parsing Pinterest API ---

type pinterestResponse struct {
	ResourceResponse struct {
		Data struct {
			Results []pinterestResult `json:"results"`
		} `json:"data"`
		Bookmark string `json:"bookmark"`
	} `json:"resource_response"`
}

type pinterestResult struct {
	Images struct {
		Orig struct {
			URL string `json:"url"`
		} `json:"orig"`
	} `json:"images"`

	// Video via story_pin_data (most common for video pins)
	StoryPinData *storyPinData `json:"story_pin_data"`

	// Video via direct videos field (some pins)
	Videos *videoData `json:"videos"`
}

type storyPinData struct {
	TotalVideoDuration int         `json:"total_video_duration"` // milliseconds
	Pages              []storyPage `json:"pages"`
}

type storyPage struct {
	Blocks []storyBlock `json:"blocks"`
}

type storyBlock struct {
	BlockType int        `json:"block_type"`
	Video     *videoData `json:"video"`
}

type videoData struct {
	ID        string     `json:"id"`
	VideoList *videoList `json:"video_list"`
}

type videoList struct {
	Variants map[string]videoVariant `json:"-"` // custom unmarshal
}

type videoVariant struct {
	URL       string `json:"url"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Duration  int    `json:"duration"`
	Thumbnail string `json:"thumbnail"`
}

// UnmarshalJSON for videoList handles the dynamic keys (V_720W, V_HLSV3_MOBILE, etc.)
func (vl *videoList) UnmarshalJSON(data []byte) error {
	vl.Variants = make(map[string]videoVariant)
	return json.Unmarshal(data, &vl.Variants)
}

// FetchCSRFToken is the public version that uses the cache.
func (c *PinterestClient) FetchCSRFToken(ctx context.Context) (string, error) {
	return c.getCachedCSRFToken(ctx)
}

func (c *PinterestClient) fetchCSRFToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", "https://www.pinterest.com/", nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request for csrf token: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch base url for csrf token: %w", err)
	}
	defer resp.Body.Close()

	for _, cookie := range resp.Cookies() {
		if cookie.Name == "csrftoken" {
			return cookie.Value, nil
		}
	}

	return "", fmt.Errorf("csrftoken cookie not found in response headers from root")
}
