package pinterest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
)

const (
	baseURL = "https://www.pinterest.com"
	csrfTTL = 30 * time.Minute
)

type PinterestClient struct {
	HTTPClient *http.Client

	// Cached CSRF token and App version
	csrfMu     sync.Mutex
	csrfToken  string
	csrfFetch  time.Time
	appVersion string
}

func NewClient(fallbackDNS string) *PinterestClient {
	transport := CreatePinterestTransport(fallbackDNS)

	return &PinterestClient{
		HTTPClient: &http.Client{
			Transport: &UserAgentTransport{
				RoundTripper: transport,
				UserAgent:    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36",
			},
			Timeout: 30 * time.Second,
		},
	}
}

func (c *PinterestClient) GetCSRFToken(ctx context.Context) (string, error) {
	c.csrfMu.Lock()
	defer c.csrfMu.Unlock()

	if c.csrfToken != "" && time.Since(c.csrfFetch) < csrfTTL {
		return c.csrfToken, nil
	}

	token, version, err := c.fetchCSRFToken(ctx)
	if err != nil {
		if version != "" {
			c.appVersion = version
		}
		return "", err
	}

	c.csrfToken = token
	if version != "" {
		c.appVersion = version
	}
	c.csrfFetch = time.Now()
	return token, nil
}

func (c *PinterestClient) getAppVersion(ctx context.Context) string {
	c.csrfMu.Lock()
	version := c.appVersion
	c.csrfMu.Unlock()

	if version != "" {
		return version
	}

	c.GetCSRFToken(ctx)

	c.csrfMu.Lock()
	defer c.csrfMu.Unlock()
	return c.appVersion
}

func (c *PinterestClient) fetchCSRFToken(ctx context.Context) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL, nil)
	if err != nil {
		return "", "", err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("failed to fetch homepage: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	// Simple extraction for CSRF token and App version
	html := string(body)
	csrf := ""
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "csrftoken" {
			csrf = cookie.Value
			break
		}
	}

	version := ""
	// Look for app_version in JSON blocks
	if idx := strings.Index(html, "\"app_version\":\""); idx != -1 {
		start := idx + 15
		end := strings.Index(html[start:], "\"")
		if end != -1 {
			version = html[start : start+end]
		}
	}

	return csrf, version, nil
}

// doResourceRequest is the generic method to prepare and send Pinterest Resource API requests
func (c *PinterestClient) doResourceRequest(ctx context.Context, resourceName, action string, options interface{}, sourceURL string, csrfToken string) (io.ReadCloser, error) {
	resourceURL := fmt.Sprintf("%s/resource/%s/%s/", baseURL, resourceName, action)

	dataObj := struct {
		Options interface{} `json:"options"`
		Context struct{}    `json:"context"`
	}{
		Options: options,
		Context: struct{}{},
	}

	dataBytes, err := sonic.Marshal(dataObj)
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("data", string(dataBytes))
	if sourceURL != "" {
		params.Set("source_url", sourceURL)
	}

	finalURL := resourceURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", finalURL, nil)
	if err != nil {
		return nil, err
	}

	// Set headers
	req.Header.Set("Accept", "application/json, text/javascript, */*, q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", baseURL+"/")

	if version := c.getAppVersion(ctx); version != "" {
		req.Header.Set("X-App-Version", version)
	}

	token := csrfToken
	if token == "" {
		if cachedToken, err := c.GetCSRFToken(ctx); err == nil && cachedToken != "" {
			token = cachedToken
		}
	}
	
	if token != "" {
		req.Header.Set("X-CSRFToken", token)
		req.Header.Add("Cookie", "csrftoken="+token)
	}

	req.Header.Set("X-Pinterest-AppState", "active")
	if sourceURL != "" {
		req.Header.Set("X-Pinterest-Source-Url", sourceURL)
	}

	// Set PWS Handler header based on resource
	handler := ""
	switch resourceName {
	case "BaseSearchResource":
		handler = "www/search/[scope].js"
	case "PinResource":
		handler = "www/pin/[id].js"
	case "UserResource":
		handler = "www/user/[username].js"
	case "BoardsFeedResource", "UserActivityPinsResource", "UserPinsResource":
		handler = "www/user/[username].js"
	case "BoardResource":
		handler = "www/board/[owner]/[slug].js"
	case "BoardFeedResource":
		handler = "www/board/[owner]/[slug].js"
	case "UnifiedCommentsResource":
		handler = "www/pin/[id].js"
	case "InterestResource":
		handler = "www/ideas/[interest]/[id].js"
	case "BestPinsFeedAltResource":
		handler = "www/ideas/[interest]/[id].js"
	}
	if handler != "" {
		req.Header.Set("X-Pinterest-PWS-Handler", handler)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bodyStr := string(body)
		if len(bodyStr) > 200 {
			bodyStr = bodyStr[:200] + "..."
		}
		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, bodyStr)
	}

	return resp.Body, nil
}

// CallResource reads the entire response body into memory
func (c *PinterestClient) CallResource(ctx context.Context, resourceName, action string, options interface{}, sourceURL string, csrfToken string) ([]byte, error) {
	bodyReader, err := c.doResourceRequest(ctx, resourceName, action, options, sourceURL, csrfToken)
	if err != nil {
		return nil, err
	}
	defer bodyReader.Close()
	return io.ReadAll(bodyReader)
}

