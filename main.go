package main

import (
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
	s3Prefix           string
	s3RawPrefix        string
	tracks             sync.Map
	segmentCache       *LRUCache
	signedURLCache     *LRUCache
	rateLimiter        *RateLimiter
	stats              *StreamingStats
	statsMu            sync.RWMutex
	requestGroup       singleflight.Group
	cacheExpiry        time.Duration
	signedURLExpiry    time.Duration
}

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
	visits sync.Map
	window time.Duration
}

func NewStreamingServer(bucketName, s3Prefix, s3RawPrefix, region, accessKey, secretKey string) (*StreamingServer, error) {
	cfg, err := loadAWSConfig(region, accessKey, secretKey)
	if err != nil {
		return nil, fmt.Errorf("AWS config error: %w", err)
	}

	s3Client := s3.NewFromConfig(cfg)
	presignClient := s3.NewPresignClient(s3Client)

	return &StreamingServer{
		s3Client:        s3Client,
		presignClient:   presignClient,
		bucketName:      bucketName,
		s3Prefix:        ensureTrailingSlash(s3Prefix),
		s3RawPrefix:     ensureTrailingSlash(s3RawPrefix),
		segmentCache:    NewLRUCache(DefaultCacheSize),
		signedURLCache:  NewLRUCache(DefaultSignedURLCacheSize),
		rateLimiter:     NewRateLimiter(DefaultRateLimitWindow),
		cacheExpiry:     DefaultCacheExpiry,
		signedURLExpiry: DefaultSignedURLExpiry,
		stats: &StreamingStats{
			TrackStreams: make(map[string]int64),
			StartTime:    time.Now(),
		},
	}, nil
}

// Improved LRUCache implementation
func NewLRUCache(maxSize int) *LRUCache {
	return &LRUCache{
		cache:  make(map[string]*cacheItem, maxSize),
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
		// Evict expired items first
		now := time.Now()
		for k, v := range c.cache {
			if now.After(v.expiration) {
				delete(c.cache, k)
			}
		}
		
		// If still full, remove random item
		if len(c.cache) >= c.maxSize {
			for k := range c.cache {
				delete(c.cache, k)
				break
			}
		}
	}

	c.cache[key] = &cacheItem{
		value:      value,
		expiration: time.Now().Add(ttl),
	}
}

// Optimized RateLimiter
func NewRateLimiter(window time.Duration) *RateLimiter {
	return &RateLimiter{
		window: window,
	}
}

func (rl *RateLimiter) Allow(ip string) bool {
	now := time.Now()
	
	if last, ok := rl.visits.Load(ip); ok {
		if now.Sub(last.(time.Time)) < rl.window {
			return false
		}
	}
	
	rl.visits.Store(ip, now)
	return true
}

func (rl *RateLimiter) Cleanup() {
	cutoff := time.Now().Add(-5 * time.Minute)
	rl.visits.Range(func(key, value interface{}) bool {
		if value.(time.Time).Before(cutoff) {
			rl.visits.Delete(key)
		}
		return true
	})
}

// Core streaming handlers
func (s *StreamingServer) handleMasterPlaylist(w http.ResponseWriter, r *http.Request) {
	trackID := extractTrackID(r.URL.Path)
	if trackID == "" {
		http.Error(w, "Invalid track ID", http.StatusBadRequest)
		return
	}

	track, err := s.loadTrackMetadata(trackID)
	if err != nil {
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	s.updateStats(trackID)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "max-age=300")
	setCommonHeaders(w)

	s3Key := fmt.Sprintf("%s%s/playlist.m3u8", s.s3Prefix, trackID)
	if data, err := s.fetchFromS3WithSignedURL(s3Key); err == nil {
		w.Write(s.fixPlaylistURLs(data, trackID, false))
	} else {
		w.Write([]byte(s.generateMasterPlaylist(trackID)))
	}
}

