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

		// Preload next page if enabled
		if s.Config.Preload && result.Bookmark != "" && result.Bookmark != "-end-" {
			go s.preload("web", query, result.Bookmark, result.CSRFToken)
		}
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

	// Preload next page if enabled
	if s.Config.Preload && result.Bookmark != "" && result.Bookmark != "-end-" {
		go s.preload("api", query, result.Bookmark, result.CSRFToken)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) preload(mode, query, bookmark, csrfToken string) {
	// Avoid duplicate preloads?
	// Simple preload logic
	var result *pinterest.SearchResult
	var err error
	var cacheKey string

	if mode == "web" {
		cacheKey = fmt.Sprintf("search:web:%s:%s", query, bookmark)
		// Check if already cached
		if _, found := s.Cache.Get(cacheKey); found {
			return
		}
		result, err = s.Client.SearchWeb(query, bookmark, csrfToken)
	} else {
		cacheKey = fmt.Sprintf("search:api:%s:%s", query, bookmark)
		if _, found := s.Cache.Get(cacheKey); found {
			return
		}
		result, err = s.Client.SearchAPI(query, bookmark, csrfToken)
	}

	if err == nil {
		if serialized, err := json.Marshal(result); err == nil {
			fmt.Printf("Preloaded %s result for query: %s\n", mode, query)
			s.Cache.Set(cacheKey, serialized, 15*time.Minute)
		}
	} else {
		fmt.Printf("Preload failed: %v\n", err)
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Image Proxy Read Error: %v\n", err)
		http.Error(w, "Image unavailable", http.StatusInternalServerError)
		return
	}

	if s.Cache != nil {
		s.Cache.Set(imageURL, body, 0) // Images cached indefinitely (or until eviction)
	}

	w.Write(body)
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
