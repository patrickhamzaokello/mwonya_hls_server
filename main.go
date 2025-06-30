package main

import (
	"bufio"
	"bytes"
	"context"
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
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/joho/godotenv"
	"golang.org/x/sync/singleflight"
)

// Optimized constants for high performance
const (
	DefaultCacheSize          = 10000    // Increased cache size
	DefaultCacheExpiry        = 2 * time.Hour // Longer cache expiry
	DefaultSignedURLExpiry    = 30 * time.Minute // Longer signed URL expiry
	DefaultSignedURLCacheSize = 50000    // Much larger signed URL cache
	DefaultPort               = "8080"
	DefaultRegion             = "us-east-1"
	
	// Buffer sizes for optimal performance
	ReadBufferSize  = 64 * 1024  // 64KB read buffer
	WriteBufferSize = 64 * 1024  // 64KB write buffer
	
	// Pre-allocated pools
	PooledBufferSize = 1024 * 1024 // 1MB pooled buffers
)

// Memory pools for zero-allocation operations
var (
	bufferPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, PooledBufferSize)
		},
	}
	
	stringBuilderPool = sync.Pool{
		New: func() interface{} {
			return &strings.Builder{}
		},
	}
)

type AudioTrack struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration"`
}

// Optimized cache with better eviction and memory management
type FastCache struct {
	items    map[string]*cacheItem
	mu       sync.RWMutex
	maxSize  int
	hitCount int64
	missCount int64
}

type cacheItem struct {
	value      []byte    // Store as bytes for zero-copy operations
	expiration int64     // Use int64 for faster comparison
	lastAccess int64     // For LRU eviction
}

func NewFastCache(maxSize int) *FastCache {
	return &FastCache{
		items:   make(map[string]*cacheItem, maxSize),
		maxSize: maxSize,
	}
}

func (c *FastCache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	item, exists := c.items[key]
	c.mu.RUnlock()
	
	if !exists {
		atomic.AddInt64(&c.missCount, 1)
		return nil, false
	}
	
	now := time.Now().UnixNano()
	if now > item.expiration {
		c.mu.Lock()
		delete(c.items, key)
		c.mu.Unlock()
		atomic.AddInt64(&c.missCount, 1)
		return nil, false
	}
	
	// Update last access time atomically
	atomic.StoreInt64(&item.lastAccess, now)
	atomic.AddInt64(&c.hitCount, 1)
	return item.value, true
}

func (c *FastCache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	// Fast eviction when cache is full
	if len(c.items) >= c.maxSize {
		c.evictLRU()
	}
	
	now := time.Now().UnixNano()
	c.items[key] = &cacheItem{
		value:      value,
		expiration: now + ttl.Nanoseconds(),
		lastAccess: now,
	}
}

func (c *FastCache) evictLRU() {
	if len(c.items) == 0 {
		return
	}
	
	var oldestKey string
	var oldestTime int64 = time.Now().UnixNano()
	
	// Find oldest item
	for k, v := range c.items {
		if v.lastAccess < oldestTime {
			oldestTime = v.lastAccess
			oldestKey = k
		}
	}
	
	if oldestKey != "" {
		delete(c.items, oldestKey)
	}
}

func (c *FastCache) Stats() (hits, misses int64) {
	return atomic.LoadInt64(&c.hitCount), atomic.LoadInt64(&c.missCount)
}

// Optimized streaming server with minimal allocations
type StreamingServer struct {
	s3Client        *s3.Client
	presignClient   *s3.PresignClient
	bucketName      string
	s3Prefix        string
	s3RawPrefix     string
	
	// Optimized storage
	tracks          sync.Map
	segmentCache    *FastCache
	signedURLCache  *FastCache
	
	// Reduced coordination overhead
	requestGroup    singleflight.Group
	
	// Configuration
	signedURLExpiry time.Duration
	
	// Atomic counters for zero-lock stats
	totalStreams    int64
	bandwidthUsage  int64
	startTime       int64
}

func NewStreamingServer(bucketName, s3Prefix, s3RawPrefix, region, accessKey, secretKey string) (*StreamingServer, error) {
	cfg, err := loadAWSConfig(region, accessKey, secretKey)
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %v", err)
	}

	s3Client := s3.NewFromConfig(cfg)
	presignClient := s3.NewPresignClient(s3Client)

	return &StreamingServer{
		s3Client:        s3Client,
		presignClient:   presignClient,
		bucketName:      bucketName,
		s3Prefix:        ensureTrailingSlash(s3Prefix),
		s3RawPrefix:     ensureTrailingSlash(s3RawPrefix),
		segmentCache:    NewFastCache(DefaultCacheSize),
		signedURLCache:  NewFastCache(DefaultSignedURLCacheSize),
		signedURLExpiry: DefaultSignedURLExpiry,
		startTime:       time.Now().UnixNano(),
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
	return config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
}

func ensureTrailingSlash(path string) string {
	if path != "" && !strings.HasSuffix(path, "/") {
		return path + "/"
	}
	return path
}

