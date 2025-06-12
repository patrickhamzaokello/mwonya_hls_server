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
	tracks             map[string]*AudioTrack
	segmentCache       map[string][]byte
	signedURLCache     map[string]*SignedURLCacheItem
	rateLimiter        map[string]time.Time
	stats              StreamingStats
	mu                 sync.RWMutex
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

func NewStreamingServer(bucketName, s3Prefix, s3RawPrefix, region, accessKey, secretKey string) (*StreamingServer, error) {
	var cfg aws.Config
	var err error

	if accessKey != "" && secretKey != "" {
		cfg, err = config.LoadDefaultConfig(context.TODO(),
			config.WithRegion(region),
			config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
				return aws.Credentials{
					AccessKeyID:     accessKey,
					SecretAccessKey: secretKey,
				}, nil
			}),
			))
	} else {
		cfg, err = config.LoadDefaultConfig(context.TODO(),
			config.WithRegion(region),
		)
	}

	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %v", err)
	}

	s3Client := s3.NewFromConfig(cfg)
	presignClient := s3.NewPresignClient(s3Client)

	if s3Prefix != "" && !strings.HasSuffix(s3Prefix, "/") {
		s3Prefix = s3Prefix + "/"
	}
	if s3RawPrefix != "" && !strings.HasSuffix(s3RawPrefix, "/") {
		s3RawPrefix = s3RawPrefix + "/"
	}

	return &StreamingServer{
		s3Client:           s3Client,
		presignClient:      presignClient,
		bucketName:         bucketName,
		s3Prefix:           s3Prefix,
		s3RawPrefix:        s3RawPrefix,
		tracks:             make(map[string]*AudioTrack),
		segmentCache:       make(map[string][]byte),
		signedURLCache:     make(map[string]*SignedURLCacheItem),
		rateLimiter:        make(map[string]time.Time),
		maxCacheSize:       1000,
		cacheExpiry:        24 * time.Hour,
		signedURLExpiry:    15 * time.Minute,
		signedURLCacheSize: 5000,
		stats: StreamingStats{
			TrackStreams: make(map[string]int64),
			StartTime:    time.Now(),
		},
	}, nil
}

