package server

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleError(w http.ResponseWriter, status int, userMsg string, logErr error, r *http.Request) {
	if logErr != nil {
		log.Printf("Error (%d): %v", status, logErr)
	}

	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	baseURL := scheme + "://" + r.Host

	w.WriteHeader(status)
	data := struct {
		Message       string
		Detail        string
		Query         string
		Scope         string
		IsSafari      bool
		HasVideo      bool
		BaseURL       string
		OgTitle       string
		OgDescription string
		OgImage       string
		OgType        string
	}{
		Message:       userMsg,
		Query:         "",
		Scope:         "pins",
		IsSafari:      false,
		HasVideo:      false,
		BaseURL:       baseURL,
	}

	if s.Config.ShowErrorsToClient && logErr != nil {
		data.Detail = logErr.Error()
	}

	data.OgTitle = "Error " + fmt.Sprint(status)
	data.OgDescription = userMsg
	data.OgImage = ""
	data.OgType = "website"

	if err := s.Tmpl.ExecuteTemplate(w, "error.html", data); err != nil {
		log.Printf("Failed to render error template: %v", err)
		// Fallback if template fails
		http.Error(w, userMsg, status)
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
