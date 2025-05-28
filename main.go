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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type AudioTrack struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration"` // in seconds
}

type StreamingServer struct {
	s3Client       *s3.Client
	presignClient  *s3.PresignClient
	bucketName     string
	s3Prefix       string // General folder path in S3
	tracks         map[string]*AudioTrack
	segmentCache   map[string][]byte
	signedURLCache map[string]*SignedURLCacheItem
	rateLimiter    map[string]time.Time
	mu             sync.RWMutex
	
	// Cache configuration
	maxCacheSize       int
	cacheExpiry        time.Duration
	signedURLExpiry    time.Duration
	signedURLCacheSize int
}

type SignedURLCacheItem struct {
	url       string
	expiresAt time.Time
}

type CacheItem struct {
	data      []byte
	timestamp time.Time
}

func NewStreamingServer(bucketName, s3Prefix, region, accessKey, secretKey string) (*StreamingServer, error) {
	// Load AWS config with credentials from environment
	var cfg aws.Config
	var err error
	
	if accessKey != "" && secretKey != "" {
		// Use credentials from environment variables
		cfg, err = config.LoadDefaultConfig(context.TODO(),
			config.WithRegion(region),
			config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
				return aws.Credentials{
					AccessKeyID:     accessKey,
					SecretAccessKey: secretKey,
				}, nil
			})),
		)
	} else {
		// Fallback to default AWS config (IAM roles, etc.)
		cfg, err = config.LoadDefaultConfig(context.TODO(),
			config.WithRegion(region),
		)
	}
	
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %v", err)
	}

	s3Client := s3.NewFromConfig(cfg)
	presignClient := s3.NewPresignClient(s3Client)

	// Ensure s3Prefix ends with / if not empty
	if s3Prefix != "" && !strings.HasSuffix(s3Prefix, "/") {
		s3Prefix = s3Prefix + "/"
	}

	return &StreamingServer{
		s3Client:           s3Client,
		presignClient:      presignClient,
		bucketName:         bucketName,
		s3Prefix:           s3Prefix,
		tracks:             make(map[string]*AudioTrack),
		segmentCache:       make(map[string][]byte),
		signedURLCache:     make(map[string]*SignedURLCacheItem),
		rateLimiter:        make(map[string]time.Time),
		maxCacheSize:       1000, // Maximum cached segments
		cacheExpiry:        24 * time.Hour,
		signedURLExpiry:    15 * time.Minute, // Signed URLs expire in 15 minutes
		signedURLCacheSize: 5000, // Maximum cached signed URLs
	}, nil
}

// Generate signed URL for S3 object
func (s *StreamingServer) getSignedURL(key string) (string, error) {
	// Check cache first
	s.mu.RLock()
	if cached, exists := s.signedURLCache[key]; exists {
		if time.Now().Before(cached.expiresAt.Add(-2*time.Minute)) { // Refresh 2 minutes before expiry
			s.mu.RUnlock()
			return cached.url, nil
		}
	}
	s.mu.RUnlock()

	// Generate new signed URL
	request, err := s.presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = s.signedURLExpiry
	})

	if err != nil {
		return "", fmt.Errorf("failed to create signed URL for %s: %v", key, err)
	}

	// Cache the signed URL
	s.mu.Lock()
	// Clean up cache if it's getting too large
	if len(s.signedURLCache) >= s.signedURLCacheSize {
		// Remove expired entries
		now := time.Now()
		for k, v := range s.signedURLCache {
			if now.After(v.expiresAt) {
				delete(s.signedURLCache, k)
			}
		}
		
		// If still too large, remove oldest entries
		if len(s.signedURLCache) >= s.signedURLCacheSize {
			count := 0
			for k := range s.signedURLCache {
				delete(s.signedURLCache, k)
				count++
				if count >= s.signedURLCacheSize/4 { // Remove 25% of cache
					break
				}
			}
		}
	}

	s.signedURLCache[key] = &SignedURLCacheItem{
		url:       request.URL,
		expiresAt: time.Now().Add(s.signedURLExpiry),
	}
	s.mu.Unlock()

	return request.URL, nil
}

