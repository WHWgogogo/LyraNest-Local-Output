package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lyranest/lyranest-local-output/internal/config"
)

// ---------------------------------------------------------------------------
// /api/play
// ---------------------------------------------------------------------------

type playRequest struct {
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	Artist      string   `json:"artist"`
	Album       string   `json:"album"`
	Volume      *int     `json:"volume"`
	Position    *float64 `json:"position"`
	StartPaused bool     `json:"start_paused"`
}

func (s *Server) play(w http.ResponseWriter, r *http.Request) {
	var request playRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request")
		return
	}
	streamURL := strings.TrimSpace(request.URL)
	if streamURL == "" {
		writeError(w, http.StatusBadRequest, "url is required", "invalid_request")
		return
	}
	// The queue URI is what MPD actually accepts. HTTP streams pass through
	// unchanged; local library paths are rewritten into the music-directory
	// relative form, because MPD denies absolute file:// URIs (verified on the
	// target host: "ACK [4@0] {add} Access denied").
	queueURI, err := resolvePlayURI(streamURL, s.cfg.MPDMusicDirectory)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "playback kernel is unavailable", "mpd_unavailable")
		return
	}

	ctx := r.Context()
	// Single-instance semantics: the sound card is exclusive (no dmix), so a
	// new play request replaces the current track instead of queueing.
	if err := s.client.Clear(ctx); err != nil {
		s.writeKernelError(w, err)
		return
	}
	if err := s.client.Add(ctx, queueURI); err != nil {
		s.writeKernelError(w, err)
		return
	}
	if request.Volume != nil {
		if err := s.client.SetVolume(ctx, *request.Volume); err != nil {
			s.writeKernelError(w, err)
			return
		}
	}
	if err := s.client.Play(ctx); err != nil {
		s.writeKernelError(w, err)
		return
	}
	if request.Position != nil && *request.Position > 0 {
		if err := s.client.SeekCur(ctx, time.Duration(*request.Position*float64(time.Second))); err != nil {
			s.writeKernelError(w, err)
			return
		}
	}
	if request.StartPaused {
		if err := s.client.Pause(ctx, true); err != nil {
			s.writeKernelError(w, err)
			return
		}
	}
	s.logger.Info("playback started", "url", redactURL(streamURL), "queue_uri", redactURL(queueURI), "title", request.Title, "artist", request.Artist)
	s.writePlayback(w, ctx, http.StatusOK)
}

// ---------------------------------------------------------------------------
// /api/control
// ---------------------------------------------------------------------------

type controlRequest struct {
	Action   string   `json:"action"`
	Value    *float64 `json:"value"`
	Position *float64 `json:"position"`
	Enabled  *bool    `json:"enabled"`
}

func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	var request controlRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request")
		return
	}
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "playback kernel is unavailable", "mpd_unavailable")
		return
	}
	action := strings.ToLower(strings.TrimSpace(request.Action))
	ctx := r.Context()

	var err error
	switch action {
	case "pause":
		err = s.client.Pause(ctx, true)
	case "resume", "unpause":
		err = s.client.Pause(ctx, false)
	case "toggle":
		status, statusErr := s.client.Status(ctx)
		if statusErr != nil {
			err = statusErr
			break
		}
		err = s.client.Pause(ctx, status["state"] == "play")
	case "play":
		err = s.client.Play(ctx)
	case "stop":
		err = s.client.Stop(ctx)
	case "next":
		err = s.client.Next(ctx)
	case "prev", "previous":
		err = s.client.Previous(ctx)
	case "seek":
		seconds := 0.0
		if request.Position != nil {
			seconds = *request.Position
		} else if request.Value != nil {
			seconds = *request.Value
		}
		if seconds < 0 {
			writeError(w, http.StatusBadRequest, "seek position must not be negative", "invalid_request")
			return
		}
		err = s.client.SeekCur(ctx, time.Duration(seconds*float64(time.Second)))
	case "repeat":
		err = s.client.SetRepeat(ctx, request.Enabled != nil && *request.Enabled)
	case "random", "shuffle":
		err = s.client.SetRandom(ctx, request.Enabled != nil && *request.Enabled)
	case "single":
		err = s.client.SetSingle(ctx, request.Enabled != nil && *request.Enabled)
	case "consume":
		err = s.client.SetConsume(ctx, request.Enabled != nil && *request.Enabled)
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported action %q (want pause|resume|toggle|play|stop|next|prev|seek|repeat|random|single|consume)", request.Action), "invalid_request")
		return
	}
	if err != nil {
		s.writeKernelError(w, err)
		return
	}
	s.writePlayback(w, ctx, http.StatusOK)
}