// CallResourceStream decodes the response body directly into dest without loading it all into memory
func (c *PinterestClient) CallResourceStream(ctx context.Context, resourceName, action string, options interface{}, sourceURL string, csrfToken string, dest interface{}) error {
	bodyReader, err := c.doResourceRequest(ctx, resourceName, action, options, sourceURL, csrfToken)
	if err != nil {
		return err
	}
	defer bodyReader.Close()
	return sonic.ConfigDefault.NewDecoder(bodyReader).Decode(dest)
}

func (c *PinterestClient) Search(ctx context.Context, query string, scope string, bookmark string, csrfToken string) (*SearchResult, error) {
	if scope == "" {
		scope = "pins"
	}

	options := struct {
		Query                    string   `json:"query"`
		Scope                    string   `json:"scope"`
		NoFetchContextOnResource bool     `json:"no_fetch_context_on_resource"`
		Bookmarks                []string `json:"bookmarks,omitempty"`
	}{
		Query:                    query,
		Scope:                    scope,
		NoFetchContextOnResource: false,
	}
	if bookmark != "" {
		options.Bookmarks = []string{bookmark}
	}

	sourceURL := "/search/" + scope + "/?q=" + url.QueryEscape(query)
	var pResp pinterestResponse
	if err := c.CallResourceStream(ctx, "BaseSearchResource", "get", options, sourceURL, csrfToken, &pResp); err != nil {
		return nil, err
	}

	res := &SearchResult{
		Bookmark: pResp.ResourceResponse.Bookmark,
	}

	switch scope {
	case "pins", "videos":
		media := make([]MediaItem, 0)
		for _, r := range pResp.ResourceResponse.Data.Results {
			if isAdOrProduct(&r) {
				continue
			}
			media = append(media, extractMediaItem(&r))
		}
		res.Media = media
	case "boards":
		boards := make([]Board, 0)
		for _, r := range pResp.ResourceResponse.Data.Results {
			boards = append(boards, extractBoard(&r))
		}
		res.Boards = boards
	case "users":
		users := make([]UserProfile, 0)
		for _, r := range pResp.ResourceResponse.Data.Results {
			users = append(users, extractUserProfile(&r))
		}
		res.Users = users
	}

	token, _ := c.GetCSRFToken(ctx)
	res.CSRFToken = token

	return res, nil
}

func extractBoard(b *pinterestResult) Board {
	thumb := b.ImageCoverURL
	thumbFallback := b.ImageCoverURL
	if thumb == "" {
		img := extractImageWithFallback(b.Images)
		if img.Original.URL != "" {
			thumb = img.Original.URL
			thumbFallback = img.Fallback.URL
		}
	}

	name := b.BoardTitle
	if name == "" {
		name = b.Name
	}
	if name == "" {
		name = string(b.Title)
	}

	return Board{
		ID:                b.ID,
		Name:              name,
		URL:               b.URL,
		PinCount:          b.PinCount,
		Thumbnail:         thumb,
		ThumbnailFallback: thumbFallback,
		Description:       b.Description,
	}
}

func extractUserProfile(r *pinterestResult) UserProfile {
	username := r.Username
	if username == "" && r.Pinner != nil {
		username = r.Pinner.Username
	}

	fullName := r.FullName
	if fullName == "" && r.Pinner != nil {
		fullName = r.Pinner.FullName
	}

	avatar := r.ImageMediumURL
	if avatar == "" && r.Pinner != nil {
		avatar = r.Pinner.ImageMediumURL
	}

	return UserProfile{
		Username:       username,
		FullName:       fullName,
		AvatarURL:      avatar,
		AvatarFallback: avatar,
		FollowerCount:  r.FollowerCount,
		About:          r.About,
	}
}


func (c *PinterestClient) GetPin(ctx context.Context, pinID string, csrfToken string) (*MediaItem, error) {
	options := struct {
		FieldSetKey string `json:"field_set_key"`
		ID          string `json:"id"`
	}{
		FieldSetKey: "detailed",
		ID:          pinID,
	}

	var pResp pinterestResponse
	if err := c.CallResourceStream(ctx, "PinResource", "get", options, "/pin/"+pinID+"/", csrfToken, &pResp); err != nil {
		return nil, err
	}

	item := extractMediaItem(&pResp.ResourceResponse.Data)
	return &item, nil
}

