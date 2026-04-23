# audio-replay

A [Viam](https://viam.com/) audio input module that extracts the audio track from a local video file and exposes it as a virtual `audio_in` component. It uses `ffmpeg` to decode the video's audio stream in real time and streams PCM16 `AudioChunk`s through Viam's audio_in API, looping back to the beginning when the video ends. Useful for testing and development (paired with [video-replay](https://github.com/dmhilly/viam-video-replay), you can simulate a camera + mic from a single video file).

## Model: `devin-hilly:audio-replay:audio`

### Requirements

- `ffmpeg` must be installed on the host machine and on `$PATH`
- The video file must contain at least one audio stream (any codec ffmpeg can decode)
- Supported architectures: `linux/amd64`, `linux/arm64`

### Configuration

```json
{
  "video_path": "/path/to/your/video.mp4"
}
```

### Attributes

| Name           | Type   | Inclusion | Default | Description                                           |
|----------------|--------|-----------|---------|-------------------------------------------------------|
| `video_path`   | string | Required  | —       | Absolute path to the video file                       |
| `sample_rate`  | int    | Optional  | 48000   | Output sample rate in Hz (ffmpeg resamples as needed) |
| `num_channels` | int    | Optional  | 1       | Output channel count (ffmpeg remixes as needed)       |

### Example configuration

```json
{
  "components": [
    {
      "name": "my-replay-audio",
      "api": "rdk:component:audio_in",
      "model": "devin-hilly:audio-replay:audio",
      "attributes": {
        "video_path": "/home/user/videos/test.mp4",
        "sample_rate": 48000,
        "num_channels": 1
      }
    }
  ],
  "modules": [
    {
      "type": "registry",
      "name": "devin-hilly_audio-replay",
      "module_id": "devin-hilly:audio-replay",
      "version": "latest"
    }
  ]
}
```

## Building from source

```bash
# Install ffmpeg (macOS)
brew install ffmpeg

# Install ffmpeg (Linux)
sudo apt-get install -y ffmpeg

# Build the binary
make build

# Package as a module tarball
make module.tar.gz
```

## Limitations

- Returns PCM16 (`pcm16`) audio only
- Requires `ffmpeg` at runtime