// Load track metadata from S3 or database
func (s *StreamingServer) loadTrackMetadata(trackID string) (*AudioTrack, error) {
	s.mu.RLock()
	if track, exists := s.tracks[trackID]; exists {
		s.mu.RUnlock()
		return track, nil
	}
	s.mu.RUnlock()

	// Try to fetch metadata from S3
	metadataKey := fmt.Sprintf("%s%s/metadata.json", s.s3Prefix, trackID)
	
	result, err := s.s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(metadataKey),
	})
	
	if err != nil {
		// If no metadata file, create default metadata
		track := &AudioTrack{
			ID:       trackID,
			Title:    strings.ReplaceAll(trackID, "_", " "),
			Artist:   "Unknown Artist",
			Duration: 180, // Default 3 minutes
		}
		
		s.mu.Lock()
		s.tracks[trackID] = track
		s.mu.Unlock()
		
		return track, nil
	}
	defer result.Body.Close()

	var track AudioTrack
	if err := json.NewDecoder(result.Body).Decode(&track); err != nil {
		return nil, fmt.Errorf("failed to decode metadata: %v", err)
	}

	s.mu.Lock()
	s.tracks[trackID] = &track
	s.mu.Unlock()

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

// Fix playlist URLs to work with the server's routing
func (s *StreamingServer) fixPlaylistURLs(content []byte, trackID string, isSegment bool) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	var result strings.Builder
	
	for scanner.Scan() {
		line := scanner.Text()
		
		// Skip comment lines and empty lines
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			result.WriteString(line)
			result.WriteString("\n")
			continue
		}
		
		// Handle master playlist URLs (absolute paths starting with /)
		if strings.HasPrefix(line, "/") && strings.HasSuffix(line, "/playlist.m3u8") {
			// Extract the parts from the absolute path
			// Example: /mwonya_audio/track_03eea3e1ce1e/low/playlist.m3u8
			parts := strings.Split(strings.Trim(line, "/"), "/")
			if len(parts) >= 3 {
				// Get the quality (last part before playlist.m3u8)
				quality := parts[len(parts)-2]
				// Convert to relative path for our server
				line = fmt.Sprintf("/stream/%s/%s/playlist.m3u8", trackID, quality)
			}
		} else if isSegment && strings.HasSuffix(line, ".ts") {
			// For segment playlists, convert relative segment names to full URLs
			// segment_000.ts -> /stream/trackID/quality/segment_000.ts
			// We need to extract quality from the current context
			line = line // Keep as relative for now, will be handled in segment request
		}
		
		result.WriteString(line)
		result.WriteString("\n")
	}
	
	return []byte(result.String())
}

