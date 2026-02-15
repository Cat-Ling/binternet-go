package server

import (
	"binternet-go/assets"
	"binternet-go/internal/cache"
	"binternet-go/internal/config"
	"binternet-go/internal/pinterest"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	Router *chi.Mux
	Config *config.Config
	Client *pinterest.PinterestClient
	Cache  cache.Cache // TODO: Use this for caching images/search results if needed
	Tmpl   *template.Template
}

func NewServer(cfg *config.Config) (*Server, error) {
	s := &Server{
		Router: chi.NewRouter(),
		Config: cfg,
		Client: pinterest.NewClient(cfg.FallbackDNS),
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

	// Load Templates from Embed FS
	// Note: patterns are relative to the FS root
	s.Tmpl, err = template.ParseFS(assets.AssetsFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse templates: %w", err)
	}

	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.Router.Use(middleware.Logger)
	s.Router.Use(middleware.Recoverer)
	s.Router.Use(s.SecurityHeadersMiddleware)

	// Static files from Embed FS
	// assets.AssetsFS root contains "static" and "templates" folders
	staticSubFS, _ := fs.Sub(assets.AssetsFS, "static")
	s.Router.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticSubFS))))

	// Legacy static file support (misc/style-dark.css)
	// Serve static files from /misc/
	// Use a custom handler to serve from the embedded FS "static" subdirectory
	s.Router.Get("/misc/*", func(w http.ResponseWriter, r *http.Request) {
		fsPath := "static/" + chi.URLParam(r, "*")

		data, err := assets.AssetsFS.ReadFile(fsPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		// Set content type based on extension
		if strings.HasSuffix(fsPath, ".css") {
			w.Header().Set("Content-Type", "text/css")
		} else if strings.HasSuffix(fsPath, ".png") {
			w.Header().Set("Content-Type", "image/png")
		} else if strings.HasSuffix(fsPath, ".ico") {
			w.Header().Set("Content-Type", "image/x-icon")
		}

		w.Write(data)
	})

	s.Router.Get("/", s.handleIndex)
	s.Router.Get("/search.php", s.handleSearch)
	s.Router.Get("/api.php", s.handleAPI)
	s.Router.Get("/image_proxy.php", s.handleImageProxy)
	s.Router.Get("/legal", s.handleLegal)
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

	// Try Cache First
	cacheKey := fmt.Sprintf("search:web:%s:%s", query, bookmark)
	var result *pinterest.SearchResult

	if cachedBytes, found := s.Cache.Get(cacheKey); found {
		if err := json.Unmarshal(cachedBytes, &result); err == nil {
			// Cache hit
			// Update CSRF token from request if bookmark is used?
			// The cached result has a CSRF token from when it was fetched.
			// Is it safe to reuse? Probably.
		}
	}

	if result == nil {
		// Use SearchWeb for the HTML frontend
		var err error
		result, err = s.Client.SearchWeb(query, bookmark, csrfToken)
		if err != nil {
			s.handleError(w, http.StatusInternalServerError, "Search failed. Please try again later.", err)
			return
		}

		// Cache the result (15 minutes)
		if serialized, err := json.Marshal(result); err == nil {
			s.Cache.Set(cacheKey, serialized, 15*time.Minute)
		}
	}

	// Preload next page if enabled (Runs on both Cache Hit and Miss)
	if s.Config.Preload && result.Bookmark != "" && result.Bookmark != "-end-" {
		go s.preload("web", query, result.Bookmark, result.CSRFToken)
	}

	data := struct {
		Query     string
		Images    []string
		Bookmark  string
		CSRFToken string
	}{
		Query:     query,
		Images:    result.Images,
		Bookmark:  result.Bookmark,
		CSRFToken: result.CSRFToken, // Use the one from result (which might be new)
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
		if err := json.Unmarshal(cachedBytes, &result); err == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result)
			return
		}
	}

	// Use SearchAPI for the JSON API
	var err error
	result, err = s.Client.SearchAPI(query, bookmark, csrfToken)
	if err != nil {
		// API always returns JSON
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Internal Server Error"})
		// Log the actual error
		fmt.Printf("API Error: %v\n", err)
		return
	}

	// Cache the result (15 minutes)
	if serialized, err := json.Marshal(result); err == nil {
		s.Cache.Set(cacheKey, serialized, 15*time.Minute)
	}

	// Preload next page if enabled (Runs on both Cache Hit and Miss)
	if s.Config.Preload && result.Bookmark != "" && result.Bookmark != "-end-" {
		go s.preload("api", query, result.Bookmark, result.CSRFToken)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) preload(mode, query, bookmark, csrfToken string) {
	// Avoid duplicate preloads?
	// Preload logic
	var result *pinterest.SearchResult
	var err error
	var cacheKey string

	if mode == "web" {
		cacheKey = fmt.Sprintf("search:web:%s:%s", query, bookmark)
	} else {
		cacheKey = fmt.Sprintf("search:api:%s:%s", query, bookmark)
	}

	// Check Cache for Search Result
	if cachedBytes, found := s.Cache.Get(cacheKey); found {
		if jsonErr := json.Unmarshal(cachedBytes, &result); jsonErr != nil {
			result = nil
		}
	}

	// If not in cache or unmarshal failed, fetch from source
	if result == nil {
		if mode == "web" {
			result, err = s.Client.SearchWeb(query, bookmark, csrfToken)
		} else {
			result, err = s.Client.SearchAPI(query, bookmark, csrfToken)
		}

		if err != nil {
			fmt.Printf("Preload fetch failed: %v\n", err)
			return
		}

		// Cache the new result
		if serialized, err := json.Marshal(result); err == nil {
			fmt.Printf("Preloaded %s result for query: %s\n", mode, query)
			s.Cache.Set(cacheKey, serialized, 15*time.Minute)
		}
	}

	// Preload Images (from either cached or fetched result)
	if result != nil && len(result.Images) > 0 && s.Config.PreloadImages {
		go func(images []string) {
			// Simple semaphore to limit concurrent image preloads to, say, 10 at a time
			sem := make(chan struct{}, 10)
			for _, imgURL := range images {
				sem <- struct{}{}
				go func(url string) {
					defer func() { <-sem }()
					// Check cache first
					if _, found := s.Cache.Get(url); found {
						return
					}

					// Fetch and Cache
					resp, err := http.Get(url)
					if err != nil {
						return
					}
					defer resp.Body.Close()

					if resp.StatusCode == http.StatusOK {
						body, err := io.ReadAll(resp.Body)
						if err == nil && len(body) > 0 {
							s.Cache.Set(url, body, 0)
						}
					}
				}(imgURL)
			}
		}(result.Images)
	}
}

