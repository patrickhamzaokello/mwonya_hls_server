package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/joho/godotenv"
	"golang.org/x/sync/singleflight"
)

// Constants
const (
	DefaultCacheSize          = 1000
	DefaultCacheExpiry        = 24 * time.Hour
	DefaultSignedURLExpiry    = 15 * time.Minute
	DefaultSignedURLCacheSize = 5000
	DefaultRateLimitWindow    = 100 * time.Millisecond
	DefaultCleanupInterval    = 5 * time.Minute
	DefaultPort               = "8080"
	DefaultRegion             = "us-east-1"
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
	SignedURLRequests int64            `json:"signed_url_requests"`
}

type StreamingServer struct {
	s3Client           *s3.Client
	presignClient      *s3.PresignClient
	bucketName         string
	s3Prefix           string // Folder path for HLS files
	s3RawPrefix        string // Folder path for raw/non-HLS files
	tracks             sync.Map // Thread-safe track storage
	segmentCache       *LRUCache
	signedURLCache     *LRUCache
	rateLimiter        *RateLimiter
	stats              *StreamingStats
	statsMu            sync.RWMutex
	requestGroup       singleflight.Group
	maxCacheSize       int
	cacheExpiry        time.Duration
	signedURLExpiry    time.Duration
	signedURLCacheSize int
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

func NewStreamingServer(bucketName, s3Prefix, s3RawPrefix, region, accessKey, secretKey string) (*StreamingServer, error) {
	cfg, err := loadAWSConfig(region, accessKey, secretKey)
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %v", err)
	}

	s3Client := s3.NewFromConfig(cfg)
	presignClient := s3.NewPresignClient(s3Client)

	// Ensure prefixes end with slash
	s3Prefix = ensureTrailingSlash(s3Prefix)
	s3RawPrefix = ensureTrailingSlash(s3RawPrefix)

	return &StreamingServer{
		s3Client:           s3Client,
		presignClient:      presignClient,
		bucketName:         bucketName,
		s3Prefix:           s3Prefix,
		s3RawPrefix:        s3RawPrefix,
		segmentCache:       NewLRUCache(DefaultCacheSize),
		signedURLCache:     NewLRUCache(DefaultSignedURLCacheSize),
		rateLimiter:        NewRateLimiter(DefaultRateLimitWindow),
		maxCacheSize:       DefaultCacheSize,
		cacheExpiry:        DefaultCacheExpiry,
		signedURLExpiry:    DefaultSignedURLExpiry,
		signedURLCacheSize: DefaultSignedURLCacheSize,
		stats: &StreamingStats{
			TrackStreams: make(map[string]int64),
			StartTime:    time.Now(),
		},
	}, nil
}

func loadAWSConfig(region, accessKey, secretKey string) (aws.Config, error) {
	if accessKey != "" && secretKey != "" {
		return config.LoadDefaultConfig(context.TODO(),
			config.WithRegion(region),
			config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
				return aws.Credentials{
					AccessKeyID:     accessKey,
					SecretAccessKey: secretKey,
				}, nil
			})),
		)
	}
	return config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(region),
	)
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
	default: return "application/octet-stream"
	}
}

func (s *StreamingServer) fileExists(key string) (bool, error) {
	_, err := s.s3Client.HeadObject(context.TODO(), &s3.HeadObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	})

	if err != nil {
		var awsErr *types.NotFound
		if errors.As(err, &awsErr) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *StreamingServer) getSignedURL(key string) (string, error) {
	// Check cache first
	if cached, ok := s.signedURLCache.Get(key); ok {
		return cached.(string), nil
	}

	// Use singleflight to prevent thundering herd
	url, err, _ := s.requestGroup.Do(key, func() (interface{}, error) {
		request, err := s.presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
			Bucket: aws.String(s.bucketName),
			Key:    aws.String(key),
		}, func(opts *s3.PresignOptions) {
			opts.Expires = s.signedURLExpiry
		})

		if err != nil {
			return nil, fmt.Errorf("failed to create signed URL for %s: %v", key, err)
		}

		s.statsMu.Lock()
		s.stats.SignedURLRequests++
		s.statsMu.Unlock()

		s.signedURLCache.Set(key, request.URL, s.signedURLExpiry)
		return request.URL, nil
	})

	if err != nil {
		return "", err
	}
	return url.(string), nil
}