// Optimized signed URL generation with better caching
func (s *StreamingServer) getSignedURL(key string) (string, error) {
	// Check cache first
	if cached, ok := s.signedURLCache.Get(key); ok {
		return *(*string)(unsafe.Pointer(&cached)), nil
	}

	// Use singleflight to prevent duplicate requests
	result, err, _ := s.requestGroup.Do(key, func() (interface{}, error) {
		request, err := s.presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
			Bucket: aws.String(s.bucketName),
			Key:    aws.String(key),
		}, func(opts *s3.PresignOptions) {
			opts.Expires = s.signedURLExpiry
		})

		if err != nil {
			return nil, err
		}

		// Cache the URL as bytes for zero-copy operations
		urlBytes := []byte(request.URL)
		s.signedURLCache.Set(key, urlBytes, s.signedURLExpiry)
		return request.URL, nil
	})

	if err != nil {
		return "", err
	}
	return result.(string), nil
}

// Optimized file fetching with connection reuse
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

func (s *StreamingServer) fetchFromS3(key string) ([]byte, error) {
	signedURL, err := s.getSignedURL(key)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Get(signedURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	// Use pooled buffer for reading
	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)

	var result []byte
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			result = append(result, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

// Optimized segment serving with zero-copy operations
func (s *StreamingServer) handleSegment(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	parts := strings.Split(strings.Trim(path, "/"), "/")

	if len(parts) < 3 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	trackID := parts[0]
	quality := parts[1]
	segmentFile := parts[2]

	// Validate quality quickly
	if quality != "low" && quality != "med" && quality != "high" {
		http.Error(w, "Invalid quality", http.StatusBadRequest)
		return
	}

	if !strings.HasSuffix(segmentFile, ".ts") {
		http.Error(w, "Invalid segment", http.StatusBadRequest)
		return
	}

	cacheKey := trackID + "/" + quality + "/" + segmentFile

	// Check cache first
	if cachedData, ok := s.segmentCache.Get(cacheKey); ok {
		atomic.AddInt64(&s.bandwidthUsage, int64(len(cachedData)))
		s.serveSegmentFast(w, cachedData)
		return
	}

	// Fetch from S3
	s3Key := s.s3Prefix + trackID + "/" + quality + "/" + segmentFile
	data, err := s.fetchFromS3(s3Key)
	if err != nil {
		http.Error(w, "Segment not found", http.StatusNotFound)
		return
	}

	atomic.AddInt64(&s.bandwidthUsage, int64(len(data)))
	s.segmentCache.Set(cacheKey, data, DefaultCacheExpiry)
	s.serveSegmentFast(w, data)
}

// Optimized segment serving with pre-set headers
func (s *StreamingServer) serveSegmentFast(w http.ResponseWriter, data []byte) {
	h := w.Header()
	h["Content-Type"] = []string{"video/MP2T"}
	h["Access-Control-Allow-Origin"] = []string{"*"}
	h["Cache-Control"] = []string{"max-age=86400"}
	h["Accept-Ranges"] = []string{"bytes"}
	h["Content-Length"] = []string{strconv.Itoa(len(data))}
	
	w.Write(data)
}

// Optimized playlist handling with string builder pooling
func (s *StreamingServer) handleMasterPlaylist(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	trackID := strings.TrimSuffix(path, "/playlist.m3u8")
	trackID = strings.Trim(trackID, "/")

	atomic.AddInt64(&s.totalStreams, 1)

	h := w.Header()
	h["Content-Type"] = []string{"application/vnd.apple.mpegurl"}
	h["Access-Control-Allow-Origin"] = []string{"*"}
	h["Cache-Control"] = []string{"max-age=300"}

	// Try to get from S3 first
	s3Key := s.s3Prefix + trackID + "/playlist.m3u8"
	if data, err := s.fetchFromS3(s3Key); err == nil {
		fixedData := s.fixPlaylistURLs(data, trackID)
		w.Write(fixedData)
		return
	}

	// Generate master playlist
	sb := stringBuilderPool.Get().(*strings.Builder)
	defer func() {
		sb.Reset()
		stringBuilderPool.Put(sb)
	}()

	basePath := "/stream/" + trackID
	sb.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n\n")
	sb.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=80000,CODECS=\"mp4a.40.2\"\n")
	sb.WriteString(basePath + "/low/playlist.m3u8\n\n")
	sb.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=160000,CODECS=\"mp4a.40.2\"\n")
	sb.WriteString(basePath + "/med/playlist.m3u8\n\n")
	sb.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=384000,CODECS=\"mp4a.40.2\"\n")
	sb.WriteString(basePath + "/high/playlist.m3u8\n")

	w.Write([]byte(sb.String()))
}

// Optimized playlist URL fixing
func (s *StreamingServer) fixPlaylistURLs(content []byte, trackID string) []byte {
	sb := stringBuilderPool.Get().(*strings.Builder)
	defer func() {
		sb.Reset()
		stringBuilderPool.Put(sb)
	}()

	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasSuffix(line, ".ts") && !strings.HasPrefix(line, "/") {
			sb.WriteString("/stream/")
			sb.WriteString(trackID)
			sb.WriteByte('/')
			// Extract quality from context or use default
			sb.WriteString("high/")
			sb.WriteString(line)
		} else {
			sb.WriteString(line)
		}
		sb.WriteByte('\n')
	}

	return []byte(sb.String())
}