func (c *PinterestClient) GetUser(ctx context.Context, username string, csrfToken string) (*UserProfile, error) {
	options := map[string]interface{}{
		"username": username,
		"field_set_key": "profile",
	}

	body, err := c.CallResource(ctx, "UserResource", "get", options, "/"+username+"/", csrfToken)
	if err != nil {
		// FALLBACK: If 403 or other error, try fetching the HTML directly
		htmlBody, hErr := c.fetchHTML(ctx, "/"+username+"/")
		if hErr == nil {
			profile, pErr := c.extractUserFromHTML(htmlBody, username)
			if pErr == nil {
				return profile, nil
			}
		}
		return nil, err
	}

	type userResp struct {
		Username       string `json:"username"`
		FullName       string `json:"full_name"`
		ImageMediumURL string `json:"image_medium_url"`
		ImageLargeURL  string `json:"image_large_url"`
		FollowerCount  int    `json:"follower_count"`
		FollowingCount int    `json:"following_count"`
		BoardCount     int    `json:"board_count"`
		BoardCountAlt  int    `json:"boardCount"`
		About          string `json:"about"`
	}

	var pResp struct {
		ResourceResponse struct {
			Data userResp `json:"data"`
		} `json:"resource_response"`
	}

	if err := sonic.Unmarshal(body, &pResp); err != nil || pResp.ResourceResponse.Data.Username == "" {
		// Fallback: try unmarshaling Data directly if resource_response is missing
		var fallback struct {
			Data userResp `json:"data"`
		}
		if err2 := sonic.Unmarshal(body, &fallback); err2 == nil && fallback.Data.Username != "" {
			pResp.ResourceResponse.Data = fallback.Data
		} else {
			// Try even flatter structure
			var raw userResp
			if err3 := sonic.Unmarshal(body, &raw); err3 == nil && raw.Username != "" {
				pResp.ResourceResponse.Data = raw
			}
		}
	}

	d := pResp.ResourceResponse.Data
	if d.Username == "" {
		// One last attempt: maybe it's just a username string or we can at least return some data
		if d.FullName == "" {
			return nil, fmt.Errorf("user metadata not found for %s", username)
		}
		d.Username = username
	}

	avatar := d.ImageLargeURL
	if avatar == "" {
		avatar = d.ImageMediumURL
	}

	avatarFallback := avatar
	isHEIC := strings.HasSuffix(strings.ToLower(avatar), ".heic")
	if isHEIC {
		// Pinterest doesn't always provide simple fallback URLs for avatars in the same way as pins,
		// but we store the original for now. Most avatars are JPEGs.
	}

	boardCount := d.BoardCount
	if boardCount == 0 {
		boardCount = d.BoardCountAlt
	}

	return &UserProfile{
		Username:       d.Username,
		FullName:       d.FullName,
		AvatarURL:      avatar,
		AvatarFallback: avatarFallback,
		FollowerCount:  d.FollowerCount,
		FollowingCount: d.FollowingCount,
		BoardCount:     boardCount,
		About:          d.About,
	}, nil
}

func (c *PinterestClient) GetBoard(ctx context.Context, username, slug string) (*Board, error) {
	// First, try direct BoardResource (most efficient/robust)
	options := map[string]interface{}{
		"username":      username,
		"slug":          slug,
		"field_set_key": "detailed",
	}

	body, err := c.CallResource(ctx, "BoardResource", "get", options, "/"+username+"/"+slug+"/", "")
	if err == nil {
		var pResp struct {
			ResourceResponse struct {
				Data pinterestResult `json:"data"`
			} `json:"resource_response"`
		}
		if err := sonic.Unmarshal(body, &pResp); err == nil && pResp.ResourceResponse.Data.ID != "" {
			b := pResp.ResourceResponse.Data
			
			thumb := b.ImageCoverURL
			thumbFallback := b.ImageCoverURL
			if thumb == "" {
				img := extractImageWithFallback(b.Images)
				if img.Original.URL != "" {
					thumb = img.Original.URL
					thumbFallback = img.Fallback.URL
				}
			}

			name := b.BoardTitle
			if name == "" {
				name = b.Name
			}
			if name == "" {
				name = string(b.Title)
			}

			return &Board{
				ID:          b.ID,
				Name:        name,
				URL:         b.URL,
				Slug:        slug,
				PinCount:          b.PinCount,
				Thumbnail:         thumb,
				ThumbnailFallback: thumbFallback,
				Description:       b.Description,
			}, nil
		}
	}

	// Fallback to finding the board in the user's board feed to get the ID
	boards, _, err := c.GetBoards(ctx, username, "", "")
	if err == nil {
		for _, b := range boards {
			// Check if slug matches or URL ends with /slug/
			boardSlug := b.Slug
			if boardSlug == "" {
				parts := strings.Split(strings.Trim(b.URL, "/"), "/")
				if len(parts) > 0 {
					boardSlug = parts[len(parts)-1]
				}
			}
			if boardSlug == slug {
				return &b, nil
			}
		}
	}

	return nil, fmt.Errorf("could not resolve board ID for %s/%s", username, slug)
}

func (c *PinterestClient) GetUserCreatedPins(ctx context.Context, username string, bookmark string, csrfToken string) (*SearchResult, error) {
	options := map[string]interface{}{
		"username":            username,
		"is_own_profile_pins": false,
		"field_set_key":       "profile_created_grid_item",
		"exclude_add_pin_rep": true,
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}

	sourceURL := "/" + username + "/_created/"
	body, err := c.CallResource(ctx, "UserActivityPinsResource", "get", options, sourceURL, csrfToken)
	if err != nil {
		return nil, err
	}

	var pResp struct {
		ResourceResponse struct {
			Data     []pinterestResult `json:"data"`
			Bookmark string            `json:"bookmark"`
		} `json:"resource_response"`
	}
	if err := sonic.Unmarshal(body, &pResp); err != nil {
		return nil, err
	}

	res := &SearchResult{
		Bookmark: pResp.ResourceResponse.Bookmark,
	}

	media := make([]MediaItem, 0)
	for _, r := range pResp.ResourceResponse.Data {
		if isAdOrProduct(&r) {
			continue
		}
		media = append(media, extractMediaItem(&r))
	}
	res.Media = media
	res.CSRFToken, _ = c.GetCSRFToken(ctx)

	return res, nil
}

