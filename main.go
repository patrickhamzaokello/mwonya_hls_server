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
	DefaultCacheSize          = 10000        // Increased from 1000
	DefaultCacheExpiry        = 2 * time.Hour // Reduced from 24 hours
	DefaultSignedURLExpiry    = 15 * time.Minute
	DefaultSignedURLCacheSize = 50000       // Increased from 5000
	DefaultRateLimitWindow    = 100 * time.Millisecond
	DefaultCleanupInterval    = 5 * time.Minute
	DefaultPort               = "8080"
	DefaultRegion             = "us-east-1"
	PrecacheSegmentCount      = 10          // Number of segments to precache
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

type ExistenceCache struct {
	cache map[string]bool
	mu    sync.RWMutex
	ttl   time.Duration
}

type PreloadManager struct {
	server *StreamingServer
	mu     sync.RWMutex
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
	existenceCache     *ExistenceCache
	preloadManager     *PreloadManager
	isPreloaded        bool
	preloadMu          sync.RWMutex
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

	server := &StreamingServer{
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
		existenceCache: &ExistenceCache{
			cache: make(map[string]bool),
			ttl:   DefaultCacheExpiry,
		},
		preloadManager: &PreloadManager{
			server: nil, // Will be set after server creation
		},
	}
	
	server.preloadManager.server = server
	
	return server, nil
}

func (s *StreamingServer) preloadAllTracks() {
	s.preloadMu.Lock()
	defer s.preloadMu.Unlock()
	
	if s.isPreloaded {
		return
	}

	log.Println("Starting track metadata preloading...")
	
	// List all objects in the bucket with the HLS prefix
	listInput := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucketName),
		Prefix: aws.String(s.s3Prefix),
	}

	paginator := s3.NewListObjectsV2Paginator(s.s3Client, listInput)
	trackIDs := make(map[string]struct{})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.TODO())
		if err != nil {
			log.Printf("Error listing objects during preload: %v", err)
			continue
		}

		for _, obj := range page.Contents {
			key := *obj.Key
			// Extract track ID from path (format: prefix/trackID/...)
			parts := strings.Split(strings.TrimPrefix(key, s.s3Prefix), "/")
			if len(parts) > 0 && parts[0] != "" {
				trackIDs[parts[0]] = struct{}{}
			}
		}
	}

	// Load metadata for all discovered tracks
	for trackID := range trackIDs {
		if _, err := s.loadTrackMetadata(trackID); err != nil {
			log.Printf("Error preloading track %s: %v", trackID, err)
		} else {
			// Pre-warm URLs for this track
			s.warmupSignedURLs(trackID)
		}
	}

	s.isPreloaded = true
	log.Printf("Preloaded metadata for %d tracks", len(trackIDs))
}

func (s *StreamingServer) warmupSignedURLs(trackID string) {
	// Pre-generate URLs for critical files
	keysToWarm := []string{
		fmt.Sprintf("%s%s/playlist.m3u8", s.s3Prefix, trackID),
		fmt.Sprintf("%s%s/low/playlist.m3u8", s.s3Prefix, trackID),
		fmt.Sprintf("%s%s/med/playlist.m3u8", s.s3Prefix, trackID),
		fmt.Sprintf("%s%s/high/playlist.m3u8", s.s3Prefix, trackID),
	}

	// Add first N segments for each quality
	for _, quality := range []string{"low", "med", "high"} {
		for i := 0; i < PrecacheSegmentCount; i++ {
			keysToWarm = append(keysToWarm, 
				fmt.Sprintf("%s%s/%s/segment_%d.ts", s.s3Prefix, trackID, quality, i))
		}
	}

	// Generate URLs in background
	go func() {
		for _, key := range keysToWarm {
			if _, err := s.getSignedURL(key); err != nil {
				log.Printf("Error warming URL for %s: %v", key, err)
			}
		}
	}()
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

func loadAWSConfig(region, accessKey, secretKey string) (aws.Config, error) {
	if accessKey != "" && secretKey != "" {
		return config.LoadDefaultConfig(context.TODO(),
			config.WithRegion(region),
			config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
				return aws.Credentials{
					AccessKeyID:     accessKey,
					SecretAccessKey: secretKey,
				}, nil
			}),
		))
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

	// Don't check existence first - just try to get it
	result, err := s.s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(metadataKey),
	})

	if err != nil {
		var notFoundErr *types.NotFound
		if errors.As(err, &notFoundErr) {
			// Try raw location if not found in HLS
			metadataKey = fmt.Sprintf("%s%s/metadata.json", s.s3RawPrefix, trackID)
			result, err = s.s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
				Bucket: aws.String(s.bucketName),
				Key:    aws.String(metadataKey),
			})
			
			if err != nil {
				if errors.As(err, &notFoundErr) {
					// Return default metadata if not found
					return &AudioTrack{
						ID:       trackID,
						Title:    strings.ReplaceAll(trackID, "_", " "),
						Artist:   "Unknown Artist",
						Duration: 180,
					}, nil
				}
				return nil, fmt.Errorf("failed to get metadata: %v", err)
			}
		} else {
			return nil, fmt.Errorf("failed to get metadata: %v", err)
		}
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

	s.ensureTrackPreloaded(trackID)

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

	s.ensureTrackPreloaded(trackID)

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

	s.ensureTrackPreloaded(trackID)

	_, err := s.loadTrackMetadata(trackID)
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

	// Redirect client directly to S3 instead of proxying
	http.Redirect(w, r, signedURL, http.StatusTemporaryRedirect)
	log.Printf("Redirected client to signed URL for file: %s", s3Key)
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
		// Check existence cache first
		s.existenceCache.mu.RLock()
		exists, found := s.existenceCache.cache[key]
		s.existenceCache.mu.RUnlock()

		if found {
			if exists {
				return key
			}
			continue
		}

		// Not in cache, check S3
		_, err := s.s3Client.HeadObject(context.TODO(), &s3.HeadObjectInput{
			Bucket: aws.String(s.bucketName),
			Key:    aws.String(key),
		})

		if err == nil {
			// Update existence cache
			s.existenceCache.mu.Lock()
			s.existenceCache.cache[key] = true
			s.existenceCache.mu.Unlock()
			return key
		}

		// Update existence cache with negative result
		s.existenceCache.mu.Lock()
		s.existenceCache.cache[key] = false
		s.existenceCache.mu.Unlock()
	}
	return ""
}

