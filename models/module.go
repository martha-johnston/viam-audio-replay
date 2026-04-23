package models

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/spatialmath"
	rutils "go.viam.com/rdk/utils"
)

// Our audio model
var Audio = resource.NewModel("martha-johnston", "audio-replay", "audio")

const (
	chunkDurationMs    = 100
	subscriberBuf      = 64
	defaultSampleRate  = 48000
	defaultNumChannels = 1
)

func init() {
	resource.RegisterComponent(
		audioin.API,
		Audio,
		resource.Registration[audioin.AudioIn, *Config]{
			Constructor: newAudioReplayAudio,
		},
	)
}

// Config holds the JSON attributes
type Config struct {
	VideoPath   string `json:"video_path"`
	SampleRate  int32  `json:"sample_rate,omitempty"`
	NumChannels int32  `json:"num_channels,omitempty"`
}

// Validate ensures VideoPath is set
func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.VideoPath == "" {
		return nil, nil, fmt.Errorf("video_path is required for audio replay")
	}
	if c.SampleRate < 0 {
		return nil, nil, fmt.Errorf("sample_rate must be non-negative")
	}
	if c.NumChannels < 0 {
		return nil, nil, fmt.Errorf("num_channels must be non-negative")
	}
	return nil, nil, nil
}

// audioReplayAudio implements audioin.AudioIn + resource.Reconfigurable
type audioReplayAudio struct {
	name       resource.Name
	logger     logging.Logger
	cfg        *Config
	cancelFunc context.CancelFunc

	// Lifetime of the resource.
	mainCtx context.Context

	// Per-open-file loop context + ffmpeg subprocess state.
	procMutex   sync.Mutex
	loopCancel  context.CancelFunc
	ffmpegCmd   *exec.Cmd
	ffmpegOut   io.ReadCloser
	sampleRate  int32
	numChannels int32

	// Subscribers — guarded by subMutex.
	subMutex    sync.Mutex
	subscribers map[int]chan *audioin.AudioChunk
	nextSubID   int
}

// newAudioReplayAudio is called once when the audio input is created.
func newAudioReplayAudio(
	ctx context.Context,
	deps resource.Dependencies,
	rawConf resource.Config,
	logger logging.Logger,
) (audioin.AudioIn, error) {
	logger.Infof("[newAudioReplayAudio] Called")

	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	mainCtx, cancelFunc := context.WithCancel(context.Background())

	a := &audioReplayAudio{
		name:        rawConf.ResourceName(),
		logger:      logger,
		cfg:         conf,
		cancelFunc:  cancelFunc,
		mainCtx:     mainCtx,
		subscribers: map[int]chan *audioin.AudioChunk{},
	}

	if err := a.openAndStartLoop(conf); err != nil {
		cancelFunc()
		return nil, fmt.Errorf("failed to start ffmpeg at creation: %w", err)
	}

	logger.Infof("[newAudioReplayAudio] AudioIn constructed successfully: %q", a.name)
	return a, nil
}

// openAndStartLoop launches ffmpeg to extract PCM16 audio from the video,
// looping forever, and starts a goroutine that broadcasts chunks to subscribers.
func (a *audioReplayAudio) openAndStartLoop(conf *Config) error {
	a.procMutex.Lock()
	defer a.procMutex.Unlock()

	a.stopFFmpegLocked()

	sampleRate := conf.SampleRate
	if sampleRate == 0 {
		sampleRate = defaultSampleRate
	}
	numChannels := conf.NumChannels
	if numChannels == 0 {
		numChannels = defaultNumChannels
	}

	loopCtx, loopCancel := context.WithCancel(a.mainCtx)

	// -re paces at native rate; -stream_loop -1 loops the input forever.
	// -vn drops video; -f s16le outputs raw little-endian PCM16 to stdout.
	cmd := exec.CommandContext(loopCtx, "ffmpeg",
		"-nostdin",
		"-hide_banner",
		"-loglevel", "warning",
		"-re",
		"-stream_loop", "-1",
		"-i", conf.VideoPath,
		"-vn",
		"-f", "s16le",
		"-acodec", "pcm_s16le",
		"-ar", fmt.Sprintf("%d", sampleRate),
		"-ac", fmt.Sprintf("%d", numChannels),
		"-",
	)
	cmd.Stderr = newLoggerWriter(a.logger, "[ffmpeg] ")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		loopCancel()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		loopCancel()
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	a.loopCancel = loopCancel
	a.ffmpegCmd = cmd
	a.ffmpegOut = stdout
	a.sampleRate = sampleRate
	a.numChannels = numChannels

	a.logger.Infof("[openAndStartLoop] ffmpeg started for %q (%dHz, %dch)",
		conf.VideoPath, sampleRate, numChannels)

	go a.audioUpdateLoop(loopCtx, stdout, sampleRate, numChannels)
	return nil
}

// stopFFmpegLocked kills any running ffmpeg process. Caller must hold procMutex.
func (a *audioReplayAudio) stopFFmpegLocked() {
	if a.loopCancel != nil {
		a.loopCancel()
		a.loopCancel = nil
	}
	if a.ffmpegOut != nil {
		a.ffmpegOut.Close()
		a.ffmpegOut = nil
	}
	if a.ffmpegCmd != nil {
		_ = a.ffmpegCmd.Wait()
		a.ffmpegCmd = nil
	}
}

