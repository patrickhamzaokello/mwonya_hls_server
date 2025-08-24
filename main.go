package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
)

// Constants
const (
	DefaultCacheSize          = 10000
	DefaultCacheExpiry        = 2 * time.Hour
	DefaultRateLimitWindow    = 100 * time.Millisecond
	DefaultCleanupInterval    = 5 * time.Minute
	DefaultPort               = "8080"
	PrecacheSegmentCount      = 10
)

type AudioTrack struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration"` // in seconds
}

type StreamingStats struct {
	TotalStreams      int64            `json:"total_streams"`
	TrackStreams      map[string]int64 `json:"track_streams"`
	ActiveStreams     int64            `json:"active_streams"`
	BandwidthUsage    int64            `json:"bandwidth_usage"` // in bytes
	StartTime         time.Time        `json:"start_time"`
	Uptime            string           `json:"uptime"`
	CacheHits         int64            `json:"cache_hits"`
	CacheMisses       int64            `json:"cache_misses"`
}

type LocalFileHandler struct {
	basePath string
	mu       sync.RWMutex
}

type StreamingServer struct {
	fileHandler     *LocalFileHandler
	hlsPrefix       string // Folder path for HLS files
	rawPrefix       string // Folder path for raw/non-HLS files
	tracks          sync.Map // Thread-safe track storage
	segmentCache    *LRUCache
	rateLimiter     *RateLimiter
	stats           *StreamingStats
	statsMu         sync.RWMutex
	maxCacheSize    int
	cacheExpiry     time.Duration
	isPreloaded     bool
	preloadMu       sync.RWMutex
}

// LRUCache implements a thread-safe LRU cache with expiration
type LRUCache struct {
	cache  map[string]*cacheItem
	mu     sync.RWMutex
	maxSize int
}

type cacheItem struct {
	value      interface{}
	expiration time.Time
}

type RateLimiter struct {
	visits map[string]time.Time
	mu     sync.Mutex
	window time.Duration
}

func NewLocalFileHandler(basePath string) *LocalFileHandler {
	return &LocalFileHandler{
		basePath: basePath,
	}
}

func (l *LocalFileHandler) ReadFile(path string) ([]byte, error) {
	fullPath := filepath.Join(l.basePath, path)
	return os.ReadFile(fullPath)
}

func (l *LocalFileHandler) FileExists(path string) (bool, error) {
	fullPath := filepath.Join(l.basePath, path)
	_, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (l *LocalFileHandler) ListFiles(prefix string) ([]string, error) {
	fullPath := filepath.Join(l.basePath, prefix)
	var files []string

	err := filepath.Walk(fullPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			relPath, err := filepath.Rel(l.basePath, path)
			if err != nil {
				return err
			}
			files = append(files, relPath)
		}
		return nil
	})

	return files, err
}

func NewRateLimiter(window time.Duration) *RateLimiter {
	return &RateLimiter{
		visits: make(map[string]time.Time),
		window: window,
	}
}

func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	if last, exists := rl.visits[ip]; exists {
		if now.Sub(last) < rl.window {
			return false
		}
	}
	rl.visits[ip] = now
	return true
}

func (rl *RateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	for ip, last := range rl.visits {
		if now.Sub(last) > 5*time.Minute {
			delete(rl.visits, ip)
		}
	}
}

func NewLRUCache(maxSize int) *LRUCache {
	return &LRUCache{
		cache:  make(map[string]*cacheItem),
		maxSize: maxSize,
	}
}

func (c *LRUCache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	item, exists := c.cache[key]
	c.mu.RUnlock()

	if !exists {
		return nil, false
	}

	if time.Now().After(item.expiration) {
		c.mu.Lock()
		delete(c.cache, key)
		c.mu.Unlock()
		return nil, false
	}

	return item.value, true
}

func (c *LRUCache) Set(key string, value interface{}, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.cache) >= c.maxSize {
		// Simple eviction strategy - remove random items
		for k := range c.cache {
			delete(c.cache, k)
			if len(c.cache) < c.maxSize/2 {
				break
			}
		}
	}

	c.cache[key] = &cacheItem{
		value:      value,
		expiration: time.Now().Add(ttl),
	}
}

