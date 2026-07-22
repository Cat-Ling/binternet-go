package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"sync"
)

// isAllowedProxyDomain checks if a host is allowed for proxying
func (s *Server) isAllowedProxyDomain(host string) bool {
	for _, domain := range s.Whitelist {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func getBestVariantURL(data []byte) string {
	str := string(data)
	if !strings.Contains(str, "#EXT-X-STREAM-INF") {
		return ""
	}

	lines := strings.Split(str, "\n")
	type variant struct {
		uriLine   string
		bandwidth int
	}
	var variants []variant
	var currentInfoLine string
	
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			currentInfoLine = line
		} else if currentInfoLine != "" && !strings.HasPrefix(line, "#") {
			bw := 0
			if idx := strings.Index(currentInfoLine, "BANDWIDTH="); idx != -1 {
				fmt.Sscanf(currentInfoLine[idx+10:], "%d", &bw)
			}
			variants = append(variants, variant{
				uriLine:   line,
				bandwidth: bw,
			})
			currentInfoLine = ""
		} else if currentInfoLine == "" {
			// skip headers
		}
	}
	
	if len(variants) == 0 {
		return ""
	}
	
	var best variant
	for _, v := range variants {
		if v.bandwidth > best.bandwidth {
			best = v
		}
	}
	
	return best.uriLine
}

var proxyBufPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 64*1024)
		return &buf
	},
}

func (s *Server) handleUnifiedProxy(w http.ResponseWriter, r *http.Request) {
	domain := chi.URLParam(r, "domain")
	sub := chi.URLParam(r, "sub")
	path := chi.URLParam(r, "*")

	if domain == "ideas" || domain == "search" || domain == "pin" {
		http.Error(w, "Route collision avoided", http.StatusNotFound)
		return
	}

	targetURL := fmt.Sprintf("https://%s.%s.com/%s", sub, domain, path)
	s.proxyMedia(w, r, targetURL)
}

func (s *Server) proxyMedia(w http.ResponseWriter, r *http.Request, targetURL string) {
	u, err := url.Parse(targetURL)
	if err != nil {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	if !s.isAllowedProxyDomain(u.Host) {
		http.Error(w, "Domain not allowed", http.StatusForbidden)
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Check cache
	if s.Cache != nil {
		if data, found := s.Cache.Get(targetURL); found {
			if ct, mediaData, ok := decodeCachedMedia(data); ok {
				w.Header().Set("Content-Type", ct)
				http.ServeContent(w, r, "media", time.Time{}, bytes.NewReader(mediaData))
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
	
	// Map HEIC/HEIF based on extension if Content-Type is missing or generic
	if contentType == "" || contentType == "application/octet-stream" {
		ext := ""
		lastDot := strings.LastIndex(u.Path, ".")
		if lastDot != -1 {
			ext = strings.ToLower(u.Path[lastDot+1:])
		}
		if ext == "heic" || ext == "heif" {
			contentType = "image/heic"
		}
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Accept-Ranges", "bytes")

	isM3U8 := strings.HasSuffix(u.Path, ".m3u8")
	isMediaChunk := strings.HasSuffix(u.Path, ".ts") || strings.HasSuffix(u.Path, ".m4s") || strings.HasSuffix(u.Path, ".mp4") || strings.HasSuffix(u.Path, ".m4v") || strings.HasSuffix(u.Path, ".cmfv")
	const maxCacheSize = 25 * 1024 * 1024 // 25MB for images

	if isM3U8 && resp.StatusCode == http.StatusOK {
		m3u8Data, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "Failed to read playlist", http.StatusBadGateway)
			return
		}
		
		// Optimize: Instead of serving a master playlist with 1 variant,
		// we fetch the best subplaylist directly on the server side and serve IT as the master!
		// This saves the browser an entire HTTP round trip, cutting time-to-first-frame by ~30%.
		subURL := getBestVariantURL(m3u8Data)
		if subURL != "" {
			fullSubURL := subURL
			if !strings.HasPrefix(subURL, "http") {
				lastSlash := strings.LastIndex(targetURL, "/")
				if lastSlash != -1 {
					fullSubURL = targetURL[:lastSlash+1] + subURL
				}
			}
			
			subReq, _ := http.NewRequestWithContext(r.Context(), "GET", fullSubURL, nil)
			subResp, subErr := s.Client.HTTPClient.Do(subReq)
			if subErr == nil && subResp.StatusCode == http.StatusOK {
				subData, err := io.ReadAll(subResp.Body)
				if err == nil {
					m3u8Data = subData // Replace master playlist with subplaylist!
				}
				subResp.Body.Close()
			}
		}
		
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(m3u8Data)))
		w.WriteHeader(resp.StatusCode)
		w.Write(m3u8Data)

		if s.Cache != nil {
			go func(data []byte, ct string) {
				s.Cache.Set(targetURL, encodeCachedMedia(ct, data), 0)
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
	// Do not cache large media chunks in memory, only images up to maxCacheSize
	doCache := resp.StatusCode == http.StatusOK && s.Cache != nil && !isMediaChunk && (resp.ContentLength > 0 && resp.ContentLength <= maxCacheSize)

	var writer io.Writer = w
	if doCache {
		cacheBuf.Grow(int(resp.ContentLength))
		writer = io.MultiWriter(w, &cacheBuf)
	}

	bufPtr := proxyBufPool.Get().(*[]byte)
	_, err = io.CopyBuffer(writer, resp.Body, *bufPtr)
	proxyBufPool.Put(bufPtr)
	
	if err != nil {
		return
	}

	if doCache && cacheBuf.Len() > 0 {
		go func(data []byte, ct string) {
			s.Cache.Set(targetURL, encodeCachedMedia(ct, data), 0)
		}(cacheBuf.Bytes(), contentType)
	}
}

func (s *Server) handleImageProxy(w http.ResponseWriter, r *http.Request) {
	imageURL := r.URL.Query().Get("url")
	if imageURL == "" {
		http.Error(w, "Missing URL", http.StatusBadRequest)
		return
	}
	s.proxyMedia(w, r, imageURL)
}

func (s *Server) handlePathProxy(w http.ResponseWriter, r *http.Request) {
	targetPath := chi.URLParam(r, "*")
	if targetPath == "" {
		http.Error(w, "Missing path", http.StatusBadRequest)
		return
	}
	targetURL := "https://" + targetPath
	s.proxyMedia(w, r, targetURL)
}