// audioUpdateLoop reads fixed-size PCM16 chunks from ffmpeg's stdout and
// broadcasts them to any subscribers.
func (a *audioReplayAudio) audioUpdateLoop(ctx context.Context, stdout io.Reader, sampleRate, numChannels int32) {
	a.logger.Infof("[audioUpdateLoop] Starting for %q", a.name)
	defer a.logger.Infof("[audioUpdateLoop] Exiting for %q", a.name)

	samplesPerChunk := int(sampleRate) * chunkDurationMs / 1000
	chunkBytes := samplesPerChunk * int(numChannels) * 2 // PCM16 = 2 bytes/sample
	chunkDurationNs := int64(chunkDurationMs) * int64(time.Millisecond)

	var sequence int32
	for {
		if ctx.Err() != nil {
			return
		}
		buf := make([]byte, chunkBytes)
		if _, err := io.ReadFull(stdout, buf); err != nil {
			if ctx.Err() == nil {
				a.logger.Warnf("[audioUpdateLoop] ffmpeg read error: %v", err)
			}
			return
		}

		now := time.Now()
		chunk := &audioin.AudioChunk{
			AudioData: buf,
			AudioInfo: &rutils.AudioInfo{
				Codec:        rutils.CodecPCM16,
				SampleRateHz: sampleRate,
				NumChannels:  numChannels,
			},
			StartTimestampNanoseconds: now.UnixNano(),
			EndTimestampNanoseconds:   now.UnixNano() + chunkDurationNs,
			Sequence:                  sequence,
		}
		a.broadcast(chunk)
		sequence++
	}
}

func (a *audioReplayAudio) broadcast(chunk *audioin.AudioChunk) {
	a.subMutex.Lock()
	defer a.subMutex.Unlock()
	for _, sub := range a.subscribers {
		select {
		case sub <- chunk:
		default:
			// Drop if the subscriber isn't keeping up.
		}
	}
}

// GetAudio subscribes to the ongoing playback loop and streams chunks until
// the requested duration elapses or the context is canceled.
func (a *audioReplayAudio) GetAudio(
	ctx context.Context,
	codec string,
	durationSeconds float32,
	previousTimestampNs int64,
	extra map[string]interface{},
) (chan *audioin.AudioChunk, error) {
	if codec != "" && codec != rutils.CodecPCM16 {
		return nil, fmt.Errorf("audio-replay only supports codec %q, got %q", rutils.CodecPCM16, codec)
	}

	out := make(chan *audioin.AudioChunk, subscriberBuf)
	a.subMutex.Lock()
	id := a.nextSubID
	a.nextSubID++
	a.subscribers[id] = out
	a.subMutex.Unlock()

	go func() {
		defer func() {
			a.subMutex.Lock()
			delete(a.subscribers, id)
			a.subMutex.Unlock()
			close(out)
		}()

		var durCh <-chan time.Time
		if durationSeconds > 0 {
			durCh = time.After(time.Duration(float32(time.Second) * durationSeconds))
		}
		select {
		case <-ctx.Done():
		case <-a.mainCtx.Done():
		case <-durCh:
		}
	}()

	return out, nil
}

// Properties returns the audio device properties.
func (a *audioReplayAudio) Properties(ctx context.Context, extra map[string]interface{}) (rutils.Properties, error) {
	a.procMutex.Lock()
	defer a.procMutex.Unlock()
	return rutils.Properties{
		SupportedCodecs: []string{rutils.CodecPCM16},
		SampleRateHz:    a.sampleRate,
		NumChannels:     a.numChannels,
	}, nil
}

// Reconfigure changes the video by always stopping and restarting ffmpeg.
func (a *audioReplayAudio) Reconfigure(
	ctx context.Context,
	deps resource.Dependencies,
	rawConf resource.Config,
) error {
	a.logger.Infof("[Reconfigure] Called for %q", a.name)

	newConf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return err
	}

	if err := a.openAndStartLoop(newConf); err != nil {
		return fmt.Errorf("reconfigure: %w", err)
	}
	a.cfg = newConf
	return nil
}

// Name returns the resource name.
func (a *audioReplayAudio) Name() resource.Name {
	return a.name
}

// Geometries returns no geometries.
func (a *audioReplayAudio) Geometries(ctx context.Context, extra map[string]interface{}) ([]spatialmath.Geometry, error) {
	return nil, nil
}

// Status returns the current status.
func (a *audioReplayAudio) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

// DoCommand is not supported.
func (a *audioReplayAudio) DoCommand(
	ctx context.Context,
	cmd map[string]interface{},
) (map[string]interface{}, error) {
	return nil, fmt.Errorf("do command not supported")
}

// Close cleans up on resource removal.
func (a *audioReplayAudio) Close(ctx context.Context) error {
	a.logger.Infof("[Close] Called for %q", a.name)
	a.procMutex.Lock()
	a.stopFFmpegLocked()
	a.procMutex.Unlock()
	a.cancelFunc()
	return nil
}

// loggerWriter adapts an io.Writer to a logging.Logger so ffmpeg stderr ends up in the RDK logs.
type loggerWriter struct {
	logger logging.Logger
	prefix string
}

func newLoggerWriter(l logging.Logger, prefix string) *loggerWriter {
	return &loggerWriter{logger: l, prefix: prefix}
}

func (w *loggerWriter) Write(p []byte) (int, error) {
	w.logger.Infof("%s%s", w.prefix, string(p))
	return len(p), nil
}