func (s *StreamingServer) fetchFromS3WithSignedURL(key string) ([]byte, error) {
	// Construct full S3 key
	fullKey := s.s3Prefix + key
	
	signedURL, err := s.getSignedURL(fullKey)
	if err != nil {
		return nil, err
	}

	resp, err := http.Get(signedURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch from signed URL: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from signed URL", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (s *StreamingServer) handleMasterPlaylist(w http.ResponseWriter, r *http.Request) {
	// Extract trackID from URL path
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	trackID := strings.TrimSuffix(path, "/playlist.m3u8")
	
	// Handle case where path might have extra slashes
	trackID = strings.Trim(trackID, "/")

	// Load track metadata
	track, err := s.loadTrackMetadata(trackID)
	if err != nil {
		log.Printf("Error loading track metadata for %s: %v", trackID, err)
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	// Set proper headers for M3U8
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
	w.Header().Set("Cache-Control", "max-age=300") // Cache for 5 minutes

	// Try to fetch master playlist from S3 first
	s3Key := fmt.Sprintf("%s/playlist.m3u8", trackID)
	if data, err := s.fetchFromS3WithSignedURL(s3Key); err == nil {
		// Fix URLs in the fetched playlist
		fixedData := s.fixPlaylistURLs(data, trackID, false)
		w.Write(fixedData)
		log.Printf("Served master playlist from S3 for track: %s (%s)", track.Title, trackID)
	} else {
		// Generate master playlist if not found in S3
		playlist := s.generateMasterPlaylist(trackID)
		w.Write([]byte(playlist))
		log.Printf("Served generated master playlist for track: %s (%s)", track.Title, trackID)
	}
}

func (s *StreamingServer) handleQualityPlaylist(w http.ResponseWriter, r *http.Request) {
	// Parse URL path: /stream/trackID/quality/playlist.m3u8
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	
	if len(parts) < 3 {
		http.Error(w, "Invalid path format", http.StatusBadRequest)
		return
	}

	trackID := parts[0]
	quality := parts[1]
	// parts[2] should be "playlist.m3u8"

	// Validate quality
	if quality != "low" && quality != "med" && quality != "high" {
		http.Error(w, "Invalid quality", http.StatusBadRequest)
		return
	}

	// Load track metadata to verify track exists
	_, err := s.loadTrackMetadata(trackID)
	if err != nil {
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	// Fetch playlist from S3 using signed URL
	s3Key := fmt.Sprintf("%s/%s/playlist.m3u8", trackID, quality)
	data, err := s.fetchFromS3WithSignedURL(s3Key)
	if err != nil {
		log.Printf("Error fetching quality playlist from S3: %v", err)
		http.Error(w, "Playlist not found", http.StatusNotFound)
		return
	}

	// Fix segment URLs in the quality playlist
	fixedData := s.fixSegmentURLs(data, trackID, quality)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
	w.Header().Set("Cache-Control", "max-age=3600") // Cache for 1 hour

	w.Write(fixedData)
	log.Printf("Served %s quality playlist for track: %s", quality, trackID)
}

// Fix segment URLs in quality playlists
func (s *StreamingServer) fixSegmentURLs(content []byte, trackID, quality string) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	var result strings.Builder
	
	for scanner.Scan() {
		line := scanner.Text()
		
		// Convert relative segment URLs to absolute URLs for our server
		if strings.HasSuffix(line, ".ts") && !strings.HasPrefix(line, "/") {
			// Convert segment_000.ts to /stream/trackID/quality/segment_000.ts
			line = fmt.Sprintf("/stream/%s/%s/%s", trackID, quality, line)
		}
		
		result.WriteString(line)
		result.WriteString("\n")
	}
	
	return []byte(result.String())
}

func (s *StreamingServer) handleSegment(w http.ResponseWriter, r *http.Request) {
	// Rate limiting per IP
	clientIP := strings.Split(r.RemoteAddr, ":")[0]
	s.mu.Lock()
	if lastRequest, exists := s.rateLimiter[clientIP]; exists {
		if time.Since(lastRequest) < 100*time.Millisecond {
			s.mu.Unlock()
			http.Error(w, "Rate limited", http.StatusTooManyRequests)
			return
		}
	}
	s.rateLimiter[clientIP] = time.Now()
	s.mu.Unlock()

	// Clean up rate limiter periodically
	go s.cleanupRateLimiter()

	// Parse URL path: /stream/trackID/quality/segment.ts
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	
	if len(parts) < 3 {
		http.Error(w, "Invalid segment path format", http.StatusBadRequest)
		return
	}

	trackID := parts[0]
	quality := parts[1]
	segmentFile := parts[2]

	// Validate inputs
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
	s.mu.RLock()
	if cachedData, exists := s.segmentCache[cacheKey]; exists {
		s.mu.RUnlock()
		
		w.Header().Set("Content-Type", "video/MP2T")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "max-age=86400") // Cache for 24 hours
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(len(cachedData)))
		
		w.Write(cachedData)
		return
	}
	s.mu.RUnlock()

	// Fetch from S3 using signed URL
	s3Key := fmt.Sprintf("%s/%s/%s", trackID, quality, segmentFile)
	data, err := s.fetchFromS3WithSignedURL(s3Key)
	if err != nil {
		log.Printf("Error fetching segment from S3: %v", err)
		http.Error(w, "Segment not found", http.StatusNotFound)
		return
	}

	// Cache the segment if cache isn't full
	s.mu.Lock()
	if len(s.segmentCache) < s.maxCacheSize {
		s.segmentCache[cacheKey] = data
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "video/MP2T")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "max-age=86400") // Cache for 24 hours
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))

	w.Write(data)
}

func (s *StreamingServer) cleanupRateLimiter() {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	now := time.Now()
	for ip, lastTime := range s.rateLimiter {
		if now.Sub(lastTime) > 5*time.Minute {
			delete(s.rateLimiter, ip)
		}
	}
}

