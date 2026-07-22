package server

import (
	"github.com/flinternet/flinternet/internal/pinterest"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/vmihailenco/msgpack/v5"
)

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	baseURL := scheme + "://" + r.Host

	data := struct {
		Query         string
		Scope         string
		Boards        []pinterest.Board
		Bookmark      string
		BaseURL       string
		OgTitle       string
		OgDescription string
		OgImage       string
		OgType        string
	}{
		Query:         "",
		Scope:         "pins",
		Boards:        nil,
		Bookmark:      "",
		BaseURL:       baseURL,
		OgTitle:       "Flinternet - Privacy Respecting Pinterest Search",
		OgDescription: "Search and explore Pinterest without tracking, ads, or clunky interfaces. Your privacy, your search.",
		OgImage:       "", // Default logo will be used
		OgType:        "website",
	}
	s.Tmpl.ExecuteTemplate(w, "index.html", data)
}

func (s *Server) handleLegal(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	baseURL := scheme + "://" + r.Host

	data := struct {
		Query         string
		Scope         string
		BaseURL       string
		OgTitle       string
		OgDescription string
		OgImage       string
		OgType        string
	}{
		Query:         "",
		Scope:         "pins",
		BaseURL:       baseURL,
		OgTitle:       "Legal Notice - Flinternet",
		OgDescription: "Legal notice and disclaimer for Flinternet.",
		OgImage:       "",
		OgType:        "website",
	}
	err := s.Tmpl.ExecuteTemplate(w, "legal.html", data)
	if err != nil {
		log.Println("Error executing legal template:", err)
	}
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	baseURL := scheme + "://" + r.Host

	pinID := chi.URLParam(r, "id")
	query := r.URL.Query().Get("q")
	scope := r.URL.Query().Get("s")
	if scope == "" {
		scope = "pins"
	}
	if pinID == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	cacheKey := "pin:v4:" + pinID
	var item *pinterest.MediaItem

	if cachedBytes, found := s.Cache.Get(cacheKey); found {
		if err := msgpack.Unmarshal(cachedBytes, &item); err == nil {
			// Cache hit
		}
	}

	if item == nil {
		var err error
		item, err = s.Client.GetPin(r.Context(), pinID, "")
		if err != nil {
			s.handleError(w, http.StatusNotFound, "Pin not found.", err, r)
			return
		}

		if serialized, err := msgpack.Marshal(item); err == nil {
			s.Cache.Set(cacheKey, serialized, 0)
		}
	}

	data := struct {
		Item          *pinterest.MediaItem
		HasVideo      bool
		IsSafari      bool
		Query         string
		Scope         string
		BaseURL       string
		OgTitle       string
		OgDescription string
		OgImage       string
		OgType        string
		VideoURL      string
	}{
		Item:     item,
		HasVideo: item.IsVideo,
		IsSafari: s.isSafari(r),
		Query:    query,
		Scope:    scope,
		BaseURL:  baseURL,
		OgTitle:  item.Title,
		OgDescription: item.Description,
		OgImage:  item.URL,
		OgType:   "website",
	}

	if item.IsVideo {
		data.OgType = "video.other"
		data.VideoURL = item.VideoURL
	}

	if data.OgTitle == "" {
		data.OgTitle = "Pin by " + item.AuthorName
	}
	if data.OgDescription == "" {
		data.OgDescription = "View this pin and more inspiration on Flinternet."
	}

	s.Tmpl.ExecuteTemplate(w, "pin.html", data)
}

func (s *Server) handleComments(w http.ResponseWriter, r *http.Request) {
	pinID := chi.URLParam(r, "id")
	aid := r.URL.Query().Get("aid")
	bookmark := r.URL.Query().Get("bookmark")

	commentRes, err := s.Client.GetComments(r.Context(), pinID, aid, bookmark, "")
	if err != nil {
		http.Error(w, "Failed to load comments", http.StatusInternalServerError)
		return
	}

	if commentRes == nil {
		http.Error(w, "Comments not found", http.StatusNotFound)
		return
	}

	data := struct {
		PinID         string
		Aid           string
		CommentResult *pinterest.CommentResult
		Query         string
	}{
		PinID:         pinID,
		Aid:           commentRes.AggregatedPinID,
		CommentResult: commentRes,
		Query:         "",
	}

	s.Tmpl.ExecuteTemplate(w, "comments.html", data)
}