func (c *PinterestClient) GetUserSavedPins(ctx context.Context, username string, bookmark string, csrfToken string) (*SearchResult, error) {
	options := map[string]interface{}{
		"username":            username,
		"is_own_profile_pins": false,
		"field_set_key":       "mobile_grid_item",
		"add_vase":            true,
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}

	sourceURL := "/" + username + "/_saved/"
	body, err := c.CallResource(ctx, "UserPinsResource", "get", options, sourceURL, csrfToken)
	if err != nil {
		return nil, err
	}

	var pResp struct {
		ResourceResponse struct {
			Data     []pinterestResult `json:"data"`
			Bookmark string            `json:"bookmark"`
		} `json:"resource_response"`
	}
	if err := sonic.Unmarshal(body, &pResp); err != nil {
		return nil, err
	}

	res := &SearchResult{
		Bookmark: pResp.ResourceResponse.Bookmark,
	}

	media := make([]MediaItem, 0)
	for _, r := range pResp.ResourceResponse.Data {
		if isAdOrProduct(&r) {
			continue
		}
		media = append(media, extractMediaItem(&r))
	}
	res.Media = media
	res.CSRFToken, _ = c.GetCSRFToken(ctx)

	return res, nil
}

func (c *PinterestClient) GetBoards(ctx context.Context, username string, bookmark string, csrfToken string) ([]Board, string, error) {
	options := map[string]interface{}{
		"username": username,
		"field_set_key": "profile_grid_item",
		"page_size": 25,
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}

	body, err := c.CallResource(ctx, "BoardsFeedResource", "get", options, "/"+username+"/", csrfToken)
	if err != nil {
		// FALLBACK: If 403 or other error, try fetching the HTML directly
		htmlBody, hErr := c.fetchHTML(ctx, "/"+username+"/")
		if hErr == nil {
			boards, bErr := c.extractBoardsFromHTML(htmlBody)
			if bErr == nil {
				return boards, "", nil
			}
		}
		return nil, "", err
	}

	var pResp struct {
		ResourceResponse struct {
			Data     []pinterestResult `json:"data"`
			Bookmark string            `json:"bookmark"`
		} `json:"resource_response"`
	}

	if err := sonic.Unmarshal(body, &pResp); err != nil || len(pResp.ResourceResponse.Data) == 0 {
		// Fallback: try unmarshaling Data directly if resource_response is missing
		var fallback struct {
			Data []pinterestResult `json:"data"`
		}
		if err2 := sonic.Unmarshal(body, &fallback); err2 == nil && len(fallback.Data) > 0 {
			pResp.ResourceResponse.Data = fallback.Data
		} else {
			// Try even flatter structure
			var raw []pinterestResult
			if err3 := sonic.Unmarshal(body, &raw); err3 == nil && len(raw) > 0 {
				pResp.ResourceResponse.Data = raw
			} else if err != nil {
				return nil, "", err
			}
		}
	}

	boards := make([]Board, 0)
	for _, b := range pResp.ResourceResponse.Data {
		thumb := b.ImageCoverURL
		thumbFallback := b.ImageCoverURL
		if thumb == "" {
			img := extractImageWithFallback(b.Images)
			if img.Original.URL != "" {
				thumb = img.Original.URL
				thumbFallback = img.Fallback.URL
			}
		}

		u := b.URL
		slug := ""
		if u != "" {
			parts := strings.Split(strings.Trim(u, "/"), "/")
			if len(parts) > 0 {
				slug = parts[len(parts)-1]
			}
		}

		name := b.BoardTitle
		if name == "" {
			name = b.Name
		}
		if name == "" {
			name = string(b.Title)
		}

		boards = append(boards, Board{
			ID:          b.ID,
			Name:        name,
			URL:         u,
			Slug:        slug,
			PinCount:    b.PinCount,
			Thumbnail:   thumb,
			ThumbnailFallback: thumbFallback,
			Description: b.Description,
		})
	}

	return boards, pResp.ResourceResponse.Bookmark, nil
}