func (s *StreamingServer) loadTrackMetadata(trackID string) (*AudioTrack, error) {
	// Check if track is already loaded
	if cached, ok := s.tracks.Load(trackID); ok {
		return cached.(*AudioTrack), nil
	}

	// Try to load from S3
	track, err := s.loadTrackFromS3(trackID)
	if err != nil {
		return nil, err
	}

	// Cache the loaded track
	s.tracks.Store(trackID, track)
	return track, nil
}

func (s *StreamingServer) loadTrackFromS3(trackID string) (*AudioTrack, error) {
	// Try HLS location first
	metadataKey := fmt.Sprintf("%s%s/metadata.json", s.s3Prefix, trackID)
	exists, err := s.fileExists(metadataKey)
	if err != nil {
		return nil, fmt.Errorf("error checking metadata in HLS location: %v", err)
	}

	if !exists {
		// Try raw location if not found in HLS
		metadataKey = fmt.Sprintf("%s%s/metadata.json", s.s3RawPrefix, trackID)
		exists, err = s.fileExists(metadataKey)
		if err != nil {
			return nil, fmt.Errorf("error checking metadata in raw location: %v", err)
		}
		if !exists {
			// Return default metadata if not found
			return &AudioTrack{
				ID:       trackID,
				Title:    strings.ReplaceAll(trackID, "_", " "),
				Artist:   "Unknown Artist",
				Duration: 180,
			}, nil
		}
	}

	result, err := s.s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(metadataKey),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata: %v", err)
	}
	defer result.Body.Close()

	var track AudioTrack
	if err := json.NewDecoder(result.Body).Decode(&track); err != nil {
		return nil, fmt.Errorf("failed to decode metadata: %v", err)
	}

	return &track, nil
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