func NewStreamingServer(hlsPrefix, rawPrefix, localPath string) (*StreamingServer, error) {
	// Ensure prefixes end with slash
	hlsPrefix = ensureTrailingSlash(hlsPrefix)
	rawPrefix = ensureTrailingSlash(rawPrefix)

	fileHandler := NewLocalFileHandler(localPath)

	server := &StreamingServer{
		fileHandler:     fileHandler,
		hlsPrefix:       hlsPrefix,
		rawPrefix:       rawPrefix,
		segmentCache:    NewLRUCache(DefaultCacheSize),
		rateLimiter:     NewRateLimiter(DefaultRateLimitWindow),
		maxCacheSize:    DefaultCacheSize,
		cacheExpiry:     DefaultCacheExpiry,
		stats: &StreamingStats{
			TrackStreams: make(map[string]int64),
			StartTime:    time.Now(),
		},
	}
	
	return server, nil
}

func (s *StreamingServer) preloadAllTracks() {
	s.preloadMu.Lock()
	defer s.preloadMu.Unlock()
	
	if s.isPreloaded {
		return
	}

	log.Println("Starting track metadata preloading...")
	
	// List all files in the HLS prefix
	files, err := s.fileHandler.ListFiles(s.hlsPrefix)
	if err != nil {
		log.Printf("Error listing files during preload: %v", err)
		return
	}

	trackIDs := make(map[string]struct{})
	for _, file := range files {
		// Extract track ID from path (format: prefix/trackID/...)
		relPath := strings.TrimPrefix(file, s.hlsPrefix)
		parts := strings.Split(relPath, "/")
		if len(parts) > 0 && parts[0] != "" {
			trackIDs[parts[0]] = struct{}{}
		}
	}

	// Load metadata for all discovered tracks
	for trackID := range trackIDs {
		if _, err := s.loadTrackMetadata(trackID); err != nil {
			log.Printf("Error preloading track %s: %v", trackID, err)
		}
	}

	s.isPreloaded = true
	log.Printf("Preloaded metadata for %d tracks", len(trackIDs))
}

func (s *StreamingServer) ensureTrackPreloaded(trackID string) {
	s.preloadMu.RLock()
	preloaded := s.isPreloaded
	s.preloadMu.RUnlock()

	if !preloaded {
		if _, err := s.loadTrackMetadata(trackID); err != nil {
			log.Printf("Error loading track %s: %v", trackID, err)
		}
	}
}

func ensureTrailingSlash(path string) string {
	if path != "" && !strings.HasSuffix(path, "/") {
		return path + "/"
	}
	return path
}

func getContentTypeByExtension(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp3": return "audio/mpeg"
	case ".wav": return "audio/wav"
	case ".ogg": return "audio/ogg"
	case ".flac": return "audio/flac"
	case ".m4a": return "audio/mp4"
	case ".aac": return "audio/aac"
	case ".webm": return "audio/webm"
	case ".jpg", ".jpeg": return "image/jpeg"
	case ".png": return "image/png"
	case ".gif": return "image/gif"
	case ".svg": return "image/svg+xml"
	default: return "application/octet-stream"
	}
}

func (s *StreamingServer) loadTrackMetadata(trackID string) (*AudioTrack, error) {
	// Check if track is already loaded
	if cached, ok := s.tracks.Load(trackID); ok {
		return cached.(*AudioTrack), nil
	}

	// Try to load from local files
	track, err := s.loadTrackFromFiles(trackID)
	if err != nil {
		return nil, err
	}

	// Cache the loaded track
	s.tracks.Store(trackID, track)
	return track, nil
}

