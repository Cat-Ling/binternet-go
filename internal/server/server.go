package server

import (
	"github.com/flinternet/flinternet/assets"
	"github.com/flinternet/flinternet/internal/cache"
	"github.com/flinternet/flinternet/internal/config"
	"github.com/flinternet/flinternet/internal/pinterest"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

func encodeCachedMedia(contentType string, data []byte) []byte {
	ctLen := len(contentType)
	if ctLen > 255 {
		ctLen = 255
	}
	encoded := make([]byte, 1+ctLen+len(data))
	encoded[0] = byte(ctLen)
	copy(encoded[1:], contentType[:ctLen])
	copy(encoded[1+ctLen:], data)
	return encoded
}

func decodeCachedMedia(encoded []byte) (string, []byte, bool) {
	if len(encoded) < 1 {
		return "", nil, false
	}
	ctLen := int(encoded[0])
	if len(encoded) < 1+ctLen {
		return "", nil, false
	}
	contentType := string(encoded[1 : 1+ctLen])
	data := encoded[1+ctLen:]
	return contentType, data, true
}

type Server struct {
	Router     *chi.Mux
	Config     *config.Config
	Client     *pinterest.PinterestClient
	Cache      cache.Cache
	Tmpl       *template.Template
	PreloadSem chan struct{}
	Whitelist  []string

	// Deduplicate in-flight preloads
	preloadInFlight sync.Map
}

//go:embed whitelist.json
var whitelistJSON []byte

func NewServer(cfg *config.Config) (*Server, error) {
	s := &Server{
		Router:     chi.NewRouter(),
		Config:     cfg,
		Client:     pinterest.NewClient(cfg.FallbackDNS),
		PreloadSem: make(chan struct{}, 50),
	}

	if err := json.Unmarshal(whitelistJSON, &s.Whitelist); err != nil {
		return nil, fmt.Errorf("failed to load proxy whitelist: %w", err)
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
		"versionYear": func(v int64) int {
			return time.Unix(v, 0).Year()
		},
		"formatCount": func(n int) string {
			if n >= 1000000 {
				return fmt.Sprintf("%.1fM", float64(n)/1000000.0)
			}
			if n >= 1000 {
				return fmt.Sprintf("%.1fK", float64(n)/1000.0)
			}
			return fmt.Sprintf("%d", n)
		},
		"proxyURL": func(u string) string {
			parsed, err := url.Parse(u)
			if err != nil {
				return u
			}
			host := parsed.Host
			if !s.isAllowedProxyDomain(host) {
				return "/proxy/image/?url=" + url.QueryEscape(u)
			}

			parts := strings.Split(host, ".")
			if len(parts) < 2 {
				return "/proxy/image/?url=" + url.QueryEscape(u)
			}

			domain := parts[len(parts)-2]
			sub := strings.Join(parts[:len(parts)-2], ".")
			if sub == "" {
				sub = "www"
			}

			return "/" + domain + "/" + sub + parsed.Path
		},
		"getSlug": func(u string) string {
			u = strings.TrimSuffix(u, "/")
			parts := strings.Split(u, "/")
			if len(parts) > 0 {
				return parts[len(parts)-1]
			}
			return ""
		},
		"trimDescription": func(s string) string {
			s = strings.TrimSpace(s)
			// Replace multiple newlines with a single newline to prevent redundant "more" buttons
			for strings.Contains(s, "\n\n") {
				s = strings.ReplaceAll(s, "\n\n", "\n")
			}
			return s
		},
		"hasMore": func(s string) bool {
			s = strings.TrimSpace(s)
			// Pinterest descriptions are often long, but we only show "more" if it's over ~100 chars
			// or contains at least one newline after trimming.
			// This is a heuristic to match the user's request.
			return len(s) > 100 || strings.Contains(s, "\n")
		},
		"isHEIC": func(u string) bool {
			return strings.HasSuffix(strings.ToLower(u), ".heic") || strings.HasSuffix(strings.ToLower(u), ".heif")
		},
	}
	s.Tmpl, err = template.New("").Funcs(funcMap).ParseFS(assets.AssetsFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse templates: %w", err)
	}

	s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Router.ServeHTTP(w, r)
}

func (s *Server) isSafari(r *http.Request) bool {
	ua := r.Header.Get("User-Agent")
	// Safari UA contains "Safari" but NOT "Chrome" or "Chromium" or "Edg"
	return strings.Contains(ua, "Safari") && !strings.Contains(ua, "Chrome") && !strings.Contains(ua, "Chromium") && !strings.Contains(ua, "Edg")
}