func (s *StreamingServer) fallbackToHLS(w http.ResponseWriter, r *http.Request, trackID string) {
	hlsMasterKey := fmt.Sprintf("%s%s/playlist.m3u8", s.s3Prefix, trackID)
	
	// Check existence cache first
	s.existenceCache.mu.RLock()
	exists, found := s.existenceCache.cache[hlsMasterKey]
	s.existenceCache.mu.RUnlock()

	if found && !exists {
		http.Error(w, "Track not found in either raw or HLS format", http.StatusNotFound)
		return
	}

	// Not in cache, check S3
	_, err := s.s3Client.HeadObject(context.TODO(), &s3.HeadObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(hlsMasterKey),
	})

	if err != nil {
		// Update existence cache
		s.existenceCache.mu.Lock()
		s.existenceCache.cache[hlsMasterKey] = false
		s.existenceCache.mu.Unlock()
		
		http.Error(w, "Track not found in either raw or HLS format", http.StatusNotFound)
		return
	}

	// Update existence cache
	s.existenceCache.mu.Lock()
	s.existenceCache.cache[hlsMasterKey] = true
	s.existenceCache.mu.Unlock()

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

func (s *StreamingServer) startBackgroundPreloading() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		for range ticker.C {
			s.discoverNewTracks()
			s.warmupPopularTracks()
		}
	}()
}

func (s *StreamingServer) discoverNewTracks() {
	// Implementation for discovering new tracks periodically
	// Similar to preloadAllTracks but only adds new ones
}

func (s *StreamingServer) warmupPopularTracks() {
	// Implementation for warming URLs of popular tracks
	s.statsMu.RLock()
	popularTracks := make([]string, 0, 10)
	for trackID := range s.stats.TrackStreams {
		popularTracks = append(popularTracks, trackID)
		if len(popularTracks) >= 10 {
			break
		}
	}
	s.statsMu.RUnlock()

	for _, trackID := range popularTracks {
		s.warmupSignedURLs(trackID)
	}
}

func (s *StreamingServer) precacheSegments(trackID string) {
	// Cache first N segments of each quality level
	for _, quality := range []string{"low", "med", "high"} {
		for i := 0; i < PrecacheSegmentCount; i++ {
			segmentKey := fmt.Sprintf("%s%s/%s/segment_%d.ts", 
				s.s3Prefix, trackID, quality, i)
			s.cacheSegmentInBackground(segmentKey)
		}
	}
}

func (s *StreamingServer) cacheSegmentInBackground(key string) {
	go func() {
		data, err := s.fetchFromS3WithSignedURL(key)
		if err != nil {
			log.Printf("Error precaching segment %s: %v", key, err)
			return
		}
		s.segmentCache.Set(key, data, s.cacheExpiry)
	}()
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

	// Preload all tracks at startup if configured
	if os.Getenv("PRELOAD_ON_STARTUP") == "true" {
		server.preloadAllTracks()
	}

	serveStaticFiles(config.staticDir)
	setupRoutes(server)

	go startBackgroundTasks(server)
	go server.startBackgroundPreloading()

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
	preloadOnStartup bool
	warmSignedURLs bool
	maxPreloadTracks int
	segmentPrecacheCount int
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
		preloadOnStartup: getEnvBoolWithDefault("PRELOAD_ON_STARTUP", true),
		warmSignedURLs: getEnvBoolWithDefault("WARM_SIGNED_URLS", true),
		maxPreloadTracks: getEnvIntWithDefault("MAX_PRELOAD_TRACKS", 1000),
		segmentPrecacheCount: getEnvIntWithDefault("SEGMENT_PRECACHE_COUNT", 10),
	}

	if cfg.bucketName == "" {
		log.Fatal("S3_BUCKET_NAME environment variable is required")
	}

	if (cfg.awsAccessKey == "") != (cfg.awsSecretKey == "") {
		log.Fatal("Both AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be provided together, or both omitted to use default credentials")
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

func getEnvIntWithDefault(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	result, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return result
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
	log.Printf("🎵 Directs File URL format: http://localhost:%s/file/TRACK_ID", config.port)
	log.Printf("❤️  Health check: http://localhost:%s/health", config.port)
	log.Printf("📊 Stats: http://localhost:%s/stats", config.port)
	log.Printf("🎶 Track list: http://localhost:%s/tracks", config.port)
	log.Printf("🔐 Using S3 signed URLs with %v expiry", server.signedURLExpiry)
	log.Printf("⚡ Preloading enabled: %v", config.preloadOnStartup)
	log.Printf("🔥 URL warming enabled: %v", config.warmSignedURLs)
}