func (s *StreamingServer) handleQualityPlaylist(w http.ResponseWriter, r *http.Request) {
	trackID, quality := extractTrackQuality(r.URL.Path)
	if trackID == "" || quality == "" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	if _, err := s.loadTrackMetadata(trackID); err != nil {
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	s3Key := fmt.Sprintf("%s%s/%s/playlist.m3u8", s.s3Prefix, trackID, quality)
	data, err := s.fetchFromS3WithSignedURL(s3Key)
	if err != nil {
		http.Error(w, "Playlist not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "max-age=3600")
	setCommonHeaders(w)
	w.Write(s.fixSegmentURLs(data, trackID, quality))
}

func (s *StreamingServer) handleSegment(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimiter.Allow(getClientIP(r)) {
		http.Error(w, "Rate limited", http.StatusTooManyRequests)
		return
	}

	trackID, quality, segmentFile := extractSegmentInfo(r.URL.Path)
	if trackID == "" || quality == "" || segmentFile == "" {
		http.Error(w, "Invalid segment path", http.StatusBadRequest)
		return
	}

	cacheKey := fmt.Sprintf("%s/%s/%s", trackID, quality, segmentFile)
	if cachedData, ok := s.segmentCache.Get(cacheKey); ok {
		serveSegment(w, cachedData.([]byte))
		s.updateBandwidthStats(len(cachedData.([]byte)), true)
		return
	}

	s3Key := fmt.Sprintf("%s%s/%s/%s", s.s3Prefix, trackID, quality, segmentFile)
	data, err := s.fetchFromS3WithSignedURL(s3Key)
	if err != nil {
		http.Error(w, "Segment not found", http.StatusNotFound)
		return
	}

	s.segmentCache.Set(cacheKey, data, s.cacheExpiry)
	serveSegment(w, data)
	s.updateBandwidthStats(len(data), false)
}

// Optimized helper functions
func (s *StreamingServer) fetchFromS3WithSignedURL(key string) ([]byte, error) {
	if cached, ok := s.signedURLCache.Get(key); ok {
		return s.fetchFromURL(cached.(string))
	}

	url, err, _ := s.requestGroup.Do(key, func() (interface{}, error) {
		request, err := s.presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
			Bucket: aws.String(s.bucketName),
			Key:    aws.String(key),
		}, func(opts *s3.PresignOptions) {
			opts.Expires = s.signedURLExpiry
		})

		if err != nil {
			return nil, fmt.Errorf("failed to create signed URL: %w", err)
		}

		s.statsMu.Lock()
		s.stats.SignedURLRequests++
		s.statsMu.Unlock()

		s.signedURLCache.Set(key, request.URL, s.signedURLExpiry)
		return request.URL, nil
	})

	if err != nil {
		return nil, err
	}

	return s.fetchFromURL(url.(string))
}

func (s *StreamingServer) fetchFromURL(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch from URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (s *StreamingServer) loadTrackMetadata(trackID string) (*AudioTrack, error) {
	if cached, ok := s.tracks.Load(trackID); ok {
		return cached.(*AudioTrack), nil
	}

	track, err := s.loadTrackFromS3(trackID)
	if err != nil {
		return nil, err
	}

	s.tracks.Store(trackID, track)
	return track, nil
}

func (s *StreamingServer) loadTrackFromS3(trackID string) (*AudioTrack, error) {
	metadataKey := fmt.Sprintf("%s%s/metadata.json", s.s3Prefix, trackID)
	exists, err := s.fileExists(metadataKey)
	if err != nil {
		return nil, fmt.Errorf("metadata check failed: %w", err)
	}

	if !exists {
		metadataKey = fmt.Sprintf("%s%s/metadata.json", s.s3RawPrefix, trackID)
		if exists, err = s.fileExists(metadataKey); err != nil || !exists {
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
		return nil, fmt.Errorf("failed to get metadata: %w", err)
	}
	defer result.Body.Close()

	var track AudioTrack
	if err := json.NewDecoder(result.Body).Decode(&track); err != nil {
		return nil, fmt.Errorf("failed to decode metadata: %w", err)
	}

	return &track, nil
}

// Path extraction utilities
func extractTrackID(path string) string {
	path = strings.TrimPrefix(path, "/stream/")
	return strings.TrimSuffix(path, "/playlist.m3u8")
}

func extractTrackQuality(path string) (string, string) {
	parts := strings.Split(strings.TrimPrefix(path, "/stream/"), "/")
	if len(parts) < 3 {
		return "", ""
	}
	return parts[0], parts[1]
}

func extractSegmentInfo(path string) (string, string, string) {
	parts := strings.Split(strings.TrimPrefix(path, "/stream/"), "/")
	if len(parts) < 3 {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}

// Stats management
func (s *StreamingServer) updateStats(trackID string) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.stats.TotalStreams++
	s.stats.TrackStreams[trackID]++
	s.stats.ActiveStreams++
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

// Common utilities
func ensureTrailingSlash(path string) string {
	if path != "" && !strings.HasSuffix(path, "/") {
		return path + "/"
	}
	return path
}

func getClientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.Split(ip, ",")[0]
	}
	return strings.Split(r.RemoteAddr, ":")[0]
}

func setCommonHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
}

func serveSegment(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "video/MP2T")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
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

// Main function and server setup
func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("Note: No .env file found: %v", err)
	}

	cfg := loadConfig()
	server, err := NewStreamingServer(
		cfg.bucketName,
		cfg.s3Prefix,
		cfg.s3RawPrefix,
		cfg.awsRegion,
		cfg.awsAccessKey,
		cfg.awsSecretKey,
	)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	setupRoutes(server)
	go startBackgroundTasks(server)

	log.Printf("Server starting on :%s", cfg.port)
	log.Fatal(http.ListenAndServe(":"+cfg.port, nil))
}

func setupRoutes(server *StreamingServer) {
	http.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			server.handleCORS(w, r)
			return
		}

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/playlist.m3u8"):
			if strings.Count(path, "/") == 3 {
				server.handleQualityPlaylist(w, r)
			} else {
				server.handleMasterPlaylist(w, r)
			}
		case strings.HasSuffix(path, ".ts"):
			server.handleSegment(w, r)
		default:
			http.Error(w, "Invalid path", http.StatusBadRequest)
		}
	})

	http.HandleFunc("/health", server.handleHealth)
	http.HandleFunc("/stats", server.handleStats)
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