func (s *StreamingServer) loadTrackFromFiles(trackID string) (*AudioTrack, error) {
	// Try HLS location first
	metadataKey := fmt.Sprintf("%s%s/metadata.json", s.hlsPrefix, trackID)

	data, err := s.fileHandler.ReadFile(metadataKey)
	if err == nil {
		var track AudioTrack
		if err := json.Unmarshal(data, &track); err != nil {
			return nil, fmt.Errorf("failed to decode metadata: %v", err)
		}
		return &track, nil
	}

	// Try raw location (using raw/metadata/<trackID>.json for flat structure)
	metadataKey = fmt.Sprintf("%smetadata/%s.json", s.rawPrefix, trackID)
	data, err = s.fileHandler.ReadFile(metadataKey)
	if err == nil {
		var track AudioTrack
		if err := json.Unmarshal(data, &track); err != nil {
			return nil, fmt.Errorf("failed to decode metadata: %v", err)
		}
		return &track, nil
	}

	// Return default metadata if not found
	return &AudioTrack{
		ID:       trackID,
		Title:    strings.ReplaceAll(trackID, "_", " "),
		Artist:   "Unknown Artist",
		Duration: 180,
	}, nil
}

func (s *StreamingServer) generateMasterPlaylist(trackID string) string {
	basePath := fmt.Sprintf("/stream/%s", trackID)
	return fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:3

#EXT-X-STREAM-INF:BANDWIDTH=80000,CODECS="mp4a.40.2"
%s/low/playlist.m3u8

#EXT-X-STREAM-INF:BANDWIDTH=160000,CODECS="mp4a.40.2"
%s/med/playlist.m3u8

#EXT-X-STREAM-INF:BANDWIDTH=384000,CODECS="mp4a.40.2"
%s/high/playlist.m3u8
`, basePath, basePath, basePath)
}

func (s *StreamingServer) fixPlaylistURLs(content []byte, trackID string, isSegment bool) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	var result strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			result.WriteString(line)
			result.WriteString("\n")
			continue
		}

		if strings.HasPrefix(line, "/") && strings.HasSuffix(line, "/playlist.m3u8") {
			parts := strings.Split(strings.Trim(line, "/"), "/")
			if len(parts) >= 3 {
				quality := parts[len(parts)-2]
				line = fmt.Sprintf("/stream/%s/%s/playlist.m3u8", trackID, quality)
			}
		} else if isSegment && strings.HasSuffix(line, ".ts") {
			line = line
		}

		result.WriteString(line)
		result.WriteString("\n")
	}

	return []byte(result.String())
}

func (s *StreamingServer) fetchFromLocalFiles(key string) ([]byte, error) {
	return s.fileHandler.ReadFile(key)
}

func (s *StreamingServer) handleMasterPlaylist(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	trackID := strings.TrimSuffix(path, "/playlist.m3u8")
	trackID = strings.Trim(trackID, "/")

	s.ensureTrackPreloaded(trackID)

	track, err := s.loadTrackMetadata(trackID)
	if err != nil {
		log.Printf("Error loading track metadata for %s: %v", trackID, err)
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	s.updateStats(trackID)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "max-age=300")

	key := fmt.Sprintf("%s%s/playlist.m3u8", s.hlsPrefix, trackID)
	if data, err := s.fetchFromLocalFiles(key); err == nil {
		fixedData := s.fixPlaylistURLs(data, trackID, false)
		w.Write(fixedData)
		log.Printf("Served master playlist for track: %s (%s)", track.Title, trackID)
	} else {
		playlist := s.generateMasterPlaylist(trackID)
		w.Write([]byte(playlist))
		log.Printf("Served generated master playlist for track: %s (%s)", track.Title, trackID)
	}
}

func (s *StreamingServer) updateStats(trackID string) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.stats.TotalStreams++
	s.stats.TrackStreams[trackID]++
	s.stats.ActiveStreams++
}

func (s *StreamingServer) handleQualityPlaylist(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	parts := strings.Split(strings.Trim(path, "/"), "/")

	if len(parts) < 3 {
		http.Error(w, "Invalid path format", http.StatusBadRequest)
		return
	}

	trackID := parts[0]
	quality := parts[1]

	if quality != "low" && quality != "med" && quality != "high" {
		http.Error(w, "Invalid quality", http.StatusBadRequest)
		return
	}

	s.ensureTrackPreloaded(trackID)

	_, err := s.loadTrackMetadata(trackID)
	if err != nil {
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	key := fmt.Sprintf("%s%s/%s/playlist.m3u8", s.hlsPrefix, trackID, quality)
	data, err := s.fetchFromLocalFiles(key)
	if err != nil {
		log.Printf("Error fetching quality playlist: %v", err)
		http.Error(w, "Playlist not found", http.StatusNotFound)
		return
	}

	fixedData := s.fixSegmentURLs(data, trackID, quality)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "max-age=3600")

	w.Write(fixedData)
	log.Printf("Served %s quality playlist for track: %s", quality, trackID)
}

func (s *StreamingServer) fixSegmentURLs(content []byte, trackID, quality string) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	var result strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasSuffix(line, ".ts") && !strings.HasPrefix(line, "/") {
			line = fmt.Sprintf("/stream/%s/%s/%s", trackID, quality, line)
		}

		result.WriteString(line)
		result.WriteString("\n")
	}

	return []byte(result.String())
}

func (s *StreamingServer) handleSegment(w http.ResponseWriter, r *http.Request) {
	clientIP := getClientIP(r)
	if !s.rateLimiter.Allow(clientIP) {
		http.Error(w, "Rate limited", http.StatusTooManyRequests)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	parts := strings.Split(strings.Trim(path, "/"), "/")

	if len(parts) < 3 {
		http.Error(w, "Invalid segment path format", http.StatusBadRequest)
		return
	}

	trackID := parts[0]
	quality := parts[1]
	segmentFile := parts[2]

	if quality != "low" && quality != "med" && quality != "high" {
		http.Error(w, "Invalid quality", http.StatusBadRequest)
		return
	}

	if !strings.HasSuffix(segmentFile, ".ts") {
		http.Error(w, "Invalid segment file", http.StatusBadRequest)
		return
	}

	cacheKey := fmt.Sprintf("%s/%s/%s", trackID, quality, segmentFile)

	// Check cache first
	if cachedData, ok := s.segmentCache.Get(cacheKey); ok {
		data := cachedData.([]byte)
		s.updateBandwidthStats(len(data), true)
		serveSegment(w, data)
		return
	}

	// Fetch from local files
	key := fmt.Sprintf("%s%s/%s/%s", s.hlsPrefix, trackID, quality, segmentFile)
	data, err := s.fetchFromLocalFiles(key)
	if err != nil {
		log.Printf("Error fetching segment: %v", err)
		http.Error(w, "Segment not found", http.StatusNotFound)
		return
	}

	s.updateBandwidthStats(len(data), false)
	s.segmentCache.Set(cacheKey, data, s.cacheExpiry)
	serveSegment(w, data)
}

func getClientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.Split(ip, ",")[0]
	}
	return strings.Split(r.RemoteAddr, ":")[0]
}

func (s *StreamingServer) updateBandwidthStats(size int, isCacheHit bool) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.stats.BandwidthUsage += int64(size)
	if isCacheHit {
		s.stats.CacheHits++
	} else {
		s.stats.CacheMisses++
	}
}

func serveSegment(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "video/MP2T")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (s *StreamingServer) handleDirectFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/file/")
	trackID := strings.Trim(path, "/")

	s.ensureTrackPreloaded(trackID)

	_, err := s.loadTrackMetadata(trackID)
	if err != nil {
		log.Printf("Error loading track metadata for %s: %v", trackID, err)
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	s.updateStats(trackID)

	fileExt := getFileExtension(r)
	key := findExistingFile(s, trackID, fileExt)
	if key == "" {
		s.fallbackToHLS(w, r, trackID)
		return
	}

	// Serve file directly
	data, err := s.fetchFromLocalFiles(key)
	if err != nil {
		log.Printf("Error reading file %s: %v", key, err)
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	contentType := getContentTypeByExtension(key)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Write(data)
	log.Printf("Served direct file: %s", key)
}

func getFileExtension(r *http.Request) string {
	if ext := r.URL.Query().Get("format"); ext != "" {
		return strings.TrimPrefix(ext, ".")
	}
	return "m4a"
}

func findExistingFile(s *StreamingServer, trackID, fileExt string) string {
	// Check only for raw/<trackID>.<format> (flat structure)
	key := fmt.Sprintf("%s%s.%s", s.rawPrefix, trackID, fileExt)
	exists, err := s.fileHandler.FileExists(key)
	if err == nil && exists {
		return key
	}
	return ""
}

func (s *StreamingServer) fallbackToHLS(w http.ResponseWriter, r *http.Request, trackID string) {
	hlsMasterKey := fmt.Sprintf("%s%s/playlist.m3u8", s.hlsPrefix, trackID)
	
	exists, err := s.fileHandler.FileExists(hlsMasterKey)
	if err != nil || !exists {
		http.Error(w, "Track not found in either raw or HLS format", http.StatusNotFound)
		return
	}

	log.Printf("Falling back to HLS for track %s", trackID)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "max-age=300")

	data, err := s.fetchFromLocalFiles(hlsMasterKey)
	if err != nil {
		http.Error(w, "Failed to fetch HLS playlist", http.StatusInternalServerError)
		return
	}

	fixedData := s.fixPlaylistURLs(data, trackID, false)
	w.Write(fixedData)
}

func (s *StreamingServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s.statsMu.RLock()
	uptime := time.Since(s.stats.StartTime).Round(time.Second)
	s.statsMu.RUnlock()

	health := map[string]interface{}{
		"status":             "healthy",
		"timestamp":          time.Now().Format(time.RFC3339),
		"tracks_loaded":      syncMapLen(&s.tracks),
		"uptime":             uptime.String(),
	}

	json.NewEncoder(w).Encode(health)
}

func syncMapLen(m *sync.Map) int {
	count := 0
	m.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

func (s *StreamingServer) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s.statsMu.RLock()
	stats := *s.stats // Make a copy
	stats.Uptime = time.Since(s.stats.StartTime).Round(time.Second).String()
	s.statsMu.RUnlock()

	json.NewEncoder(w).Encode(stats)
}

func (s *StreamingServer) handleCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusOK)
}

func (s *StreamingServer) handleTrackList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	tracks := make([]*AudioTrack, 0)
	s.tracks.Range(func(_, value interface{}) bool {
		tracks = append(tracks, value.(*AudioTrack))
		return true
	})

	json.NewEncoder(w).Encode(tracks)
}

func (s *StreamingServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseMultipartForm(32 << 20) // 32MB max
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	relPath := r.FormValue("path")
	if relPath == "" {
		relPath = handler.Filename
	}

	// Prevent path traversal
	relPath = filepath.Clean(relPath)
	if strings.Contains(relPath, "..") {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(s.fileHandler.basePath, relPath)
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	f, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	_, err = io.Copy(f, file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Upload successful"))
}

func loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		defer func() {
			if err := recover(); err != nil {
				log.Printf("Panic recovered: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)

		duration := time.Since(start)
		log.Printf("%s %s %v", r.Method, r.URL.Path, duration)
	})
}

func loggingMiddlewareHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		duration := time.Since(start)
		log.Printf("%s %s %v", r.Method, r.URL.Path, duration)
	})
}

func (s *StreamingServer) startBackgroundPreloading() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		for range ticker.C {
			s.discoverNewTracks()
		}
	}()
}

func (s *StreamingServer) discoverNewTracks() {
	// Implementation for discovering new tracks periodically
	// Similar to preloadAllTracks but only adds new ones
}

type serverConfig struct {
	hlsPrefix       string
	rawPrefix       string
	localAssetsPath string
	port            string
	staticDir       string
	preloadOnStartup bool
}

func loadConfig() *serverConfig {
	cfg := &serverConfig{
		hlsPrefix:       getEnvWithDefault("HLS_PREFIX", "hls/"),
		rawPrefix:       getEnvWithDefault("RAW_PREFIX", "raw/"),
		localAssetsPath: getEnvWithDefault("LOCAL_ASSETS_PATH", "/app/mwonya_assets"),
		port:            getEnvWithDefault("PORT", DefaultPort),
		staticDir:       "./static",
		preloadOnStartup: getEnvBoolWithDefault("PRELOAD_ON_STARTUP", true),
	}

	return cfg
}

func getEnvBoolWithDefault(key string, defaultValue bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return strings.ToLower(value) == "true"
}

func getEnvWithDefault(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func serveStaticFiles(staticDir string) {
	if _, err := os.Stat(staticDir); os.IsNotExist(err) {
		log.Printf("Warning: Static directory '%s' not found, skipping static file serving", staticDir)
	} else {
		fs := http.FileServer(http.Dir(staticDir))
		http.Handle("/", loggingMiddlewareHandler(fs))
		log.Printf("Serving static files from %s directory", staticDir)
	}
}

func setupRoutes(server *StreamingServer) {
	http.HandleFunc("/stream/", loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			server.handleCORS(w, r)
			return
		}

		path := r.URL.Path
		streamPath := strings.TrimPrefix(path, "/stream/")
		streamPath = strings.Trim(streamPath, "/")
		parts := strings.Split(streamPath, "/")

		switch {
		case len(parts) == 2 && parts[1] == "playlist.m3u8":
			server.handleMasterPlaylist(w, r)
		case len(parts) == 3 && parts[2] == "playlist.m3u8":
			server.handleQualityPlaylist(w, r)
		case len(parts) == 3 && strings.HasSuffix(parts[2], ".ts"):
			server.handleSegment(w, r)
		default:
			log.Printf("Invalid stream path: %s (parts: %v)", path, parts)
			http.Error(w, "Invalid stream path", http.StatusBadRequest)
		}
	}))

	http.HandleFunc("/file/", loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			server.handleCORS(w, r)
			return
		}
		server.handleDirectFile(w, r)
	}))

	http.HandleFunc("/upload", loggingMiddleware(server.handleUpload))

	http.Handle("/assets/", loggingMiddlewareHandler(http.StripPrefix("/assets/", http.FileServer(http.Dir(server.fileHandler.basePath)))))

	http.HandleFunc("/health", loggingMiddleware(server.handleHealth))
	http.HandleFunc("/stats", loggingMiddleware(server.handleStats))
	http.HandleFunc("/tracks", loggingMiddleware(server.handleTrackList))
}

func startBackgroundTasks(server *StreamingServer) {
	ticker := time.NewTicker(DefaultCleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		server.rateLimiter.Cleanup()
		
		server.statsMu.Lock()
		server.stats.ActiveStreams = server.stats.ActiveStreams / 2
		if server.stats.ActiveStreams < 0 {
			server.stats.ActiveStreams = 0
		}
		server.statsMu.Unlock()
	}
}

func logServerInfo(config *serverConfig, server *StreamingServer) {
	log.Printf("🚀 Local Audio Streaming Server starting on port %s", config.port)
	log.Printf("📁 Local Assets Path: %s", config.localAssetsPath)
	log.Printf("📁 HLS Prefix: %s", config.hlsPrefix)
	log.Printf("📁 Raw Prefix: %s", config.rawPrefix)
	log.Printf("🎵 HLS Stream URL format: http://localhost:%s/stream/TRACK_ID/playlist.m3u8", config.port)
	log.Printf("🎵 Direct File URL format: http://localhost:%s/file/TRACK_ID?format=mp3", config.port)
	log.Printf("📁 Assets URL format: http://localhost:%s/assets/<path>", config.port)
	log.Printf("📤 Upload URL: http://localhost:%s/upload (POST with 'file' and optional 'path'; saves to %s/<path> or %s/<filename>)", config.port, config.localAssetsPath, config.localAssetsPath)
	log.Printf("❤️  Health check: http://localhost:%s/health", config.port)
	log.Printf("📊 Stats: http://localhost:%s/stats", config.port)
	log.Printf("🎶 Track list: http://localhost:%s/tracks", config.port)
	log.Printf("⚡ Preloading enabled: %v", config.preloadOnStartup)
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Printf("Warning: Error loading .env file: %v", err)
	}

	config := loadConfig()

	server, err := NewStreamingServer(
		config.hlsPrefix,
		config.rawPrefix,
		config.localAssetsPath,
	)
	if err != nil {
		log.Fatalf("Failed to create streaming server: %v", err)
	}

	// Preload all tracks at startup if configured
	if config.preloadOnStartup {
		server.preloadAllTracks()
	}

	serveStaticFiles(config.staticDir)
	setupRoutes(server)

	go startBackgroundTasks(server)
	go server.startBackgroundPreloading()

	logServerInfo(config, server)
	log.Fatal(http.ListenAndServe(":"+config.port, nil))
}