func (s *StreamingServer) cleanupSignedURLCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	now := time.Now()
	for key, item := range s.signedURLCache {
		if now.After(item.expiresAt) {
			delete(s.signedURLCache, key)
		}
	}
}

func (s *StreamingServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	health := map[string]interface{}{
		"status":         "healthy",
		"timestamp":      time.Now().Format(time.RFC3339),
		"cache_size":     len(s.segmentCache),
		"tracks_loaded":  len(s.tracks),
		"signed_urls_cached": len(s.signedURLCache),
	}
	
	json.NewEncoder(w).Encode(health)
}

func (s *StreamingServer) handleCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusOK)
}

// Middleware for logging and recovery
func loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		
		// Recovery from panics
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
	// Load environment variables from .env file
	err := godotenv.Load()
	if err != nil {
		log.Printf("Warning: Error loading .env file: %v", err)
	}
	
	// Read AWS configuration from environment
	bucketName := os.Getenv("S3_BUCKET_NAME")
	if bucketName == "" {
		log.Fatal("S3_BUCKET_NAME environment variable is required")
	}
	
	s3Prefix := os.Getenv("S3_PREFIX")
	if s3Prefix == "" {
		s3Prefix = "hls" // Default folder name
	}
	
	awsRegion := os.Getenv("AWS_REGION")
	if awsRegion == "" {
		awsRegion = "us-east-1" // Default region
	}
	
	awsAccessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	awsSecretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	
	// Validate credentials if provided
	if (awsAccessKey == "") != (awsSecretKey == "") {
		log.Fatal("Both AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be provided together, or both omitted to use default credentials")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server, err := NewStreamingServer(bucketName, s3Prefix, awsRegion, awsAccessKey, awsSecretKey)
	if err != nil {
		log.Fatalf("Failed to create streaming server: %v", err)
	}

	// Improved routing with better path matching
	http.HandleFunc("/stream/", loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			server.handleCORS(w, r)
			return
		}

		path := r.URL.Path
		
		// Remove /stream/ prefix and clean up path
		streamPath := strings.TrimPrefix(path, "/stream/")
		streamPath = strings.Trim(streamPath, "/")
		parts := strings.Split(streamPath, "/")

		switch {
		case len(parts) == 2 && parts[1] == "playlist.m3u8":
			// Master playlist: /stream/trackID/playlist.m3u8
			server.handleMasterPlaylist(w, r)
		case len(parts) == 3 && parts[2] == "playlist.m3u8":
			// Quality playlist: /stream/trackID/quality/playlist.m3u8
			server.handleQualityPlaylist(w, r)
		case len(parts) == 3 && strings.HasSuffix(parts[2], ".ts"):
			// Segment: /stream/trackID/quality/segment.ts
			server.handleSegment(w, r)
		default:
			log.Printf("Invalid stream path: %s (parts: %v)", path, parts)
			http.Error(w, "Invalid stream path", http.StatusBadRequest)
		}
	}))

	http.HandleFunc("/health", loggingMiddleware(server.handleHealth))

	// Start background cleanup routines
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		
		for range ticker.C {
			server.cleanupRateLimiter()
			server.cleanupSignedURLCache()
		}
	}()

	log.Printf("🚀 Production Audio Streaming Server starting on port %s", port)
	log.Printf("☁️  AWS S3 Bucket: %s (Private with Signed URLs)", bucketName)
	log.Printf("📁 S3 Prefix: %s/", s3Prefix)
	log.Printf("🌍 AWS Region: %s", awsRegion)
	if awsAccessKey != "" {
		log.Printf("🔑 Using AWS credentials from environment variables")
	} else {
		log.Printf("🔑 Using default AWS credentials (IAM role/profile)")
	}
	log.Printf("🎵 Stream URL format: http://localhost:%s/stream/TRACK_ID/playlist.m3u8", port)
	log.Printf("❤️  Health check: http://localhost:%s/health", port)
	log.Printf("🔐 Using S3 signed URLs with %v expiry", server.signedURLExpiry)

	log.Fatal(http.ListenAndServe(":"+port, nil))
}