func (c *PinterestClient) GetBoardPins(ctx context.Context, username, slug, bookmark string, csrfToken string) (*SearchResult, error) {
	board, err := c.GetBoard(ctx, username, slug)
	if err != nil {
		return nil, err
	}

	options := map[string]interface{}{
		"board_id":             board.ID,
		"field_set_key":        "react_grid_pin",
		"add_vase":             true,
		"filter_section_pins":  true,
		"is_react":             true,
		"prepend":              false,
		"page_size":            25,
		"redux_normalize_feed": true,
		"layout":               "default",
		"sort":                 "default",
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}

	body, err := c.CallResource(ctx, "BoardFeedResource", "get", options, "/"+username+"/"+slug+"/", csrfToken)
	if err != nil {
		return nil, err
	}

	var pResp struct {
		ResourceResponse struct {
			Data     []pinterestResult `json:"data"`
			Bookmark string            `json:"bookmark"`
		} `json:"resource_response"`
	}

	if err := sonic.Unmarshal(body, &pResp); err != nil || (len(pResp.ResourceResponse.Data) == 0 && bookmark != "") {
		unmarshalErr := err
		if unmarshalErr == nil && len(pResp.ResourceResponse.Data) == 0 {
			unmarshalErr = fmt.Errorf("no data in response")
		}

		// Fallback for single object vs array, or redux_normalize_feed structure variations
		var fallback pinterestResponse
		if err2 := sonic.Unmarshal(body, &fallback); err2 == nil {
			media := make([]MediaItem, 0)
			results := fallback.ResourceResponse.Data.Results
			
			for _, res := range results {
				if isAdOrProduct(&res) {
					continue
				}
				media = append(media, extractMediaItem(&res))
			}
			
			// If still empty but we have ResourceResponse.Data being an array directly
			if len(media) == 0 {
				var arrayResp struct {
					ResourceResponse struct {
						Data []pinterestResult `json:"data"`
						Bookmark string `json:"bookmark"`
					} `json:"resource_response"`
				}
				if err3 := sonic.Unmarshal(body, &arrayResp); err3 == nil && len(arrayResp.ResourceResponse.Data) > 0 {
					for _, res := range arrayResp.ResourceResponse.Data {
						if isAdOrProduct(&res) {
							continue
						}
						media = append(media, extractMediaItem(&res))
					}
					token, _ := c.GetCSRFToken(ctx)
					return &SearchResult{
						Media:     media,
						Board:     board,
						Bookmark:  arrayResp.ResourceResponse.Bookmark,
						CSRFToken: token,
					}, nil
				}
			}

			token, _ := c.GetCSRFToken(ctx)
			return &SearchResult{
				Media:     media,
				Board:     board,
				Bookmark:  fallback.ResourceResponse.Bookmark,
				CSRFToken: token,
			}, nil
		}
		return nil, unmarshalErr
	}

	media := make([]MediaItem, 0)
	for _, res := range pResp.ResourceResponse.Data {
		if isAdOrProduct(&res) {
			continue
		}
		media = append(media, extractMediaItem(&res))
	}

	token, _ := c.GetCSRFToken(ctx)

	return &SearchResult{
		Media:     media,
		Board:     board,
		Bookmark:  pResp.ResourceResponse.Bookmark,
		CSRFToken: token,
	}, nil
}

func (c *PinterestClient) GetInterestPins(ctx context.Context, slug string, interestID string, bookmark string, csrfToken string) (*SearchResult, error) {
	options := map[string]interface{}{
		"interest":      interestID,
		"field_set_key": "unauth_react",
		"add_vase":      true,
		"img_nii":       true,
		"override_ids":  []string{},
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}

	sourceURL := "/ideas/" + slug + "/" + interestID + "/"
	if slug == "" {
		sourceURL = "/ideas/hub/" + interestID + "/"
	}

	body, err := c.CallResource(ctx, "BestPinsFeedAltResource", "get", options, sourceURL, csrfToken)
	if err != nil {
		return nil, err
	}

	var pResp struct {
		ResourceResponse struct {
			Data     []pinterestResult `json:"data"`
			Bookmark string            `json:"bookmark"`
		} `json:"resource_response"`
	}

	if err := sonic.Unmarshal(body, &pResp); err != nil {
		return nil, err
	}

	media := make([]MediaItem, 0)
	for _, res := range pResp.ResourceResponse.Data {
		if isAdOrProduct(&res) {
			continue
		}
		media = append(media, extractMediaItem(&res))
	}

	token, _ := c.GetCSRFToken(ctx)

	return &SearchResult{
		Media:     media,
		Bookmark:  pResp.ResourceResponse.Bookmark,
		CSRFToken: token,
	}, nil
}

