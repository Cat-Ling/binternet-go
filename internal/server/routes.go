package server

import (
	"github.com/flinternet/flinternet/assets"
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/klauspost/compress/gzhttp"
)

func (s *Server) routes() {
	// No middleware.Logger — only errors matter, stdout is expensive under load
	s.Router.Use(middleware.Recoverer)
	
	// Use high-performance klauspost/compress correctly as middleware
	s.Router.Use(func(next http.Handler) http.Handler {
		return gzhttp.GzipHandler(next)
	})

	s.Router.Use(s.SecurityHeadersMiddleware)

	// Static files from Embed FS
	staticSubFS, _ := fs.Sub(assets.AssetsFS, "static")
	s.Router.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticSubFS))))

	// Static file support for compatibility paths (misc/style-dark.css)
	s.Router.Get("/misc/*", func(w http.ResponseWriter, r *http.Request) {
		fsPath := "static/" + chi.URLParam(r, "*")

		data, err := assets.AssetsFS.ReadFile(fsPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}

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
	s.Router.Get("/ideas", s.handleIdeas)
	s.Router.Get("/ideas/{slug}/{id}/", s.handleIdeas)
	s.Router.Get("/ideas/{slug}/{id}", s.handleIdeas)
	s.Router.Get("/search/{scope}/", s.handleSearch)
	s.Router.Get("/search/{scope}", s.handleSearch)
	s.Router.Get("/search/pins/", s.handleSearch) // Keep for compatibility
	s.Router.Get("/search/pins", s.handleSearch)
	s.Router.Get("/pin/{id}/", s.handlePin)
	s.Router.Get("/pin/{id}", s.handlePin)
	s.Router.Get("/pin/{id}/comments/", s.handleComments)
	s.Router.Get("/{username}/{slug}/", s.handleBoard)
	s.Router.Get("/{username}/{slug}", s.handleBoard)
	s.Router.Get("/{username}/", s.handleUser)
	s.Router.Get("/{username}", s.handleUser)
	s.Router.Get("/api/search/", s.handleAPI)
	s.Router.Get("/proxy/image/", s.handleImageProxy)
	s.Router.Get("/video/*", s.handlePathProxy) // For transparent HLS proxying
	s.Router.Get("/legal", s.handleLegal)

	// Dynamic unified proxy scheme - LAST RESORT
	s.Router.Get("/{domain}/{sub}/*", s.handleUnifiedProxy)

	// Static files from embed
	staticFS, _ := fs.Sub(assets.AssetsFS, "static")
	s.Router.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
}

func (s *Server) SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Allow images from self and the proxy endpoint
		csp := "default-src 'self'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; object-src 'none';"
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}