func (s *StreamingServer) fetchFromS3WithSignedURL(key string) ([]byte, error) {
	exists, err := s.fileExists(key)
	if err != nil {
		return nil, fmt.Errorf("error checking file existence: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("file not found: %s", key)
	}

	signedURL, err := s.getSignedURL(key)
	if err != nil {
		return nil, fmt.Errorf("failed to generate signed URL: %w", err)
	}

	resp, err := http.Get(signedURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch from signed URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d from signed URL", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (s *StreamingServer) handleMasterPlaylist(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	trackID := strings.TrimSuffix(path, "/playlist.m3u8")
	trackID = strings.Trim(trackID, "/")

	track, err := s.loadTrackMetadata(trackID)
	if err != nil {
		log.Printf("Error loading track metadata for %s: %v", trackID, err)
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	s.updateStats(trackID)

	setCommonHeaders(w)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "max-age=300")

	s3Key := fmt.Sprintf("%s%s/playlist.m3u8", s.s3Prefix, trackID)
	if data, err := s.fetchFromS3WithSignedURL(s3Key); err == nil {
		fixedData := s.fixPlaylistURLs(data, trackID, false)
		w.Write(fixedData)
		log.Printf("Served master playlist from S3 for track: %s (%s)", track.Title, trackID)
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

func setCommonHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
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

	_, err := s.loadTrackMetadata(trackID)
	if err != nil {
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	s3Key := fmt.Sprintf("%s%s/%s/playlist.m3u8", s.s3Prefix, trackID, quality)
	data, err := s.fetchFromS3WithSignedURL(s3Key)
	if err != nil {
		log.Printf("Error fetching quality playlist from S3: %v", err)
		http.Error(w, "Playlist not found", http.StatusNotFound)
		return
	}

	fixedData := s.fixSegmentURLs(data, trackID, quality)

	setCommonHeaders(w)
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

	// Fetch from S3
	s3Key := fmt.Sprintf("%s%s/%s/%s", s.s3Prefix, trackID, quality, segmentFile)
	data, err := s.fetchFromS3WithSignedURL(s3Key)
	if err != nil {
		log.Printf("Error fetching segment from S3: %v", err)
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

    _, err := s.loadTrackMetadata(trackID)  // Removed unused track variable
    if err != nil {
        log.Printf("Error loading track metadata for %s: %v", trackID, err)
        http.Error(w, "Track not found", http.StatusNotFound)
        return
    }

    s.updateStats(trackID)

    fileExt := getFileExtension(r)
    s3Key := findExistingFile(s, trackID, fileExt)
    if s3Key == "" {
        s.fallbackToHLS(w, r, trackID)
        return
    }

    signedURL, err := s.getSignedURL(s3Key)
    if err != nil {
        log.Printf("Error generating signed URL for %s: %v", s3Key, err)
        http.Error(w, "File not found", http.StatusNotFound)
        return
    }

    log.Printf("Successfully found file at: %s", s3Key)
    s.proxyFile(w, r, signedURL, fmt.Sprintf("%s.%s", trackID, fileExt))
}

func getFileExtension(r *http.Request) string {
	if ext := r.URL.Query().Get("format"); ext != "" {
		return strings.TrimPrefix(ext, ".")
	}
	return "m4a"
}

func findExistingFile(s *StreamingServer, trackID, fileExt string) string {
	possibleKeys := []string{
		fmt.Sprintf("%s%s/%s.%s", s.s3RawPrefix, trackID, trackID, fileExt),
		fmt.Sprintf("%s%s.%s", s.s3RawPrefix, trackID, fileExt),
	}

	for _, key := range possibleKeys {
		exists, err := s.fileExists(key)
		if err != nil {
			log.Printf("Error checking file existence for %s: %v", key, err)
			continue
		}
		if exists {
			return key
		}
	}
	return ""
}

func (s *StreamingServer) proxyFile(w http.ResponseWriter, r *http.Request, signedURL, filename string) {
	req, err := http.NewRequest("GET", signedURL, nil)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Forward range header if present
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Failed to fetch file", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// Copy relevant headers
	copyHeaders(w, resp.Header)

	// Set content type if not already set
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", getContentTypeByExtension(filename))
	}

	setCommonHeaders(w)
	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("Error streaming file: %v", err)
	}
}

func copyHeaders(dst http.ResponseWriter, src http.Header) {
	for k, v := range src {
		if strings.HasPrefix(k, "Content-") || k == "Accept-Ranges" || k == "Content-Disposition" {
			dst.Header()[k] = v
		}
	}
}

func (s *StreamingServer) fallbackToHLS(w http.ResponseWriter, r *http.Request, trackID string) {
	hlsMasterKey := fmt.Sprintf("%s%s/playlist.m3u8", s.s3Prefix, trackID)
	exists, err := s.fileExists(hlsMasterKey)
	if err != nil {
		log.Printf("Error checking HLS existence for %s: %v", trackID, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if !exists {
		http.Error(w, "Track not found in either raw or HLS format", http.StatusNotFound)
		return
	}

	log.Printf("Falling back to HLS for track %s", trackID)

	setCommonHeaders(w)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "max-age=300")

	data, err := s.fetchFromS3WithSignedURL(hlsMasterKey)
	if err != nil {
		http.Error(w, "Failed to fetch HLS playlist", http.StatusInternalServerError)
		return
	}

	fixedData := s.fixPlaylistURLs(data, trackID, false)
	w.Write(fixedData)
}

func (s *StreamingServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	setCommonHeaders(w)
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
	setCommonHeaders(w)
	w.Header().Set("Content-Type", "application/json")

	s.statsMu.RLock()
	stats := *s.stats // Make a copy
	stats.Uptime = time.Since(s.stats.StartTime).Round(time.Second).String()
	s.statsMu.RUnlock()

	json.NewEncoder(w).Encode(stats)
}

func (s *StreamingServer) handleCORS(w http.ResponseWriter, r *http.Request) {
	setCommonHeaders(w)
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusOK)
}

func (s *StreamingServer) handleTrackList(w http.ResponseWriter, r *http.Request) {
	setCommonHeaders(w)
	w.Header().Set("Content-Type", "application/json")

	tracks := make([]*AudioTrack, 0)
	s.tracks.Range(func(_, value interface{}) bool {
		tracks = append(tracks, value.(*AudioTrack))
		return true
	})

	json.NewEncoder(w).Encode(tracks)
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

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Printf("Warning: Error loading .env file: %v", err)
	}

	config := loadConfig()

	server, err := NewStreamingServer(
		config.bucketName,
		config.s3Prefix,
		config.s3RawPrefix,
		config.awsRegion,
		config.awsAccessKey,
		config.awsSecretKey,
	)
	if err != nil {
		log.Fatalf("Failed to create streaming server: %v", err)
	}

	serveStaticFiles(config.staticDir)
	setupRoutes(server)

	go startBackgroundTasks(server)

	logServerInfo(config, server)
	log.Fatal(http.ListenAndServe(":"+config.port, nil))
}

type serverConfig struct {
	bucketName   string
	s3Prefix     string
	s3RawPrefix  string
	awsRegion    string
	awsAccessKey string
	awsSecretKey string
	port         string
	staticDir    string
}

func loadConfig() *serverConfig {
	cfg := &serverConfig{
		bucketName:   os.Getenv("S3_BUCKET_NAME"),
		s3Prefix:     getEnvWithDefault("S3_PREFIX", "hls"),
		s3RawPrefix:  getEnvWithDefault("S3_RAW_PREFIX", "raw/"),
		awsRegion:    getEnvWithDefault("AWS_REGION", DefaultRegion),
		awsAccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		awsSecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		port:         getEnvWithDefault("PORT", DefaultPort),
		staticDir:    "./static",
	}

	if cfg.bucketName == "" {
		log.Fatal("S3_BUCKET_NAME environment variable is required")
	}

	if (cfg.awsAccessKey == "") != (cfg.awsSecretKey == "") {
		log.Fatal("Both AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be provided together, or both omitted to use default credentials")
	}

	return cfg
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
		http.Handle("/", fs)
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
	log.Printf("🚀 Production Audio Streaming Server starting on port %s", config.port)
	log.Printf("☁️  AWS S3 Bucket: %s (Private with Signed URLs)", config.bucketName)
	log.Printf("📁 HLS Prefix: %s", config.s3Prefix)
	log.Printf("📁 Raw Prefix: %s", config.s3RawPrefix)
	log.Printf("🌍 AWS Region: %s", config.awsRegion)
	if config.awsAccessKey != "" {
		log.Printf("🔑 Using AWS credentials from environment variables")
	} else {
		log.Printf("🔑 Using default AWS credentials (IAM role/profile)")
	}
	log.Printf("🎵 HLS Stream URL format: http://localhost:%s/stream/TRACK_ID/playlist.m3u8", config.port)
	log.Printf("🎵 Direct File URL format: http://localhost:%s/file/TRACK_ID", config.port)
	log.Printf("❤️  Health check: http://localhost:%s/health", config.port)
	log.Printf("📊 Stats: http://localhost:%s/stats", config.port)
	log.Printf("🎶 Track list: http://localhost:%s/tracks", config.port)
	log.Printf("🔐 Using S3 signed URLs with %v expiry", server.signedURLExpiry)
}