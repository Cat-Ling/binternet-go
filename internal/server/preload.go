package server

import (
	"github.com/flinternet/flinternet/internal/pinterest"
	"context"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

func (s *Server) triggerPreload(mode, query, scope, bookmark, csrfToken string) {
	preloadKey := mode + ":" + query + ":" + scope + ":" + bookmark

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
		s.preload(ctx, mode, query, scope, bookmark, csrfToken)
	}()
}

func (s *Server) preload(ctx context.Context, mode, query, scope, bookmark string, csrfToken string) {
	var result *pinterest.SearchResult
	var err error
	
	cacheKey := "preload:" + mode + ":v5:" + query + ":" + scope + ":" + bookmark

	if cachedBytes, found := s.Cache.Get(cacheKey); found {
		if jsonErr := msgpack.Unmarshal(cachedBytes, &result); jsonErr != nil {
			result = nil
		}
	}

	if result == nil {
		result, err = s.Client.Search(ctx, query, scope, bookmark, csrfToken)
		if err != nil {
			return
		}

		if serialized, err := msgpack.Marshal(result); err == nil {
			s.Cache.Set(cacheKey, serialized, 0)
		}
	}

	// Preload Images
	if result != nil && len(result.Media) > 0 && s.Config.PreloadImages {
		go func(items []pinterest.MediaItem) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Recovered from panic in preload images: %v", r)
				}
			}()

			for _, item := range items {
				imgURL := item.URL
				if item.IsVideo && item.ThumbnailURL != "" {
					imgURL = item.ThumbnailURL
				}
				if imgURL == "" {
					continue
				}

				select {
				case s.PreloadSem <- struct{}{}:
					go func(target string) {
						defer func() { <-s.PreloadSem }()
						defer func() {
							if r := recover(); r != nil {
								log.Printf("Recovered from panic in image fetch: %v", r)
							}
						}()

						if _, found := s.Cache.Get(target); found {
							return
						}

						req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
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
								s.Cache.Set(target, encodeCachedMedia(resp.Header.Get("Content-Type"), body), 0)
							}
						}
					}(imgURL)
				default:
					// Global semaphore full, skip this image
				}
			}
		}(result.Media)
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
					s.Cache.Set(videoURL, encodeCachedMedia(resp.Header.Get("Content-Type"), body), 0)
				}
			}
		}(m.VideoURL)
	}
}