// ---------------------------------------------------------------------------
// /api/volume
// ---------------------------------------------------------------------------

type volumeRequest struct {
	Volume *int  `json:"volume"`
	Mute   *bool `json:"mute"`
}

func (s *Server) volume(w http.ResponseWriter, r *http.Request) {
	var request volumeRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request")
		return
	}
	if request.Volume == nil && request.Mute == nil {
		writeError(w, http.StatusBadRequest, "either volume or mute is required", "invalid_request")
		return
	}
	if s.client == nil {
		writeError(w, http.StatusServiceUnavailable, "playback kernel is unavailable", "mpd_unavailable")
		return
	}
	ctx := r.Context()

	if request.Volume != nil {
		volume := *request.Volume
		if volume < 0 || volume > 100 {
			writeError(w, http.StatusBadRequest, "volume must be between 0 and 100", "invalid_request")
			return
		}
		if err := s.client.SetVolume(ctx, volume); err != nil {
			s.writeKernelError(w, err)
			return
		}
		s.mu.Lock()
		if volume > 0 {
			s.lastVolume = volume
		}
		s.mu.Unlock()
	}
	if request.Mute != nil {
		var err error
		if *request.Mute {
			// MPD has no dedicated mute flag: remember the level and drop the
			// hardware mixer to zero, then restore it on unmute.
			status, statusErr := s.client.Status(ctx)
			if statusErr != nil {
				s.writeKernelError(w, statusErr)
				return
			}
			s.mu.Lock()
			if current := atoiOr(status["volume"], 0); current > 0 {
				s.lastVolume = current
			}
			s.mu.Unlock()
			err = s.client.SetVolume(ctx, 0)
		} else {
			s.mu.Lock()
			restore := s.lastVolume
			s.mu.Unlock()
			if restore <= 0 {
				restore = s.cfg.DefaultVolume
			}
			err = s.client.SetVolume(ctx, restore)
		}
		if err != nil {
			s.writeKernelError(w, err)
			return
		}
	}
	s.refreshMixer(ctx)
	s.writePlayback(w, ctx, http.StatusOK)
}

// ---------------------------------------------------------------------------
// /api/test-play
// ---------------------------------------------------------------------------

type testPlayResponse struct {
	Success   bool   `json:"success"`
	URL       string `json:"url"`
	Seconds   int    `json:"seconds"`
	Frequency int    `json:"frequency_hz"`
	Mixer     any    `json:"mixer"`
	Playback  any    `json:"playback"`
}

func (s *Server) testPlay(w http.ResponseWriter, r *http.Request) {
	if s.client == nil || s.tone == nil {
		writeError(w, http.StatusServiceUnavailable, "playback kernel is unavailable", "mpd_unavailable")
		return
	}
	ctx := r.Context()
	toneURL, err := s.tone.Enable()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not prepare the test tone: "+err.Error(), "tone_failed")
		return
	}
	if err := s.client.Clear(ctx); err != nil {
		s.writeKernelError(w, err)
		return
	}
	if err := s.client.Add(ctx, toneURL); err != nil {
		s.writeKernelError(w, err)
		return
	}
	if err := s.client.Play(ctx); err != nil {
		s.writeKernelError(w, err)
		return
	}
	playback, playbackErr := s.playbackSnapshot(ctx)
	if playbackErr != nil {
		playback = map[string]any{"state": "unknown", "error": playbackErr.Error()}
	}
	mixerState := any(nil)
	if s.mixer != nil {
		mixerState = s.mixer.State()
	}
	s.logger.Info("test tone dispatched to the playback kernel",
		"url", toneURL, "seconds", s.cfg.TestToneSeconds, "frequency_hz", s.cfg.TestToneFrequencyHz)
	writeJSON(w, http.StatusOK, testPlayResponse{
		Success:   true,
		URL:       toneURL,
		Seconds:   s.cfg.TestToneSeconds,
		Frequency: s.cfg.TestToneFrequencyHz,
		Mixer:     mixerState,
		Playback:  playback,
	})
}

