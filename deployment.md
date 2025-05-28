# Audio Streaming Server - Docker Deployment Guide

This guide will help you deploy the HLS audio streaming server using Docker and Docker Compose.

## Prerequisites

- Docker (v20.10 or later)
- Docker Compose (v2.0 or later)
- AWS S3 bucket with HLS audio files
- AWS credentials (Access Key or IAM Role)

## Quick Start

### 1. Clone and Setup

```bash
# Clone your repository (or create these files in your project directory)
git clone <your-repo>
cd audio-streaming-server

# Copy environment template
cp .env.example .env
```

### 2. Configure Environment

Edit `.env` file with your AWS and server configuration:

```bash
# Required
S3_BUCKET_NAME=your-audio-bucket-name
S3_PREFIX=hls
AWS_REGION=us-east-1

# Optional (can use IAM roles instead)
AWS_ACCESS_KEY_ID=your-access-key-id
AWS_SECRET_ACCESS_KEY=your-secret-access-key
```

### 3. Deploy

```bash
# Build and start the server
docker-compose up -d

# Check logs
docker-compose logs -f

# Check health
curl http://localhost:5010/health

```

## Deployment Options

### Option 1: Basic Deployment

```bash
# Start just the Go server
docker-compose up -d audio-streaming-server
```


## File Structure

```
audio-streaming-server/
├── main.go                 # Your Go application
├── go.mod                  # Go module file
├── go.sum                  # Go dependencies
├── Dockerfile              # Docker build instructions
├── docker-compose.yml      # Container orchestration
├── .env                    # Environment variables
├── .env.example           # Environment template
├── .dockerignore          # Docker ignore file
└── DEPLOYMENT.md          # This file
```

## Configuration Options

### Environment Variables

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `S3_BUCKET_NAME` | AWS S3 bucket name | - | Yes |
| `S3_PREFIX` | Folder path in S3 | `hls` | No |
| `AWS_REGION` | AWS region | `us-east-1` | No |
| `AWS_ACCESS_KEY_ID` | AWS access key | - | No* |
| `AWS_SECRET_ACCESS_KEY` | AWS secret key | - | No* |
| `PORT` | Server port | `8080` | No |

### Port Mapping

| Service | Internal Port | External Port | Description |
|---------|---------------|---------------|-------------|
| Audio Server | 8080 | 5010 | Direct access to Go application |

*Not required if using IAM roles or AWS profiles

### Docker Compose Profiles

- **Default**: Only the Go application

## Production Considerations

### 1. Security

```bash
# Use IAM roles instead of access keys (recommended)
# Remove AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY from .env

# Use secrets for sensitive data
echo "your-secret-key" | docker secret create aws_secret_key -
```

### 2. SSL/HTTPS Setup

```bash
# Create SSL directory
mkdir ssl

# Copy your certificates
cp your-cert.pem ssl/cert.pem
cp your-key.pem ssl/key.pem

```

### 3. Scaling

```bash
# Scale the application
docker-compose up -d --scale audio-streaming-server=3

# Use external load balancer (AWS ALB, etc.)
```

### 4. Monitoring

```bash
# View logs
docker-compose logs -f

# Monitor health
watch curl -s http://localhost:8080/health

# Resource usage
docker stats
```

## Troubleshooting

### Common Issues

1. **Port Already in Use**
   ```bash
   # Change ports in docker-compose.yml
   ports:
     - "5011:5011"  # Use port 5011 instead of 5010

   ```

2. **AWS Permissions**
   ```bash
   # Check AWS credentials
   docker-compose exec audio-streaming-server env | grep AWS

   # Test S3 access
   aws s3 ls s3://your-bucket-name/hls/ --region us-east-1
   ```

3. **Memory Issues**
   ```bash
   # Increase memory limits in docker-compose.yml
   deploy:
     resources:
       limits:
         memory: 1G
   ```

### Debug Commands

```bash
# Shell into container
docker-compose exec audio-streaming-server sh

# Check container logs
docker-compose logs audio-streaming-server

# Restart services
docker-compose restart

# Clean rebuild
docker-compose down
docker-compose build --no-cache
docker-compose up -d
```

## Testing

### 1. Health Check

```bash
# Direct to audio server
curl http://localhost:5010/health

```

### 2. Stream Test

```bash
# Test master playlist (replace TRACK_ID with actual track)
# Direct to audio server
curl http://localhost:5010/stream/TRACK_ID/playlist.m3u8

# Test quality playlist
curl http://localhost:5010/stream/TRACK_ID/high/playlist.m3u8
```

### 3. Load Testing

```bash
# Install Apache Bench
sudo apt-get install apache2-utils

# Test concurrent requests
ab -n 1000 -c 10 http://localhost:5010/health


```

## Maintenance

### Updates

```bash
# Pull latest changes
git pull

# Rebuild and redeploy
docker-compose down
docker-compose build
docker-compose up -d
```

### Cleanup

```bash
# Remove containers
docker-compose down

# Remove images
docker rmi $(docker images -q audio-streaming-server*)

# Clean up system
docker system prune -f
```

## Performance Tuning

### 1. Resource Limits

Adjust in `docker-compose.yml`:

```yaml
deploy:
  resources:
    limits:
      memory: 1G
      cpus: '1.0'
    reservations:
      memory: 512M
      cpus: '0.5'
```

### 2. Caching

The application includes built-in caching for:
- S3 signed URLs (15 minutes)
- Audio segments (in-memory)
- Rate limiting per IP

### 3. CDN Integration

For production, consider using a CDN:
- AWS CloudFront
- Cloudflare
- KeyCDN

## Support

For issues related to:
- **Docker**: Check Docker documentation
- **AWS S3**: Verify bucket permissions and region
- **Application**: Check application logs with `docker-compose logs`