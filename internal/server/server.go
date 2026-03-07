package server

import (
	"binternet-go/assets"
	"binternet-go/internal/cache"
	"binternet-go/internal/config"
	"binternet-go/internal/pinterest"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/vmihailenco/msgpack/v5"
)

// isAllowedProxyDomain checks if a host is allowed for proxying
func isAllowedProxyDomain(host string) bool {
	return host == "pinimg.com" ||
		strings.HasSuffix(host, ".pinimg.com") ||
		host == "pinterest.com" ||
		strings.HasSuffix(host, ".pinterest.com")
}

// CachedImage stores image data with its content type for correct cache retrieval
type CachedImage struct {
	ContentType string `msgpack:"ct"`
	Data        []byte `msgpack:"d"`
}

type Server struct {
	Router     *chi.Mux
	Config     *config.Config
	Client     *pinterest.PinterestClient
	Cache      cache.Cache
	Tmpl       *template.Template
	PreloadSem chan struct{}

	// Deduplicate in-flight preloads
	preloadInFlight sync.Map
}

func NewServer(cfg *config.Config) (*Server, error) {
	s := &Server{
		Router:     chi.NewRouter(),
		Config:     cfg,
		Client:     pinterest.NewClient(cfg.FallbackDNS),
		PreloadSem: make(chan struct{}, 50),
	}

	// Initialize Cache
	var err error
	s.Cache, err = cache.NewLayeredCache(
		cfg.MemoryCache, cfg.MemoryCacheLimit,
		cfg.DiskCache, cfg.DiskCacheLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cache: %w", err)
	}

	startupTime := time.Now().Unix()
	// Load Templates from Embed FS with custom functions
	funcMap := template.FuncMap{
		"formatDuration": func(ms int) string {
			secs := ms / 1000
			mins := secs / 60
			secs = secs % 60
			return fmt.Sprintf("%d:%02d", mins, secs)
		},
		"stripScheme": func(u string) string {
			return strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
		},
		"version": func() int64 {
			return startupTime
		},
	}
	s.Tmpl, err = template.New("").Funcs(funcMap).ParseFS(assets.AssetsFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse templates: %w", err)
	}

	s.routes()
	return s, nil
}

func (s *Server) routes() {
	// No middleware.Logger — only errors matter, stdout is expensive under load
	s.Router.Use(middleware.Recoverer)
	s.Router.Use(middleware.Compress(5, "text/html", "text/css", "application/json", "text/plain", "application/javascript"))
	s.Router.Use(s.SecurityHeadersMiddleware)

	// Static files from Embed FS — with 24-hour Cache-Control
	staticSubFS, _ := fs.Sub(assets.AssetsFS, "static")
	s.Router.Handle("/static/*", s.staticCacheHeaders(http.StripPrefix("/static/", http.FileServer(http.FS(staticSubFS)))))

	// Legacy static file support (misc/style-dark.css)
	s.Router.Get("/misc/*", func(w http.ResponseWriter, r *http.Request) {
		fsPath := "static/" + chi.URLParam(r, "*")

		data, err := assets.AssetsFS.ReadFile(fsPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")

		if strings.HasSuffix(fsPath, ".css") {
			w.Header().Set("Content-Type", "text/css")
		} else if strings.HasSuffix(fsPath, ".png") {
			w.Header().Set("Content-Type", "image/png")
		} else if strings.HasSuffix(fsPath, ".ico") {
			w.Header().Set("Content-Type", "image/x-icon")
		} else if strings.HasSuffix(fsPath, ".js") {
			w.Header().Set("Content-Type", "application/javascript")
		}

		w.Write(data)
	})

	s.Router.Get("/", s.handleIndex)
	s.Router.Get("/search.php", s.handleSearch)
	s.Router.Get("/api.php", s.handleAPI)
	s.Router.Get("/image_proxy.php", s.handleImageProxy)
	s.Router.Get("/video/*", s.handlePathProxy) // For transparent HLS proxying
	s.Router.Get("/legal", s.handleLegal)
}

func (s *Server) staticCacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Router.ServeHTTP(w, r)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.Tmpl.ExecuteTemplate(w, "index.html", nil)
}