func (s *Server) handleIdeas(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	baseURL := scheme + "://" + r.Host

	interestID := chi.URLParam(r, "id")
	slug := chi.URLParam(r, "slug")
	bookmark := r.URL.Query().Get("bookmark")

	if interestID == "" {
		// Fallback for general /ideas page (e.g., from the homepage)
		boards, nextBookmark, err := s.Client.GetBoards(r.Context(), "pinterest", bookmark, "")
		if err != nil {
			s.handleError(w, http.StatusInternalServerError, "Failed to load ideas.", err, r)
			return
		}

		data := struct {
			Boards        []pinterest.Board
			Bookmark      string
			Query         string
			Scope         string
			BaseURL       string
			OgTitle       string
			OgDescription string
			OgImage       string
			OgType        string
		}{
			Boards:        boards,
			Bookmark:      nextBookmark,
			Query:         "",
			Scope:         "pins",
			BaseURL:       baseURL,
			OgTitle:       "Flinternet Ideas",
			OgDescription: "Explore Ideas on Flinternet",
			OgImage:       "",
			OgType:        "website",
		}


		s.Tmpl.ExecuteTemplate(w, "index.html", data)
		return
	}

	// Specific interest page (e.g., /ideas/gal-gadot-fashion/900397309468/)
	token, _ := s.Client.GetCSRFToken(r.Context())
	result, err := s.Client.GetInterestPins(r.Context(), slug, interestID, bookmark, token)
	if err != nil {
		s.handleError(w, http.StatusInternalServerError, "Failed to load interest pins.", err, r)
		return
	}

	if result == nil {
		s.handleError(w, http.StatusInternalServerError, "Failed to load pins.", fmt.Errorf("result is nil"), r)
		return
	}

	hasVideo := false
	for _, m := range result.Media {
		if m.IsVideo {
			hasVideo = true
			break
		}
	}

	data := struct {
		Query         string
		Scope         string
		Media         []pinterest.MediaItem
		Bookmark      string
		CSRFToken     string
		HasVideo      bool
		IsSafari      bool
		BaseURL       string
		OgTitle       string
		OgDescription string
		OgImage       string
		OgType        string
		Slug          string
		InterestID    string
	}{
		Query:         strings.ReplaceAll(slug, "-", " "),
		Scope:         "pins",
		Media:         result.Media,
		Bookmark:      result.Bookmark,
		CSRFToken:     result.CSRFToken,
		HasVideo:      hasVideo,
		IsSafari:      s.isSafari(r),
		BaseURL:       baseURL,
		OgTitle:       strings.ReplaceAll(slug, "-", " "),
		OgDescription: "Explore " + strings.ReplaceAll(slug, "-", " ") + " on Flinternet",
		OgImage:       "",
		OgType:        "website",
		Slug:          slug,
		InterestID:    interestID,
	}

	if s.Config.Preload && hasVideo {
		go s.prefetchM3U8s(result.Media)
	}

	err = s.Tmpl.ExecuteTemplate(w, "ideas.html", data)
	if err != nil {
		fmt.Println("Template error in handleIdeas:", err)
	}
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	baseURL := scheme + "://" + r.Host

	username := chi.URLParam(r, "username")
	bookmark := r.URL.Query().Get("bookmark")
	query := r.URL.Query().Get("q")
	scope := r.URL.Query().Get("s")
	if scope == "" {
		scope = "users"
	}

	if username == "" {
		s.handleError(w, http.StatusBadRequest, "Username is required.", nil, r)
		return
	}

	profile, err := s.Client.GetUser(r.Context(), username, "")
	if err != nil {
		s.handleError(w, http.StatusNotFound, "User not found or profile is private.", err, r)
		return
	}

	if profile == nil {
		s.handleError(w, http.StatusNotFound, "User not found.", fmt.Errorf("profile is nil"), r)
		return
	}

	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "boards"
	}

	var boards []pinterest.Board
	var media []pinterest.MediaItem
	var nextBookmark string
	var errFetch error
	var csrfToken string

	token, _ := s.Client.GetCSRFToken(r.Context())

	switch tab {
	case "created":
		res, err := s.Client.GetUserCreatedPins(r.Context(), username, bookmark, token)
		if err == nil && res != nil {
			media = res.Media
			nextBookmark = res.Bookmark
			csrfToken = res.CSRFToken
		}
		errFetch = err
	case "more_ideas":
		res, err := s.Client.GetUserSavedPins(r.Context(), username, bookmark, token)
		if err == nil && res != nil {
			media = res.Media
			nextBookmark = res.Bookmark
			csrfToken = res.CSRFToken
		}
		errFetch = err
	default: // "boards"
		boards, nextBookmark, errFetch = s.Client.GetBoards(r.Context(), username, bookmark, token)
		// Fix for users reporting 1 board when there are 0 shown.
		if bookmark == "" && len(boards) == 0 && profile.BoardCount <= 1 {
			profile.BoardCount = 0
		}
	}

	if errFetch != nil {
		s.handleError(w, http.StatusInternalServerError, "Failed to load tab content.", errFetch, r)
		return
	}

	hasVideo := false
	for _, m := range media {
		if m.IsVideo {
			hasVideo = true
			break
		}
	}

	if s.Config.Preload && hasVideo {
		go s.prefetchM3U8s(media)
	}

	data := struct {
		Profile         *pinterest.UserProfile
		Boards          []pinterest.Board
		Media           []pinterest.MediaItem
		Bookmark        string
		CSRFToken       string
		Tab             string
		CurrentUsername string
		IsSafari        bool
		Query           string
		Scope           string
		BaseURL         string
		OgTitle         string
		OgDescription   string
		OgImage         string
		OgType          string
	}{
		Profile:         profile,
		Boards:          boards,
		Media:           media,
		Bookmark:        nextBookmark,
		CSRFToken:       csrfToken,
		Tab:             tab,
		CurrentUsername: username,
		IsSafari:        s.isSafari(r),
		Query:           query,
		Scope:           scope,
		BaseURL:         baseURL,
		OgTitle:         fmt.Sprintf("%s (@%s)", profile.FullName, profile.Username),
		OgDescription:   fmt.Sprintf("Explore boards and pins from %s on Flinternet.", profile.FullName),
		OgImage:         profile.AvatarURL,
		OgType:          "profile",
	}

	s.Tmpl.ExecuteTemplate(w, "user.html", data)
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	baseURL := scheme + "://" + r.Host

	username := chi.URLParam(r, "username")
	slug := chi.URLParam(r, "slug")
	bookmark := r.URL.Query().Get("bookmark")
	query := r.URL.Query().Get("q")
	scope := r.URL.Query().Get("s")
	if scope == "" {
		scope = "boards"
	}

	if username == "" || slug == "" {
		s.handleError(w, http.StatusBadRequest, "Username and slug are required.", nil, r)
		return
	}

	result, err := s.Client.GetBoardPins(r.Context(), username, slug, bookmark, "")
	if err != nil {
		s.handleError(w, http.StatusNotFound, "Board not found or could not be loaded.", err, r)
		return
	}

	if result == nil {
		s.handleError(w, http.StatusInternalServerError, "Failed to load board pins.", fmt.Errorf("result is nil"), r)
		return
	}

	hasVideo := false
	for _, m := range result.Media {
		if m.IsVideo {
			hasVideo = true
			break
		}
	}

	data := struct {
		Username      string
		BoardSlug     string
		Board         *pinterest.Board
		Media         []pinterest.MediaItem
		Bookmark  string
		CSRFToken string
		HasVideo  bool
		IsSafari  bool
		Query     string
		Scope     string
		BaseURL   string
		OgTitle   string
		OgDescription string
		OgImage       string
		OgType        string
		}{
		Username:  username,
		BoardSlug: slug,
		Board:     result.Board,
		Media:     result.Media,
		Bookmark:  result.Bookmark,
		CSRFToken: result.CSRFToken,
		HasVideo:  hasVideo,
		IsSafari:  s.isSafari(r),
		Query:     query,
		Scope:     scope,
		BaseURL:   baseURL,
		OgTitle:   fmt.Sprintf("%s - %s", slug, username),
		OgDescription: fmt.Sprintf("View the %s board by %s on Flinternet.", slug, username),
		OgImage:   "",
		OgType:    "website",
	}

	if result.Board != nil {
		data.OgTitle = result.Board.Name + " - " + username
		data.OgImage = result.Board.Thumbnail
		if result.Board.Description != "" {
			data.OgDescription = result.Board.Description
		}
	} else if len(result.Media) > 0 {
		data.OgImage = result.Media[0].URL
	}

	s.Tmpl.ExecuteTemplate(w, "board.html", data)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	baseURL := scheme + "://" + r.Host

	query := r.URL.Query().Get("q")
	scope := chi.URLParam(r, "scope") // From /search/{scope}/
	if scope == "" {
		scope = "pins"
	}
	bookmark := r.URL.Query().Get("bookmark")

	if query == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if len(query) > 64 {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	cacheKey := "search:v6:" + scope + ":" + query + ":" + bookmark
	var result *pinterest.SearchResult

	if cachedBytes, found := s.Cache.Get(cacheKey); found {
		if err := msgpack.Unmarshal(cachedBytes, &result); err == nil {
			// Cache hit
		}
	}

	if result == nil {
		var err error
		result, err = s.Client.Search(r.Context(), query, scope, bookmark, "")
		if err != nil {
			s.handleError(w, http.StatusInternalServerError, "Search failed. Please try again later.", err, r)
			return
		}

		if result == nil {
			s.handleError(w, http.StatusInternalServerError, "Search failed.", fmt.Errorf("result is nil"), r)
			return
		}

		if serialized, err := msgpack.Marshal(result); err == nil {
			s.Cache.Set(cacheKey, serialized, 0)
		}
	}

	// Trigger preloading if enabled
	if s.Config.Preload && result.Bookmark != "" && result.Bookmark != "-end-" && scope == "pins" {
		s.triggerPreload("web", query, scope, result.Bookmark, result.CSRFToken)
	}

	hasVideo := false
	if scope == "pins" || scope == "videos" {
		for _, m := range result.Media {
			if m.IsVideo {
				hasVideo = true
				break
			}
		}
	}

	data := struct {
		Query         string
		Scope         string
		Media         []pinterest.MediaItem
		Boards        []pinterest.Board
		Users         []pinterest.UserProfile
		Bookmark      string
		CSRFToken     string
		HasVideo      bool
		IsSafari      bool
		BaseURL       string
		OgTitle       string
		OgDescription string
		OgImage       string
		OgType        string
		VideoURL      string
	}{
		Query:         query,
		Scope:         scope,
		Media:         result.Media,
		Boards:        result.Boards,
		Users:         result.Users,
		Bookmark:      result.Bookmark,
		CSRFToken:     result.CSRFToken,
		HasVideo:      hasVideo,
		IsSafari:      s.isSafari(r),
		BaseURL:       baseURL,
		OgTitle:       query,
		OgDescription: fmt.Sprintf("Explore the latest %s %s on Flinternet. Secure, private, and ad-free.", query, scope),
		OgImage:       "",
		OgType:        "website",
	}

	if scope == "pins" || scope == "videos" {
		if len(result.Media) > 0 {
			data.OgImage = result.Media[0].URL
			if result.Media[0].IsVideo {
				data.OgType = "video.other"
				data.VideoURL = result.Media[0].VideoURL
			}
		}
	} else if scope == "boards" && len(result.Boards) > 0 {
		data.OgImage = result.Boards[0].Thumbnail
	} else if scope == "users" && len(result.Users) > 0 {
		data.OgImage = result.Users[0].AvatarURL
	}

	if s.Config.Preload && hasVideo {
		go s.prefetchM3U8s(result.Media)
	}

	s.Tmpl.ExecuteTemplate(w, "search.html", data)
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "pins"
	}
	bookmark := r.URL.Query().Get("bookmark")

	cacheKey := "api:search:v6:" + scope + ":" + query + ":" + bookmark
	var result *pinterest.SearchResult

	if cachedBytes, found := s.Cache.Get(cacheKey); found {
		if err := msgpack.Unmarshal(cachedBytes, &result); err == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result)
			return
		}
	}
	var err error
	result, err = s.Client.Search(r.Context(), query, scope, bookmark, "")
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