func (c *PinterestClient) GetComments(ctx context.Context, pinID string, aggregatedPinID string, bookmark string, csrfToken string) (*CommentResult, error) {
	targetID := aggregatedPinID
	if targetID == "" {
		pin, err := c.GetPin(ctx, pinID, csrfToken)
		if err == nil && pin.AggregatedPinID != "" {
			targetID = pin.AggregatedPinID
		} else {
			targetID = pinID
		}
	}

	options := map[string]interface{}{
		"aggregated_pin_id":    targetID,
		"page_size":            10,
		"redux_normalize_feed": true,
		"is_reversed":          false,
		"comment_featured_ids": []string{},
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}

	sourceURL := "/pin/" + pinID + "/"
	body, err := c.CallResource(ctx, "UnifiedCommentsResource", "get", options, sourceURL, csrfToken)
	if err != nil && aggregatedPinID != "" && aggregatedPinID != pinID {
		options["aggregated_pin_id"] = pinID
		body, err = c.CallResource(ctx, "UnifiedCommentsResource", "get", options, sourceURL, csrfToken)
	}

	if err != nil {
		if strings.Contains(err.Error(), "Aggregated pin data could not be found") {
			return &CommentResult{
				Comments:        []Comment{},
				AggregatedPinID: targetID,
			}, nil
		}
		return nil, err
	}

	var pResp struct {
		ResourceResponse struct {
			Data []struct {
				ID             string         `json:"id"`
				Type           string         `json:"type"`
				Text           string         `json:"text"`
				Details        string         `json:"details"`
				CreatedAt      string         `json:"created_at"`
				CommentCount   int            `json:"comment_count"`
				LikeCount      int            `json:"like_count"`
				ReactionCounts map[string]int `json:"reaction_counts"`
				User           struct {
					FullName       string `json:"full_name"`
					ImageMediumURL string `json:"image_medium_url"`
					Username       string `json:"username"`
				} `json:"user"`
				Images []struct {
					Originals struct {
						URL    string `json:"url"`
						Width  int    `json:"width"`
						Height int    `json:"height"`
					} `json:"originals"`
				} `json:"images"`
			} `json:"data"`
			Bookmark string `json:"bookmark"`
		} `json:"resource_response"`
	}

	if err := sonic.Unmarshal(body, &pResp); err != nil {
		// Fallback for different data layouts
		var fallback struct {
			ResourceResponse struct {
				Data struct {
					Results []struct {
						ID   string `json:"id"`
						Text string `json:"text"`
						User struct {
							FullName string `json:"full_name"`
							Username string `json:"username"`
						} `json:"user"`
					} `json:"results"`
				} `json:"data"`
				Bookmark string `json:"bookmark"`
			} `json:"resource_response"`
		}
		if err2 := sonic.Unmarshal(body, &fallback); err2 == nil && len(fallback.ResourceResponse.Data.Results) > 0 {
			comments := make([]Comment, 0)
			for _, rc := range fallback.ResourceResponse.Data.Results {
				author := rc.User.FullName
				if author == "" {
					author = rc.User.Username
				}
				comments = append(comments, Comment{
					ID:         rc.ID,
					Text:       rc.Text,
					AuthorName: author,
				})
			}
			return &CommentResult{
				Comments:        comments,
				Bookmark:        fallback.ResourceResponse.Bookmark,
				AggregatedPinID: targetID,
			}, nil
		}
		return nil, err
	}

	comments := make([]Comment, 0)
	for _, rc := range pResp.ResourceResponse.Data {
		author := rc.User.FullName
		if author == "" {
			author = rc.User.Username
		}
		likes := rc.LikeCount
		if likes == 0 && rc.ReactionCounts != nil {
			likes = rc.ReactionCounts["1"]
		}

		text := rc.Text
		if text == "" && rc.Type == "userdiditdata" {
			text = rc.Details
		}

		var imgs []CommentImage
		for _, img := range rc.Images {
			imgs = append(imgs, CommentImage{
				URL:    img.Originals.URL,
				Width:  img.Originals.Width,
				Height: img.Originals.Height,
			})
		}

		comments = append(comments, Comment{
			ID:             rc.ID,
			Type:           rc.Type,
			Text:           text,
			Details:        rc.Details,
			AuthorName:     author,
			CreatedAt:      rc.CreatedAt,
			Likes:          likes,
			ReplyCount:     rc.CommentCount,
			AuthorUsername: rc.User.Username,
			AuthorAvatar:   rc.User.ImageMediumURL,
			Images:         imgs,
		})
	}

	return &CommentResult{
		Comments:        comments,
		Bookmark:        pResp.ResourceResponse.Bookmark,
		AggregatedPinID: targetID,
	}, nil
}

func isAdOrProduct(res *pinterestResult) bool {
	if res.IsPromoted {
		return true
	}
	if res.RichSummary != nil && res.RichSummary.TypeName == "product" {
		return true
	}
	if res.Method == "catalog_bulk_create" {
		return true
	}
	img := extractImageWithFallback(res.Images)
	// Skip "empty" items (no ID or no image)
	if (res.EntityId == "" && res.ID == "") || img.Original.URL == "" {
		return true
	}
	return false
}

func extractMediaItem(result *pinterestResult) MediaItem {
	item := MediaItem{}

	if result.EntityId != "" {
		item.PinID = result.EntityId
	} else if result.ID != "" {
		item.PinID = result.ID
	}

	item.CommentsDisabled = result.CommentsDisabled || result.CommentsDisabledAlt

	item.Description = result.Description
	if string(result.Title) != "" {
		item.Title = string(result.Title)
	} else if result.GridTitle != "" {
		item.Title = result.GridTitle
	}

	if result.CloseupAttribution != nil && result.CloseupAttribution.FullName != "" {
		item.AuthorName = result.CloseupAttribution.FullName
		item.AuthorAvatar = result.CloseupAttribution.ImageMediumURL
		item.AuthorAvatarFallback = item.AuthorAvatar
		// We don't direct username in CloseupAttribution in our model yet, but let's check Pinner if needed
		if result.Pinner != nil {
			item.AuthorUsername = result.Pinner.Username
		}
	} else if result.Pinner != nil {
		item.AuthorName = result.Pinner.FullName
		if item.AuthorName == "" {
			item.AuthorName = result.Pinner.Username
		}
		item.AuthorUsername = result.Pinner.Username
		item.AuthorAvatar = result.Pinner.ImageMediumURL
		if item.AuthorAvatar == "" {
			item.AuthorAvatar = result.Pinner.ImageSmallURL
		}
		item.AuthorAvatarFallback = item.AuthorAvatar // Default to original
		if strings.HasSuffix(strings.ToLower(item.AuthorAvatar), ".heic") {
			// Heuristic: swap RS size for a JPEG version if possible, but Pinterest avatars
			// are usually JPEGs in these fields. Just in case, we keep it consistent.
		}
	}

	if result.AggregatedPinData != nil {
		item.AggregatedPinID = result.AggregatedPinData.ID
		if result.AggregatedPinData.Stats != nil {
			item.Saves = result.AggregatedPinData.Stats.Saves
			item.Comments = result.AggregatedPinData.Stats.Comments
			if result.AggregatedPinData.Stats.CommentCount > item.Comments {
				item.Comments = result.AggregatedPinData.Stats.CommentCount
			}
		}
	} else if result.AggregatedPinDataAlt != nil {
		item.AggregatedPinID = result.AggregatedPinDataAlt.ID
		if result.AggregatedPinDataAlt.Stats != nil {
			item.Saves = result.AggregatedPinDataAlt.Stats.Saves
			item.Comments = result.AggregatedPinDataAlt.Stats.Comments
			if result.AggregatedPinDataAlt.Stats.CommentCount > item.Comments {
				item.Comments = result.AggregatedPinDataAlt.Stats.CommentCount
			}
		}
	}

	if result.CommentCount > item.Comments {
		item.Comments = result.CommentCount
	}

	if result.RepinCount > 0 {
		item.Repins = result.RepinCount
	} else if result.RepinCountAlt > 0 {
		item.Repins = result.RepinCountAlt
	}

	if result.ReactionCounts != nil {
		if count, ok := result.ReactionCounts["1"]; ok {
			item.Likes = count
		}
	}

	img := extractImageWithFallback(result.Images)
	if img.Original.URL != "" {
		item.URL = img.Original.URL
		item.FallbackURL = img.Fallback.URL
		item.IsHEIC = img.IsHEIC
		item.ThumbnailURL = img.Original.URL
		item.Width = img.Original.Width
		item.Height = img.Original.Height
	}

	if result.StoryPinData != nil && len(result.StoryPinData.Pages) > 0 {
		for _, page := range result.StoryPinData.Pages {
			for _, block := range page.Blocks {
				if block.Video != nil && block.Video.VideoList != nil {
					item.IsVideo = true
					if result.StoryPinData.TotalVideoDuration > 0 {
						item.Duration = result.StoryPinData.TotalVideoDuration
					}
					item.processVideoList(block.Video.VideoList)
					return item
				}
			}
		}
	}

	if result.Videos != nil && result.Videos.VideoList != nil {
		item.IsVideo = true
		item.processVideoList(result.Videos.VideoList)
	}

	return item
}