func (s *Server) handleLegal(w http.ResponseWriter, r *http.Request) {
	s.Tmpl.ExecuteTemplate(w, "legal.html", nil)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	bookmark := r.URL.Query().Get("bookmark")
	csrfToken := r.URL.Query().Get("csrftoken")

	if query == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if len(query) > 64 {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	cacheKey := fmt.Sprintf("search:web:%s:%s", query, bookmark)
	var result *pinterest.SearchResult

	if cachedBytes, found := s.Cache.Get(cacheKey); found {
		if err := msgpack.Unmarshal(cachedBytes, &result); err == nil {
			// Cache hit
		}
	}

	if result == nil {
		var err error
		result, err = s.Client.SearchWeb(r.Context(), query, bookmark, csrfToken)
		if err != nil {
			s.handleError(w, http.StatusInternalServerError, "Search failed. Please try again later.", err)
			return
		}

		if serialized, err := msgpack.Marshal(result); err == nil {
			s.Cache.Set(cacheKey, serialized, 0)
		}
	}

	if s.Config.Preload && result.Bookmark != "" && result.Bookmark != "-end-" {
		s.triggerPreload("web", query, result.Bookmark, result.CSRFToken)
	}

	hasVideo := false
	for _, m := range result.Media {
		if m.IsVideo {
			hasVideo = true
			break
		}
	}

	data := struct {
		Query     string
		Images    []string
		Media     []pinterest.MediaItem
		Bookmark  string
		CSRFToken string
		HasVideo  bool
	}{
		Query:     query,
		Images:    result.Images,
		Media:     result.Media,
		Bookmark:  result.Bookmark,
		CSRFToken: result.CSRFToken,
		HasVideo:  hasVideo,
	}

	if s.Config.Preload && hasVideo {
		go s.prefetchM3U8s(result.Media)
	}

	s.Tmpl.ExecuteTemplate(w, "search.html", data)
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	bookmark := r.URL.Query().Get("bookmark")
	csrfToken := r.URL.Query().Get("csrftoken")

	cacheKey := fmt.Sprintf("search:api:%s:%s", query, bookmark)
	var result *pinterest.SearchResult

	if cachedBytes, found := s.Cache.Get(cacheKey); found {
		if err := msgpack.Unmarshal(cachedBytes, &result); err == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result)
			return
		}
	}

	var err error
	result, err = s.Client.SearchAPI(r.Context(), query, bookmark, csrfToken)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Internal Server Error"})
		log.Printf("API Error: %v", err)
		return
	}

	if serialized, err := msgpack.Marshal(result); err == nil {
		s.Cache.Set(cacheKey, serialized, 0)
	}

	if s.Config.Preload && result.Bookmark != "" && result.Bookmark != "-end-" {
		s.triggerPreload("api", query, result.Bookmark, result.CSRFToken)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) triggerPreload(mode, query, bookmark, csrfToken string) {
	preloadKey := fmt.Sprintf("%s:%s:%s", mode, query, bookmark)

	if _, loaded := s.preloadInFlight.LoadOrStore(preloadKey, struct{}{}); loaded {
		return
	}

	go func() {
		defer func() {
			s.preloadInFlight.Delete(preloadKey)
			if r := recover(); r != nil {
				log.Printf("Recovered from panic in preload: %v", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.preload(ctx, mode, query, bookmark, csrfToken)
	}()
}

func (s *Server) preload(ctx context.Context, mode, query, bookmark, csrfToken string) {
	var result *pinterest.SearchResult
	var err error
	var cacheKey string

	if mode == "web" {
		cacheKey = fmt.Sprintf("search:web:%s:%s", query, bookmark)
	} else {
		cacheKey = fmt.Sprintf("search:api:%s:%s", query, bookmark)
	}

	if cachedBytes, found := s.Cache.Get(cacheKey); found {
		if jsonErr := msgpack.Unmarshal(cachedBytes, &result); jsonErr != nil {
			result = nil
		}
	}

	if result == nil {
		if mode == "web" {
			result, err = s.Client.SearchWeb(ctx, query, bookmark, csrfToken)
		} else {
			result, err = s.Client.SearchAPI(ctx, query, bookmark, csrfToken)
		}

		if err != nil {
			return
		}

		if serialized, err := msgpack.Marshal(result); err == nil {
			s.Cache.Set(cacheKey, serialized, 0)
		}
	}

	// Preload Images
	if result != nil && len(result.Images) > 0 && s.Config.PreloadImages {
		go func(images []string) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Recovered from panic in preload images: %v", r)
				}
			}()

			for _, imgURL := range images {
				select {
				case s.PreloadSem <- struct{}{}:
					go func(imgurl string) {
						defer func() { <-s.PreloadSem }()
						defer func() {
							if r := recover(); r != nil {
								log.Printf("Recovered from panic in image fetch: %v", r)
							}
						}()

						if _, found := s.Cache.Get(imgurl); found {
							return
						}

						req, err := http.NewRequestWithContext(ctx, "GET", imgurl, nil)
						if err != nil {
							return
						}

						resp, err := s.Client.HTTPClient.Do(req)
						if err != nil {
							return
						}
						defer resp.Body.Close()

						if resp.StatusCode == http.StatusOK {
							body, err := io.ReadAll(resp.Body)
							if err == nil && len(body) > 0 {
								cached := CachedImage{
									ContentType: resp.Header.Get("Content-Type"),
									Data:        body,
								}
								if encoded, err := msgpack.Marshal(&cached); err == nil {
									s.Cache.Set(imgurl, encoded, 0)
								}
							}
						}
					}(imgURL)
				default:
					// Global semaphore full, skip this image
				}
			}
		}(result.Images)
	}
}