func getContentTypeByExtension(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".ogg":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	case ".m4a":
		return "audio/mp4"
	case ".aac":
		return "audio/aac"
	case ".webm":
		return "audio/webm"
	default:
		return "application/octet-stream"
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
	s.mu.RLock()
	if cached, exists := s.signedURLCache[key]; exists {
		if time.Now().Before(cached.expiresAt.Add(-2 * time.Minute)) {
			s.mu.RUnlock()
			return cached.url, nil
		}
	}
	s.mu.RUnlock()

	request, err := s.presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = s.signedURLExpiry
	})

	if err != nil {
		return "", fmt.Errorf("failed to create signed URL for %s: %v", key, err)
	}

	s.mu.Lock()
	if len(s.signedURLCache) >= s.signedURLCacheSize {
		now := time.Now()
		for k, v := range s.signedURLCache {
			if now.After(v.expiresAt) {
				delete(s.signedURLCache, k)
			}
		}

		if len(s.signedURLCache) >= s.signedURLCacheSize {
			count := 0
			for k := range s.signedURLCache {
				delete(s.signedURLCache, k)
				count++
				if count >= s.signedURLCacheSize/4 {
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

	s.mu.Lock()
	s.stats.SignedURLRequests++
	s.mu.Unlock()

	return request.URL, nil
}

func (s *StreamingServer) loadTrackMetadata(trackID string) (*AudioTrack, error) {
	s.mu.RLock()
	if track, exists := s.tracks[trackID]; exists {
		s.mu.RUnlock()
		return track, nil
	}
	s.mu.RUnlock()

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
			track := &AudioTrack{
				ID:       trackID,
				Title:    strings.ReplaceAll(trackID, "_", " "),
				Artist:   "Unknown Artist",
				Duration: 180,
			}

			s.mu.Lock()
			s.tracks[trackID] = track
			s.mu.Unlock()

			return track, nil
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

	s.mu.Lock()
	s.stats.TotalStreams++
	s.stats.TrackStreams[trackID]++
	s.stats.ActiveStreams++
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
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

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
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

	go s.cleanupRateLimiter()

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

	s.mu.RLock()
	if cachedData, exists := s.segmentCache[cacheKey]; exists {
		s.mu.RUnlock()

		s.mu.Lock()
		s.stats.CacheHits++
		s.stats.BandwidthUsage += int64(len(cachedData))
		s.mu.Unlock()

		w.Header().Set("Content-Type", "video/MP2T")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(len(cachedData)))

		w.Write(cachedData)
		return
	}
	s.mu.RUnlock()

	s3Key := fmt.Sprintf("%s%s/%s/%s", s.s3Prefix, trackID, quality, segmentFile)
	data, err := s.fetchFromS3WithSignedURL(s3Key)
	if err != nil {
		log.Printf("Error fetching segment from S3: %v", err)
		http.Error(w, "Segment not found", http.StatusNotFound)
		return
	}

	s.mu.Lock()
	s.stats.CacheMisses++
	s.stats.BandwidthUsage += int64(len(data))
	s.mu.Unlock()

	s.mu.Lock()
	if len(s.segmentCache) < s.maxCacheSize {
		s.segmentCache[cacheKey] = data
	}
	s.mu.Unlock()

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

	track, err := s.loadTrackMetadata(trackID)
	if err != nil {
		log.Printf("Error loading track metadata for %s: %v", trackID, track)
		http.Error(w, "Track not found", http.StatusNotFound)
		return
	}

	s.mu.Lock()
	s.stats.TotalStreams++
	s.stats.TrackStreams[trackID]++
	s.stats.ActiveStreams++
	s.mu.Unlock()

	fileExt := "m4a"
	if ext := r.URL.Query().Get("format"); ext != "" {
		fileExt = strings.TrimPrefix(ext, ".")
	}

	s3Key := fmt.Sprintf("%s%s.%s", s.s3RawPrefix, trackID, fileExt)

	exists, err := s.fileExists(s3Key)
	if err != nil {
		log.Printf("Error checking file existence for %s: %v", s3Key, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if !exists {
		s.fallbackToHLS(w, r, trackID)
		return
	}

	signedURL, err := s.getSignedURL(s3Key)
	if err != nil {
		log.Printf("Error generating signed URL for %s: %v", s3Key, err)
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	s.proxyFile(w, r, signedURL, fmt.Sprintf("%s.%s", trackID, fileExt))
}

func (s *StreamingServer) proxyFile(w http.ResponseWriter, r *http.Request, signedURL, filename string) {
	req, err := http.NewRequest("GET", signedURL, nil)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

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

	for k, v := range resp.Header {
		if strings.HasPrefix(k, "Content-") || k == "Accept-Ranges" || k == "Content-Disposition" {
			w.Header()[k] = v
		}
	}

	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", getContentTypeByExtension(filename))
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")

	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("Error streaming file: %v", err)
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

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "max-age=300")

	data, err := s.fetchFromS3WithSignedURL(hlsMasterKey)
	if err != nil {
		http.Error(w, "Failed to fetch HLS playlist", http.StatusInternalServerError)
		return
	}

	fixedData := s.fixPlaylistURLs(data, trackID, false)
	w.Write(fixedData)
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

	s.mu.RLock()
	uptime := time.Since(s.stats.StartTime).Round(time.Second)
	s.mu.RUnlock()

	health := map[string]interface{}{
		"status":             "healthy",
		"timestamp":          time.Now().Format(time.RFC3339),
		"cache_size":         len(s.segmentCache),
		"tracks_loaded":      len(s.tracks),
		"signed_urls_cached": len(s.signedURLCache),
		"uptime":             uptime.String(),
	}

	json.NewEncoder(w).Encode(health)
}

func (s *StreamingServer) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	s.mu.RLock()
	stats := s.stats
	stats.Uptime = time.Since(s.stats.StartTime).Round(time.Second).String()
	s.mu.RUnlock()

	json.NewEncoder(w).Encode(stats)
}

func (s *StreamingServer) handleCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusOK)
}

func (s *StreamingServer) handleTrackList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	s.mu.RLock()
	defer s.mu.RUnlock()

	tracks := make([]*AudioTrack, 0, len(s.tracks))
	for _, track := range s.tracks {
		tracks = append(tracks, track)
	}

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

	bucketName := os.Getenv("S3_BUCKET_NAME")
	if bucketName == "" {
		log.Fatal("S3_BUCKET_NAME environment variable is required")
	}

	s3Prefix := os.Getenv("S3_PREFIX")
	if s3Prefix == "" {
		s3Prefix = "hls"
	}

	s3RawPrefix := os.Getenv("S3_RAW_PREFIX")
	if s3RawPrefix == "" {
		s3RawPrefix = "raw/"
	}

	awsRegion := os.Getenv("AWS_REGION")
	if awsRegion == "" {
		awsRegion = "us-east-1"
	}

	awsAccessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	awsSecretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	if (awsAccessKey == "") != (awsSecretKey == "") {
		log.Fatal("Both AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be provided together, or both omitted to use default credentials")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server, err := NewStreamingServer(bucketName, s3Prefix, s3RawPrefix, awsRegion, awsAccessKey, awsSecretKey)
	if err != nil {
		log.Fatalf("Failed to create streaming server: %v", err)
	}

	staticDir := "./static"
	if _, err := os.Stat(staticDir); os.IsNotExist(err) {
		log.Printf("Warning: Static directory '%s' not found, skipping static file serving", staticDir)
	} else {
		fs := http.FileServer(http.Dir(staticDir))
		http.Handle("/", fs)
		log.Printf("Serving static files from %s directory", staticDir)
	}

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

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			server.cleanupRateLimiter()
			server.cleanupSignedURLCache()

			server.mu.Lock()
			server.stats.ActiveStreams = server.stats.ActiveStreams / 2
			if server.stats.ActiveStreams < 0 {
				server.stats.ActiveStreams = 0
			}
			server.mu.Unlock()
		}
	}()

	log.Printf("🚀 Production Audio Streaming Server starting on port %s", port)
	log.Printf("☁️  AWS S3 Bucket: %s (Private with Signed URLs)", bucketName)
	log.Printf("📁 HLS Prefix: %s/", s3Prefix)
	log.Printf("📁 Raw Prefix: %s", s3RawPrefix)
	log.Printf("🌍 AWS Region: %s", awsRegion)
	if awsAccessKey != "" {
		log.Printf("🔑 Using AWS credentials from environment variables")
	} else {
		log.Printf("🔑 Using default AWS credentials (IAM role/profile)")
	}
	log.Printf("🎵 HLS Stream URL format: http://localhost:%s/stream/TRACK_ID/playlist.m3u8", port)
	log.Printf("🎵 Direct File URL format: http://localhost:%s/file/TRACK_ID", port)
	log.Printf("❤️  Health check: http://localhost:%s/health", port)
	log.Printf("📊 Stats: http://localhost:%s/stats", port)
	log.Printf("🎶 Track list: http://localhost:%s/tracks", port)
	log.Printf("🔐 Using S3 signed URLs with %v expiry", server.signedURLExpiry)

	log.Fatal(http.ListenAndServe(":"+port, nil))
}