func (item *MediaItem) processVideoList(vl *videoList) {
	var bestVariant *videoVariant
	// Prioritize HLS because Pinterest's direct MP4s often place the MOOV atom at the end of the file,
	// causing the browser to download the entire video before it can start playing.
	// We will solve the HLS polling issue by rewriting the master playlist in the proxy to only serve the highest quality subplaylist.
	variantsToTry := []string{
		"V_HLSV4", "V_HLSV3_MOBILE",
		"V_1080P", "V_1080P_MP4", "V_720P", "V_720P_MP4", "V_720W", 
		"V_480P", "V_480P_MP4", "V_480W", "V_360P", "V_360P_MP4", "V_360W", 
		"V_240P", "V_240P_MP4", "V_240W",
	}
	
	for _, key := range variantsToTry {
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
		if bestVariant.Duration > 0 && item.Duration == 0 {
			item.Duration = bestVariant.Duration
		}
	}
}

func (c *PinterestClient) fetchHTML(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.pinterest.com"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func toOriginalURL(u string) string {
	if u == "" || strings.Contains(u, "/originals/") {
		return u
	}
	// Pinterest thumbnail URLs usually have a size segment like /140x140_RS/ or /236x/
	// We can often swap these for /originals/
	re := regexp.MustCompile(`/(?:\d+x\d*(?:_RS)?)/`)
	return re.ReplaceAllString(u, "/originals/")
}

func (c *PinterestClient) extractUserFromHTML(html []byte, username string) (*UserProfile, error) {
	state, err := c.extractStateFromHTML(html)
	if err != nil {
		return nil, err
	}

	users, ok := state["users"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("users not found in state")
	}

	for _, udata := range users {
		um, ok := udata.(map[string]interface{})
		if !ok {
			continue
		}
		if um["username"] == username {
			// Map to UserProfile
			profile := &UserProfile{
				Username:       username,
				FullName:       fmt.Sprint(um["full_name"]),
				About:          fmt.Sprint(um["about"]),
				FollowerCount:  int(toInt64(um["follower_count"])),
				FollowingCount: int(toInt64(um["following_count"])),
				BoardCount:     int(toInt64(um["board_count"])),
			}
			avatar := fmt.Sprint(um["image_xlarge_url"])
			if avatar == "" || avatar == "<nil>" {
				// Try orig if images map exists
				if imap, ok := um["images"].(map[string]interface{}); ok {
					if orig, ok := imap["orig"].(map[string]interface{}); ok {
						avatar = fmt.Sprint(orig["url"])
					}
				}
			}
			if avatar == "" || avatar == "<nil>" {
				avatar = fmt.Sprint(um["image_medium_url"])
			}
			
			avatar = toOriginalURL(avatar)
			profile.AvatarURL = avatar
			profile.AvatarFallback = avatar
			return profile, nil
		}
	}

	return nil, fmt.Errorf("user %s not found in HTML state", username)
}