func (s *Server) prefetchM3U8s(media []pinterest.MediaItem) {
	for _, m := range media {
		if !m.IsVideo || m.VideoURL == "" {
			continue
		}

		go func(videoURL string) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Recovered from panic in prefetchM3U8s: %v", r)
				}
			}()

			select {
			case s.PreloadSem <- struct{}{}:
				defer func() { <-s.PreloadSem }()
			default:
				return // Global semaphore full, skip
			}

			if _, found := s.Cache.Get(videoURL); found {
				return
			}

			req, err := http.NewRequestWithContext(context.Background(), "GET", videoURL, nil)
			if err != nil {
				return
			}

			resp, err := s.Client.HTTPClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err == nil && len(body) > 0 {
					// Apply our highest-quality rewrite to the cached version too
					if strings.HasSuffix(videoURL, ".m3u8") {
						body = processM3U8(body)
					}
					
					cached := CachedImage{
						ContentType: resp.Header.Get("Content-Type"),
						Data:        body,
					}
					if encoded, err := msgpack.Marshal(&cached); err == nil {
						s.Cache.Set(videoURL, encoded, 0)
					}
				}
			}
		}(m.VideoURL)
	}
}

func (s *Server) handleError(w http.ResponseWriter, status int, userMsg string, logErr error) {
	if logErr != nil {
		log.Printf("Error: %v", logErr)
	}

	w.WriteHeader(status)
	data := struct {
		Message string
		Detail  string
	}{
		Message: userMsg,
	}

	if s.Config.ShowErrorsToClient && logErr != nil {
		data.Detail = logErr.Error()
	}

	s.Tmpl.ExecuteTemplate(w, "error.html", data)
}