// toneHandler serves the generated WAV to MPD over loopback.
func (s *Server) toneHandler(w http.ResponseWriter, r *http.Request) {
	if s.tone == nil {
		http.NotFound(w, r)
		return
	}
	audio, ok := s.tone.Lookup(r.PathValue("token"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

func (s *Server) writePlayback(w http.ResponseWriter, ctx context.Context, status int) {
	playback, err := s.playbackSnapshot(ctx)
	if err != nil {
		writeJSON(w, status, map[string]any{"success": true, "playback_error": err.Error()})
		return
	}
	mixerState := any(nil)
	if s.mixer != nil {
		mixerState = s.mixer.State()
	}
	writeJSON(w, status, map[string]any{"success": true, "playback": playback, "mixer": mixerState})
}

func (s *Server) refreshMixer(ctx context.Context) {
	if s.mixer == nil {
		return
	}
	if _, err := s.mixer.Refresh(ctx); err != nil {
		s.logger.Debug("could not refresh mixer snapshot", "error", err)
	}
}

// resolvePlayURI turns the plugin API's url field into the URI that is pushed
// onto the MPD queue.
//
// Accepted forms (verified against MPD 0.23 on the target host):
//
//	http(s)://...            streamed as-is (the LyraNest media-token stream)
//	file:///music/<rel>      rewritten to <rel>
//	/music/<rel>             rewritten to <rel>
//	<rel>                    used as-is (already music_directory-relative)
//
// MPD denies absolute local paths with "ACK [4@0] {add} Access denied", so the
// rewrite is mandatory rather than cosmetic.
func resolvePlayURI(raw string, musicDirectory string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("url is not a valid URL")
	}
	musicDirectory = strings.TrimRight(strings.TrimSpace(musicDirectory), "/")

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		if parsed.Host == "" {
			return "", errors.New("url must include a host")
		}
		return trimmed, nil
	case "file":
		if !strings.HasPrefix(parsed.Path, "/") {
			return "", errors.New("file url must be an absolute path")
		}
		return relativeToMusicDirectory(parsed.Path, musicDirectory)
	case "":
		if strings.HasPrefix(parsed.Path, "/") {
			return relativeToMusicDirectory(parsed.Path, musicDirectory)
		}
		return cleanLibraryRelativePath(parsed.Path)
	default:
		return "", errors.New("url must be an http(s) stream, a file:// path inside the music directory, or a library-relative path")
	}
}

// relativeToMusicDirectory rewrites an absolute path into the
// music_directory-relative form MPD accepts.
func relativeToMusicDirectory(absolute string, musicDirectory string) (string, error) {
	cleaned := path.Clean(absolute)
	if musicDirectory == "" {
		return "", errors.New("absolute paths require MPD_MUSIC_DIRECTORY to be configured")
	}
	if cleaned == musicDirectory {
		return "", errors.New("url must point at a file inside the music directory, not at the directory itself")
	}
	if !strings.HasPrefix(cleaned, musicDirectory+"/") {
		return "", fmt.Errorf("absolute path must be inside the music directory (%s)", musicDirectory)
	}
	return strings.TrimPrefix(cleaned, musicDirectory+"/"), nil
}

// cleanLibraryRelativePath normalises a relative path. Prepending "/" before
// path.Clean confines any ".." segments to the music directory, so a caller can
// never address a file outside the library.
func cleanLibraryRelativePath(value string) (string, error) {
	cleaned := strings.TrimPrefix(path.Clean("/"+value), "/")
	if cleaned == "" || cleaned == "." {
		return "", errors.New("url is empty")
	}
	return cleaned, nil
}

// redactURL removes userinfo and query values from a URL before logging.
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<unparsable>"
	}
	parsed.User = nil
	if parsed.RawQuery != "" {
		parsed.RawQuery = "redacted"
	}
	return parsed.String()
}