func (c *PinterestClient) extractBoardsFromHTML(html []byte) ([]Board, error) {
	state, err := c.extractStateFromHTML(html)
	if err != nil {
		return nil, err
	}

	boardsMap, ok := state["boards"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("boards not found in state")
	}

	var boards []Board
	for _, bdata := range boardsMap {
		bm, ok := bdata.(map[string]interface{})
		if !ok {
			continue
		}
		name := fmt.Sprint(bm["board_title"])
		if name == "" || name == "<nil>" {
			name = fmt.Sprint(bm["name"])
		}
		if name == "" || name == "<nil>" {
			name = fmt.Sprint(bm["title"])
		}
		board := Board{
			ID:          fmt.Sprint(bm["id"]),
			Name:        name,
			URL:         fmt.Sprint(bm["url"]),
			Description: fmt.Sprint(bm["description"]),
			PinCount:    int(toInt64(bm["pin_count"])),
		}
		if images, ok := bm["images"].(map[string]interface{}); ok {
			if orig, ok := images["orig"].(map[string]interface{}); ok {
				board.Thumbnail = fmt.Sprint(orig["url"])
				board.ThumbnailFallback = board.Thumbnail
			} else if wide, ok := images["136x136"].(map[string]interface{}); ok {
				board.Thumbnail = toOriginalURL(fmt.Sprint(wide["url"]))
				board.ThumbnailFallback = board.Thumbnail
			}
		}
		boards = append(boards, board)
	}

	return boards, nil
}

func (c *PinterestClient) extractStateFromHTML(html []byte) (map[string]interface{}, error) {
	sHTML := string(html)
	// Try __PWS_INITIAL_PROPS__ first
	match := regexp.MustCompile(`<script id="__PWS_INITIAL_PROPS__" type="application/json">(.*?)</script>`).FindStringSubmatch(sHTML)
	if len(match) > 1 {
		var data map[string]interface{}
		if err := sonic.Unmarshal([]byte(match[1]), &data); err == nil {
			if state, ok := data["initialReduxState"].(map[string]interface{}); ok {
				return state, nil
			}
		}
	}

	// Try __PWS_DATA__
	match = regexp.MustCompile(`<script id="__PWS_DATA__" type="application/json">(.*?)</script>`).FindStringSubmatch(sHTML)
	if len(match) > 1 {
		var data map[string]interface{}
		if err := sonic.Unmarshal([]byte(match[1]), &data); err == nil {
			if props, ok := data["props"].(map[string]interface{}); ok {
				if state, ok := props["initialReduxState"].(map[string]interface{}); ok {
					return state, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("initialReduxState not found in HTML")
}

func toInt64(v interface{}) int64 {
	switch val := v.(type) {
	case float64:
		return int64(val)
	case int64:
		return val
	case int:
		return int64(val)
	case string:
		i, _ := strconv.ParseInt(val, 10, 64)
		return i
	default:
		return 0
	}
}

type imageVariant struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type extractedImage struct {
	Original imageVariant
	Fallback imageVariant
	IsHEIC   bool
}

func extractImageWithFallback(images map[string]sonic.NoCopyRawMessage) extractedImage {
	orig := extractOrigImage(images)
	isHEIC := strings.HasSuffix(strings.ToLower(orig.URL), ".heic")

	if isHEIC {
		bestJPEG := findBestJPEGVariant(images)
		if bestJPEG.URL != "" {
			return extractedImage{
				Original: orig,
				Fallback: bestJPEG,
				IsHEIC:   true,
			}
		}
	}

	return extractedImage{
		Original: orig,
		Fallback: orig,
		IsHEIC:   isHEIC,
	}
}

func extractOrigImage(images map[string]sonic.NoCopyRawMessage) imageVariant {
	if raw, ok := images["orig"]; ok {
		var v imageVariant
		if err := sonic.Unmarshal(raw, &v); err == nil {
			return v
		}
	}
	
	// Fallback: If "orig" doesn't exist, find the highest resolution available
	return findBestVariant(images)
}

func findBestVariant(images map[string]sonic.NoCopyRawMessage) imageVariant {
	re := regexp.MustCompile(`^(\d+)x$`)
	var resolutions []int
	resMap := make(map[int]sonic.NoCopyRawMessage)

	for k, v := range images {
		matches := re.FindStringSubmatch(k)
		if len(matches) > 1 {
			res, _ := strconv.Atoi(matches[1])
			resolutions = append(resolutions, res)
			resMap[res] = v
		}
	}

	if len(resolutions) == 0 {
		return imageVariant{}
	}

	sort.Sort(sort.Reverse(sort.IntSlice(resolutions)))

	var v imageVariant
	if err := sonic.Unmarshal(resMap[resolutions[0]], &v); err == nil {
		return v
	}
	return imageVariant{}
}

func findBestJPEGVariant(images map[string]sonic.NoCopyRawMessage) imageVariant {
	re := regexp.MustCompile(`^(\d+)x$`)
	var resolutions []int
	resMap := make(map[int]sonic.NoCopyRawMessage)

	for k, v := range images {
		matches := re.FindStringSubmatch(k)
		if len(matches) > 1 {
			res, _ := strconv.Atoi(matches[1])
			resolutions = append(resolutions, res)
			resMap[res] = v
		}
	}

	if len(resolutions) == 0 {
		return imageVariant{}
	}

	sort.Sort(sort.Reverse(sort.IntSlice(resolutions)))

	for _, res := range resolutions {
		var v imageVariant
		if err := sonic.Unmarshal(resMap[res], &v); err == nil {
			if !strings.HasSuffix(strings.ToLower(v.URL), ".heic") {
				return v
			}
		}
	}

	return imageVariant{}
}