func (s *Server) handleImageProxy(w http.ResponseWriter, r *http.Request) {
	imageURL := r.URL.Query().Get("url")
	if imageURL == "" {
		http.Error(w, "Missing URL", http.StatusBadRequest)
		return
	}

	u, err := url.Parse(imageURL)
	if err != nil {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	if !isAllowedProxyDomain(u.Host) {
		http.Error(w, "Domain not allowed", http.StatusForbidden)
		return
	}

	// Check cache — deserialize CachedImage to get correct content-type
	if s.Cache != nil {
		if data, found := s.Cache.Get(imageURL); found {
			var cached CachedImage
			if err := msgpack.Unmarshal(data, &cached); err == nil {
				w.Header().Set("Cache-Control", "public, max-age=3600")
				w.Header().Set("Content-Type", cached.ContentType)
				w.Write(cached.Data)
				return
			}
		}
	}

	// Fetch image
	resp, err := s.Client.HTTPClient.Get(imageURL)
	if err != nil {
		http.Error(w, "Image unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, "Image unavailable", resp.StatusCode)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Type", contentType)

	const maxBodySize = 15 * 1024 * 1024 // 15MB
	limitReader := io.LimitReader(resp.Body, maxBodySize)

	var cacheBuf bytes.Buffer
	if resp.ContentLength > 0 {
		cacheBuf.Grow(int(resp.ContentLength))
	}

	multiWriter := io.MultiWriter(w, &cacheBuf)

	_, err = io.Copy(multiWriter, limitReader)
	if err != nil {
		// Broken pipe / client disconnect — don't cache partial data
		return
	}

	// Async cache set with correct content-type
	if s.Cache != nil && cacheBuf.Len() > 0 {
		fullBody := make([]byte, cacheBuf.Len())
		copy(fullBody, cacheBuf.Bytes())

		go func(data []byte, ct string) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Recovered from panic in handleImageProxy cache set: %v", r)
				}
			}()
			cached := CachedImage{ContentType: ct, Data: data}
			if encoded, err := msgpack.Marshal(&cached); err == nil {
				s.Cache.Set(imageURL, encoded, 0)
			}
		}(fullBody, contentType)
	}
}

// handlePathProxy acts EXACTLY like handleImageProxy but takes the URL from the chi wildcard path
// This allows relative M3U8 chunk requests to transparently route through our proxy
func (s *Server) handlePathProxy(w http.ResponseWriter, r *http.Request) {
	targetPath := chi.URLParam(r, "*")
	if targetPath == "" {
		http.Error(w, "Missing path", http.StatusBadRequest)
		return
	}

	targetURL := "https://" + targetPath
	u, err := url.Parse(targetURL)
	if err != nil {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	if !isAllowedProxyDomain(u.Host) {
		http.Error(w, "Domain not allowed", http.StatusForbidden)
		return
	}

	// Check cache
	if s.Cache != nil {
		if data, found := s.Cache.Get(targetURL); found {
			var cached CachedImage
			if err := msgpack.Unmarshal(data, &cached); err == nil {
				w.Header().Set("Cache-Control", "public, max-age=3600")
				w.Header().Set("Content-Type", cached.ContentType)
				w.Header().Set("Access-Control-Allow-Origin", "*")

				// ServeContent natively handles Range requests for cached media
				http.ServeContent(w, r, "media", time.Time{}, bytes.NewReader(cached.Data))
				return
			}
		}
	}

	// Fetch media
	req, err := http.NewRequestWithContext(r.Context(), "GET", targetURL, nil)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Copy Range headers if browser is seeking through the video
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}

	resp, err := s.Client.HTTPClient.Do(req)
	if err != nil {
		http.Error(w, "Media unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		http.Error(w, "Media unavailable", resp.StatusCode)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Accept-Ranges", "bytes")

	isM3U8 := strings.HasSuffix(u.Path, ".m3u8")
	if isM3U8 && resp.StatusCode == http.StatusOK {
		m3u8Data, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "Failed to read playlist", http.StatusBadGateway)
			return
		}

		m3u8Data = processM3U8(m3u8Data)

		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(m3u8Data)))
		w.WriteHeader(resp.StatusCode)
		w.Write(m3u8Data)

		if s.Cache != nil {
			go func(data []byte, ct string) {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("Recovered from panic in m3u8 cache set: %v", r)
					}
				}()
				cached := CachedImage{ContentType: ct, Data: data}
				if encoded, err := msgpack.Marshal(&cached); err == nil {
					s.Cache.Set(targetURL, encoded, 0)
				}
			}(m3u8Data, contentType)
		}
		return
	}

	if contentRange := resp.Header.Get("Content-Range"); contentRange != "" {
		w.Header().Set("Content-Range", contentRange)
	}
	if contentLength := resp.Header.Get("Content-Length"); contentLength != "" {
		w.Header().Set("Content-Length", contentLength)
	}
	w.WriteHeader(resp.StatusCode)

	var cacheBuf bytes.Buffer
	// Only cache full OK responses under 50MB, not partial 206 responses
	const maxCacheSize = 50 * 1024 * 1024
	doCache := false
	if resp.StatusCode == http.StatusOK && s.Cache != nil && resp.ContentLength > 0 && resp.ContentLength <= maxCacheSize {
		doCache = true
		cacheBuf.Grow(int(resp.ContentLength))
	}

	var writer io.Writer = w
	if doCache {
		writer = io.MultiWriter(w, &cacheBuf)
	}

	// Stream the full response without LimitReader truncation so iOS Safari doesn't crash on large videos
	_, err = io.Copy(writer, resp.Body)
	if err != nil {
		// Broken pipe / client disconnect — don't cache partial data
		return
	}

	// Async cache set with correct content-type
	if doCache && cacheBuf.Len() > 0 && cacheBuf.Len() <= maxCacheSize {
		fullBody := make([]byte, cacheBuf.Len())
		copy(fullBody, cacheBuf.Bytes())

		go func(data []byte, ct string) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Recovered from panic in handlePathProxy cache set: %v", r)
				}
			}()
			cached := CachedImage{ContentType: ct, Data: data}
			if encoded, err := msgpack.Marshal(&cached); err == nil {
				s.Cache.Set(targetURL, encoded, 0)
			}
		}(fullBody, contentType)
	}
}