func (s *StreamingServer) handleQualityPlaylist(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	parts := strings.Split(strings.Trim(path, "/"), "/")

	if len(parts) < 3 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	trackID := parts[0]
	quality := parts[1]

	if quality != "low" && quality != "med" && quality != "high" {
		http.Error(w, "Invalid quality", http.StatusBadRequest)
		return
	}

	s3Key := s.s3Prefix + trackID + "/" + quality + "/playlist.m3u8"
	data, err := s.fetchFromS3(s3Key)
	if err != nil {
		http.Error(w, "Playlist not found", http.StatusNotFound)
		return
	}

	fixedData := s.fixSegmentURLs(data, trackID, quality)

	h := w.Header()
	h["Content-Type"] = []string{"application/vnd.apple.mpegurl"}
	h["Access-Control-Allow-Origin"] = []string{"*"}
	h["Cache-Control"] = []string{"max-age=3600"}

	w.Write(fixedData)
}

func (s *StreamingServer) fixSegmentURLs(content []byte, trackID, quality string) []byte {
	sb := stringBuilderPool.Get().(*strings.Builder)
	defer func() {
		sb.Reset()
		stringBuilderPool.Put(sb)
	}()

	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasSuffix(line, ".ts") && !strings.HasPrefix(line, "/") {
			sb.WriteString("/stream/")
			sb.WriteString(trackID)
			sb.WriteByte('/')
			sb.WriteString(quality)
			sb.WriteByte('/')
			sb.WriteString(line)
		} else {
			sb.WriteString(line)
		}
		sb.WriteByte('\n')
	}

	return []byte(sb.String())
}

// Optimized stats endpoint
func (s *StreamingServer) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	hits, misses := s.segmentCache.Stats()
	uptime := time.Duration(time.Now().UnixNano() - atomic.LoadInt64(&s.startTime))

	stats := map[string]interface{}{
		"total_streams":    atomic.LoadInt64(&s.totalStreams),
		"bandwidth_usage":  atomic.LoadInt64(&s.bandwidthUsage),
		"cache_hits":       hits,
		"cache_misses":     misses,
		"cache_hit_ratio":  float64(hits) / float64(hits+misses),
		"uptime_seconds":   uptime.Seconds(),
	}

	json.NewEncoder(w).Encode(stats)
}

// Optimized health check
func (s *StreamingServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write([]byte(`{"status":"healthy"}`))
}

func main() {
	// Load configuration
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Error loading .env file: %v", err)
	}

	bucketName := os.Getenv("S3_BUCKET_NAME")
	if bucketName == "" {
		log.Fatal("S3_BUCKET_NAME environment variable is required")
	}

	s3Prefix := getEnvWithDefault("S3_PREFIX", "hls/")
	s3RawPrefix := getEnvWithDefault("S3_RAW_PREFIX", "raw/")
	awsRegion := getEnvWithDefault("AWS_REGION", DefaultRegion)
	port := getEnvWithDefault("PORT", DefaultPort)

	// Create optimized server
	server, err := NewStreamingServer(
		bucketName,
		s3Prefix,
		s3RawPrefix,
		awsRegion,
		os.Getenv("AWS_ACCESS_KEY_ID"),
		os.Getenv("AWS_SECRET_ACCESS_KEY"),
	)
	if err != nil {
		log.Fatalf("Failed to create streaming server: %v", err)
	}

	// Setup optimized routes
	mux := http.NewServeMux()
	
	// Streaming endpoints
	mux.HandleFunc("/stream/", server.routeStream)
	mux.HandleFunc("/health", server.handleHealth)
	mux.HandleFunc("/stats", server.handleStats)

	// Configure server for high performance
	httpServer := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("🚀 Optimized Audio Streaming Server starting on port %s", port)
	log.Printf("☁️  S3 Bucket: %s", bucketName)
	log.Printf("📁 HLS Prefix: %s", s3Prefix)
	log.Printf("🎵 Stream URL: http://localhost:%s/stream/TRACK_ID/playlist.m3u8", port)
	log.Printf("📊 Stats: http://localhost:%s/stats", port)

	log.Fatal(httpServer.ListenAndServe())
}

// Optimized stream routing
func (s *StreamingServer) routeStream(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.WriteHeader(http.StatusOK)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	parts := strings.Split(strings.Trim(path, "/"), "/")

	switch len(parts) {
	case 2:
		if parts[1] == "playlist.m3u8" {
			s.handleMasterPlaylist(w, r)
			return
		}
	case 3:
		if parts[2] == "playlist.m3u8" {
			s.handleQualityPlaylist(w, r)
			return
		}
		if strings.HasSuffix(parts[2], ".ts") {
			s.handleSegment(w, r)
			return
		}
	}

	http.Error(w, "Invalid path", http.StatusBadRequest)
}

func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}