func atoiOr(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}

func atofOr(raw string, fallback float64) float64 {
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return fallback
	}
	return value
}

// ---------------------------------------------------------------------------
// Test tone generation
// ---------------------------------------------------------------------------

// ToneProvider renders a short sine WAV and publishes it on an unguessable
// loopback URL for a limited time.
//
// Using an HTTP URL (rather than a file under music_directory) means the
// self-test exercises the exact production path: MPD fetches a stream over
// HTTP and writes it to hw:0,0. It also works when the music library is not
// mounted at all.
type ToneProvider struct {
	cfg   config.Config
	audio []byte

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewToneProvider creates the test-tone provider.
func NewToneProvider(cfg config.Config) *ToneProvider {
	return &ToneProvider{cfg: cfg}
}

// Enable publishes the tone and returns the URL MPD must fetch.
func (t *ToneProvider) Enable() (string, error) {
	audio, err := t.wav()
	if err != nil {
		return "", err
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("generate tone token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	t.mu.Lock()
	t.audio = audio
	t.token = token
	t.expires = time.Now().Add(t.cfg.TestToneTTL)
	t.mu.Unlock()
	return fmt.Sprintf("http://%s:%d/tone/%s/tone.wav", t.cfg.MPDHost, t.cfg.Port, token), nil
}

// Lookup returns the tone bytes when the token matches and has not expired.
func (t *ToneProvider) Lookup(token string) ([]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token == "" || token == "" || token != t.token {
		return nil, false
	}
	if time.Now().After(t.expires) {
		return nil, false
	}
	return t.audio, true
}

func (t *ToneProvider) wav() ([]byte, error) {
	if t.audio != nil {
		return t.audio, nil
	}
	const sampleRate = 44100
	const channels = 2
	const bitsPerSample = 16
	totalFrames := sampleRate * t.cfg.TestToneSeconds
	dataSize := totalFrames * channels * (bitsPerSample / 8)

	var buffer bytes.Buffer
	buffer.WriteString("RIFF")
	binary.Write(&buffer, binary.LittleEndian, uint32(36+dataSize))
	buffer.WriteString("WAVE")
	buffer.WriteString("fmt ")
	binary.Write(&buffer, binary.LittleEndian, uint32(16))
	binary.Write(&buffer, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buffer, binary.LittleEndian, uint16(channels))
	binary.Write(&buffer, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buffer, binary.LittleEndian, uint32(sampleRate*channels*(bitsPerSample/8)))
	binary.Write(&buffer, binary.LittleEndian, uint16(channels*(bitsPerSample/8)))
	binary.Write(&buffer, binary.LittleEndian, uint16(bitsPerSample))
	buffer.WriteString("data")
	binary.Write(&buffer, binary.LittleEndian, uint32(dataSize))

	frequency := float64(t.cfg.TestToneFrequencyHz)
	// 20 ms raised-cosine fades keep the tone click-free on the analog output.
	fadeFrames := sampleRate / 50
	amplitude := 0.35
	for frame := 0; frame < totalFrames; frame++ {
		gain := 1.0
		if frame < fadeFrames {
			gain = 0.5 * (1 - math.Cos(math.Pi*float64(frame)/float64(fadeFrames)))
		} else if remaining := totalFrames - frame; remaining < fadeFrames {
			gain = 0.5 * (1 - math.Cos(math.Pi*float64(remaining)/float64(fadeFrames)))
		}
		value := int16(amplitude * gain * math.Sin(2*math.Pi*frequency*float64(frame)/float64(sampleRate)) * 32767)
		for channel := 0; channel < channels; channel++ {
			binary.Write(&buffer, binary.LittleEndian, value)
		}
	}
	return buffer.Bytes(), nil
}