func FileServer(r chi.Router, path string, root http.FileSystem) {
	if strings.ContainsAny(path, "{}*") {
		panic("FileServer does not permit any URL parameters.")
	}

	if path != "/" && path[len(path)-1] != '/' {
		r.Get(path, http.RedirectHandler(path+"/", 301).ServeHTTP)
		path += "/"
	}
	path += "*"

	r.Get(path, func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.RouteContext(r.Context())
		pathPrefix := strings.TrimSuffix(rctx.RoutePattern(), "/*")
		fs := http.StripPrefix(pathPrefix, http.FileServer(root))
		fs.ServeHTTP(w, r)
	})
}

func (s *Server) SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		csp := "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; object-src 'none';"
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

func processM3U8(data []byte) []byte {
	// Check if it's a master playlist (has #EXT-X-STREAM-INF)
	if !bytes.Contains(data, []byte("#EXT-X-STREAM-INF")) {
		return data
	}

	lines := strings.Split(string(data), "\n")
	var headers []string

	type streamInfo struct {
		infLine string
		uriLine string
		bw      int
	}
	var streams []streamInfo

	var currentInf string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#EXT-X-STREAM-INF") {
			currentInf = trimmed
		} else if currentInf != "" && !strings.HasPrefix(trimmed, "#") {
			// Parse bandwidth
			bw := 0
			fmt.Sscanf(extractPlaylistAttr(currentInf, "BANDWIDTH"), "%d", &bw)
			streams = append(streams, streamInfo{infLine: currentInf, uriLine: trimmed, bw: bw})
			currentInf = ""
		} else {
			if currentInf == "" {
				headers = append(headers, trimmed)
			}
		}
	}

	if len(streams) == 0 {
		return data
	}

	// Find highest bandwidth
	highest := streams[0]
	for _, s := range streams {
		if s.bw > highest.bw {
			highest = s
		}
	}

	// Reconstruct
	var out []string
	out = append(out, headers...)
	out = append(out, highest.infLine)
	out = append(out, highest.uriLine)

	return []byte(strings.Join(out, "\n") + "\n")
}

func extractPlaylistAttr(line, attr string) string {
	idx := strings.Index(line, attr+"=")
	if idx == -1 {
		return ""
	}
	start := idx + len(attr) + 1
	end := strings.Index(line[start:], ",")
	if end == -1 {
		return line[start:]
	}
	return line[start : start+end]
}
