import os
import subprocess
import shutil
import json
import logging
import ffmpeg

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - %(message)s',
    handlers=[logging.StreamHandler()]
)
logger = logging.getLogger(__name__)

class AudioProcessor:
    def __init__(self, 
                 raw_dir='/mnt/HC_Volume_103173495/mwonya_assets/raw',
                 output_dir='/mnt/HC_Volume_103173495/mwonya_assets/hls_output/',
                 failed_dir='/mnt/HC_Volume_103173495/mwonya_assets/failed/',
                 segment_duration=4):
        """Initialize AudioProcessor with file paths and settings."""
        self.raw_dir = raw_dir.rstrip('/')
        self.output_dir = output_dir.rstrip('/')
        self.failed_dir = failed_dir.rstrip('/')
        self.segment_duration = segment_duration

        # Ensure directories exist
        os.makedirs(self.raw_dir, exist_ok=True)
        os.makedirs(self.output_dir, exist_ok=True)
        os.makedirs(self.failed_dir, exist_ok=True)

        # HLS quality configurations
        self.hls_quality_configs = {
            'low': {
                'bitrate': '64k',
                'sample_rate': '24000',
                'channels': '2',
                'codec': 'aac',
                'codec_options': ['-profile:a', 'aac_low'],
                'description': 'Low-bandwidth quality'
            },
            'med': {
                'bitrate': '128k',
                'sample_rate': '32000',
                'channels': '2',
                'codec': 'aac',
                'codec_options': ['-profile:a', 'aac_low'],
                'description': 'Medium quality'
            },
            'high': {
                'bitrate': '320k',
                'sample_rate': '44100',
                'channels': '2',
                'codec': 'aac',
                'codec_options': ['-profile:a', 'aac_low'],
                'description': 'High quality'
            }
        }
        self.target_loudness = -14  # LUFS for normalization
        logger.info("AudioProcessor initialized")

    def process_audio(self, filename):
        """Process a single audio file to HLS format."""
        logger.info(f"Processing {filename}")
        raw_path = os.path.join(self.raw_dir, filename)
        track_id = os.path.splitext(filename)[0]
        output_track_dir = os.path.join(self.output_dir, track_id)
        
        if not os.path.isfile(raw_path):
            logger.error(f"File not found: {raw_path}")
            self._move_to_failed(raw_path)
            return False

        try:
            # Normalize loudness
            normalized_path = self.normalize_loudness(raw_path)
            if not normalized_path:
                raise Exception("Loudness normalization failed")

            # Convert to AAC
            aac_path = os.path.join(self.output_dir, track_id, f"{track_id}.m4a")
            os.makedirs(os.path.dirname(aac_path), exist_ok=True)
            if not self.convert_to_aac(normalized_path, aac_path):
                raise Exception("AAC conversion failed")

            # Generate HLS streams
            if not self.generate_hls_streams(aac_path, output_track_dir, track_id):
                raise Exception("HLS generation failed")

            # Generate metadata
            self.generate_metadata(track_id, filename, aac_path, output_track_dir)

            # Clean up temporary normalized file
            if normalized_path != raw_path:
                self.cleanup_temp_files([normalized_path])

            logger.info(f"Successfully processed {filename}")
            return True

        except Exception as e:
            logger.error(f"Failed to process {filename}: {str(e)}")
            self._move_to_failed(raw_path)
            self.cleanup_temp_files([normalized_path] if 'normalized_path' in locals() else [])
            return False

    def normalize_loudness(self, input_path):
        """Normalize audio loudness to -14 LUFS."""
        try:
            original_ext = os.path.splitext(input_path)[1].lower()
            normalized_path = f"/tmp/{os.path.basename(input_path)}_normalized{original_ext}"
            (
                ffmpeg.input(input_path)
                .filter('loudnorm', I=self.target_loudness, LRA=11, TP=-1.5)
                .output(normalized_path, acodec='aac', f='mp4')
                .overwrite_output()
                .run(quiet=True)
            )
            logger.info(f"Normalized {input_path} to {normalized_path}")
            return normalized_path
        except ffmpeg.Error as e:
            logger.error(f"Normalization failed: {e.stderr.decode('utf8')}")
            return None

    def convert_to_aac(self, input_path, output_path):
        """Convert audio to AAC format."""
        try:
            (
                ffmpeg.input(input_path)
                .output(output_path, acodec='aac', audio_bitrate='128k', ar=44100, ac=2, f='ipod')
                .overwrite_output()
                .run(quiet=True)
            )
            logger.info(f"Converted to AAC: {output_path}")
            return True
        except ffmpeg.Error as e:
            logger.error(f"AAC conversion failed: {e.stderr.decode('utf8')}")
            return False

    def generate_hls_streams(self, input_path, output_dir, track_id):
        """Generate HLS streams for multiple quality levels."""
        try:
            os.makedirs(output_dir, exist_ok=True)
            success_count = 0
            for quality, config in self.hls_quality_configs.items():
                quality_dir = os.path.join(output_dir, quality)
                if self.process_hls_quality(input_path, quality_dir, quality, config):
                    success_count += 1
                    logger.info(f"HLS generated for {quality}")
                else:
                    logger.error(f"HLS failed for {quality}")

            if success_count == 0:
                return False

            # Generate master playlist
            master_playlist_path = os.path.join(output_dir, 'playlist.m3u8')
            with open(master_playlist_path, 'w') as f:
                f.write(self.generate_master_playlist(track_id))
            logger.info(f"Master playlist generated at {master_playlist_path}")
            return True
        except Exception as e:
            logger.error(f"HLS streams error: {str(e)}")
            return False

    def process_hls_quality(self, input_path, output_dir, quality, config):
        """Process audio for a specific HLS quality level."""
        try:
            os.makedirs(output_dir, exist_ok=True)
            playlist_path = os.path.join(output_dir, 'playlist.m3u8')
            cmd = [
                'ffmpeg', '-i', input_path,
                '-c:a', config['codec'],
                *config['codec_options'],
                '-b:a', config['bitrate'],
                '-ar', config['sample_rate'],
                '-ac', config['channels'],
                '-f', 'hls',
                '-hls_time', str(self.segment_duration),
                '-hls_list_size', '0',
                '-hls_segment_filename', os.path.join(output_dir, 'segment_%03d.ts'),
                '-hls_playlist_type', 'vod',
                '-hls_flags', 'independent_segments',
                '-y', playlist_path
            ]
            subprocess.run(cmd, capture_output=True, text=True, check=True)
            if not os.path.exists(playlist_path):
                raise RuntimeError(f"Playlist not created for {quality}")
            return True
        except subprocess.CalledProcessError as e:
            logger.error(f"FFmpeg error for {quality}: {e.stderr}")
            return False
        except Exception as e:
            logger.error(f"Error for {quality}: {str(e)}")
            return False

    def generate_master_playlist(self, track_id):
        """Generate master playlist content with relative paths."""
        playlist_content = "#EXTM3U\n#EXT-X-VERSION:3\n\n"
        bandwidth_map = {'low': 80000, 'med': 160000, 'high': 384000}
        for quality in ['low', 'med', 'high']:
            config = self.hls_quality_configs[quality]
            bandwidth = bandwidth_map[quality]
            playlist_content += f'#EXT-X-STREAM-INF:BANDWIDTH={bandwidth},CODECS="mp4a.40.2"\n'
            playlist_content += f'{quality}/playlist.m3u8\n\n'
        return playlist_content

    def generate_metadata(self, track_id, filename, aac_path, output_dir):
        """Generate and save track metadata."""
        try:
            audio_info = self.get_audio_info(aac_path)
            metadata = {
                'id': track_id,
                'title': audio_info.get('title', os.path.splitext(filename)[0]),
                'artist': audio_info.get('artist', 'Unknown Artist'),
                'duration': int(audio_info.get('duration', 0)),
                'processed_at': datetime.datetime.now().isoformat(),
                'qualities': list(self.hls_quality_configs.keys()),
                'hls_master_playlist': os.path.join(output_dir, 'playlist.m3u8').replace('\\', '/')
            }
            metadata_path = os.path.join(output_dir, 'metadata.json')
            with open(metadata_path, 'w') as f:
                json.dump(metadata, f, indent=2)
            logger.info(f"Metadata saved at {metadata_path}")
        except Exception as e:
            logger.error(f"Metadata generation failed: {str(e)}")

    def get_audio_info(self, input_path):
        """Extract audio metadata using ffprobe."""
        try:
            cmd = ['ffprobe', '-v', 'quiet', '-print_format', 'json',
                   '-show_format', '-show_streams', input_path]
            result = subprocess.run(cmd, capture_output=True, text=True, check=True)
            data = json.loads(result.stdout)
            format_info = data.get('format', {})
            audio_stream = next((s for s in data.get('streams', []) if s.get('codec_type') == 'audio'), {})
            return {
                'duration': float(format_info.get('duration', 0)),
                'title': format_info.get('tags', {}).get('title', ''),
                'artist': format_info.get('tags', {}).get('artist', ''),
                'sample_rate': int(audio_stream.get('sample_rate', 44100)),
                'channels': int(audio_stream.get('channels', 2))
            }
        except Exception as e:
            logger.error(f"Audio info error: {str(e)}")
            return {'duration': 0, 'title': '', 'artist': ''}

    def cleanup_temp_files(self, file_paths):
        """Remove temporary files."""
        for path in file_paths:
            try:
                if os.path.isfile(path):
                    os.remove(path)
                logger.info(f"Cleaned up {path}")
            except Exception as e:
                logger.warning(f"Cleanup failed for {path}: {str(e)}")

    def _move_to_failed(self, raw_path):
        """Move failed file to failed directory."""
        logger.info(f"Keeping raw file {raw_path} in place despite failure")
        # try:
        #     failed_path = os.path.join(self.failed_dir, os.path.basename(raw_path))
        #     shutil.move(raw_path, failed_path)
        #     logger.info(f"Moved failed file to {failed_path}")
        # except Exception as e:
        #     logger.error(f"Failed to move {raw_path} to failed directory: {str(e)}")

    def process_all_files(self):
        """Process all audio files in the raw directory."""
        audio_extensions = ('.mp3', '.wav', '.flac', '.m4a', '.aac')
        files = [f for f in os.listdir(self.raw_dir) if f.lower().endswith(audio_extensions)]
        logger.info(f"Found {len(files)} audio files to process")

        success_count, fail_count = 0, 0
        for filename in files:
            if self.process_audio(filename):
                success_count += 1
            else:
                fail_count += 1

        logger.info(f"Processing complete: Success={success_count}, Failed={fail_count}")
        return success_count, fail_count

if __name__ == "__main__":
    import datetime
    processor = AudioProcessor()
    processor.process_all_files()