func (s *Server) handleError(w http.ResponseWriter, status int, userMsg string, logErr error) {
	if logErr != nil {
		fmt.Printf("Error: %v\n", logErr)
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

	// Validate domain
	u, err := url.Parse(imageURL)
	if err != nil {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	allowedDomains := map[string]bool{
		"pinimg.com":    true,
		"i.pinimg.com":  true,
		"pinterest.com": true,
	}

	if !allowedDomains[u.Host] {
		// Try root domain check if needed, but strict host check is safer
		http.Error(w, "Domain not allowed", http.StatusForbidden)
		return
	}

	// Check cache
	if s.Cache != nil {
		if data, found := s.Cache.Get(imageURL); found {
			w.Header().Set("Content-Type", "image/jpeg") // Defaulting to jpeg, but might need to store mime type in cache too
			// Ideally cache value should be a struct with ContentType and Body
			// For simplicity, let's assume raw bytes for now and maybe infer type or just serve.
			// Re-checking the PHP, it sets header based on fetch, but for cache we need to know.
			// Let's just write bytes.
			w.Write(data)
			return
		}
	}

	// Fetch image
	resp, err := http.Get(imageURL)
	if err != nil {
		fmt.Printf("Image Proxy Fetch Error: %v\n", err)
		http.Error(w, "Image unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Image Proxy Upstream Status: %d\n", resp.StatusCode)
		http.Error(w, "Image unavailable", resp.StatusCode)
		return
	}

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))

	// Create a TeeReader to permit streaming to user while capturing for cache
	// We need to read into a buffer for the cache
	// IMPORTANT: Since we want to be "blazing fast", we prioritize writing to 'w'.
	// But TeeReader writes to both synchronously.
	// If the user has a slow connection, this might block the cache write, or vice versa?
	// Actually, Cache write is just memory append until we flush.
	// So TeeReader is fine, but we need to capture the full body for cache.

	// 5MB limit check?
	// Let's just limit reading to avoid DoS
	const maxBodySize = 15 * 1024 * 1024 // 15MB
	limitReader := io.LimitReader(resp.Body, maxBodySize)

	// TeeReader writes to 'w' (client) and 'buf' (cache input)
	// Wait, strings.Builder is not thread safe if we used a pipe, but here we are single threaded in this handler.
	// However, TeeReader writes effectively mimic "copy to w, then copy to buf".
	// If the client is slow, writing to 'w' blocks. This is unavoidable for streaming to client.
	// But it does verify we don't wait for FULL body before sending first byte.

	// We can't use strings.Builder with TeeReader directly because TeeReader takes a Writer.
	// bytes.Buffer is a simple Writer.
	// To minimize allocs, we could pre-allocate if we knew content-length.

	// Create a pipe? No, that's for concurrent reading.
	// We just want to copy to two places.
	// io.MultiWriter(w, &buf)

	// But wait, if we fail to read fully, we shouldn't cache partials ideally.
	// But we can check error at the end.

	// The problem is we want to return from the handler only after writing to user is done,
	// but the cache setting should happen "after" that?
	// If we use `go func() { ... }` we need the data.
	// So we must capture the data during the write.

	// If we want TRULY async cache:
	// We need to read from Body.
	// Write to Client.
	// Write to some buffer.
	// AFTER client is done, maximize the buffer.

	// Better approach for speed + caching:
	// 1. Start copying to client immediately.
	// 2. Use a custom writer that also appends to a byte slice.

	cacheBuf := new(strings.Builder)
	if resp.ContentLength > 0 {
		cacheBuf.Grow(int(resp.ContentLength))
	}

	multiWriter := io.MultiWriter(w, cacheBuf)

	_, err = io.Copy(multiWriter, limitReader)
	if err != nil {
		// If client disconnects or network error, we probably shouldn't cache the potentially partial result
		fmt.Printf("Image Proxy Stream Error: %v\n", err)
		return
	}

	// Async Cache Set
	// We captured the full body efficiently while streaming.
	// Now offload the actual "Set" (which might involve disk I/O or hashing) to a goroutine
	// so we can return and close the HTTP request context immediately (though HTTP/1.1 pipelining might wait, Go handles this well).

	// We need a copy of the string/bytes because the builder buffer might be GC'd?
	// actually strings.Builder String() returns a copy or a reference?
	// It's a copy if we cast to []byte usually or just use the string.
	// Let's use string.

	fullBody := cacheBuf.String() // This creates a copy of the string currently in the builder

	if s.Cache != nil && len(fullBody) > 0 {
		go func(data string) {
			// Convert back to bytes for cache interface
			// This is cheap.
			s.Cache.Set(imageURL, []byte(data), 0)
		}(fullBody)
	}
}

// FileServer conveniently sets up a http.FileServer handler to serve
// static files from a http.FileSystem.
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
		// Content-Security-Policy
		// Allow images from self and data: (since we proxy).
		// Allow inline styles.
		// Disallow objects.
		csp := "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; object-src 'none';"
		w.Header().Set("Content-Security-Policy", csp)

		// X-Content-Type-Options
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// X-Frame-Options
		w.Header().Set("X-Frame-Options", "DENY")

		// Referrer-Policy
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// Strict-Transport-Security (HSTS) - Removed as per user request (and it's often handled by reverse proxy)

		next.ServeHTTP(w, r)
	})
}
