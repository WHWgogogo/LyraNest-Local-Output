package mpd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/lyranest/lyranest-local-output/internal/config"
)

// TemplateData is the substitution model for assets/mpd.conf.template.
type TemplateData struct {
	MusicDirectory    string
	PlaylistDirectory string
	DBFile            string
	StateFile         string
	BindAddress       string
	Port              int
	LogFile           string
	Device            string
	MixerDevice       string
	MixerControl      string
}

// Snapshot is the process-level view of the playback kernel.
type Snapshot struct {
	Running     bool      `json:"running"`
	PID         int       `json:"pid,omitempty"`
	Restarts    int       `json:"restarts"`
	ConfigPath  string    `json:"config_path"`
	LastError   string    `json:"last_error,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	ProtocolVer string    `json:"protocol_version,omitempty"`
	MixerMode   string    `json:"mixer_mode"`
}

// Manager owns the MPD child process.
type Manager struct {
	cfg    config.Config
	logger *slog.Logger
	client *Client

	mu         sync.Mutex
	command    *exec.Cmd
	running    bool
	restarts   int
	lastError  string
	startedAt  time.Time
	mixerMode  string
	tail       *ringBuffer
	template   *template.Template
	supervise  sync.WaitGroup
	stopped    bool
	configPath string
}

// NewManager creates the MPD process manager.
func NewManager(cfg config.Config, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:        cfg,
		logger:     logger,
		client:     NewClient(cfg.MPDAddress(), 5*time.Second),
		tail:       newRingBuffer(60),
		configPath: cfg.MPDConfPath,
	}
}

// Client returns the protocol client bound to this manager's MPD instance.
func (m *Manager) Client() *Client { return m.client }

// ConfigPath is the rendered MPD configuration path.
func (m *Manager) ConfigPath() string { return m.configPath }

// SetMixerControl decides between MPD's hardware and software mixer. The shell
// passes an empty control when the probed card does not expose it, because MPD
// refuses to start when a hardware mixer control is missing.
func (m *Manager) SetMixerControl(control string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.MPDMixerControl = control
	if strings.TrimSpace(control) == "" {
		m.mixerMode = "software"
	} else {
		m.mixerMode = "hardware"
	}
}

// MixerMode reports "hardware" or "software".
func (m *Manager) MixerMode() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mixerMode == "" {
		return "hardware"
	}
	return m.mixerMode
}

// TemplateData builds the substitution model from the configuration.
func (m *Manager) TemplateData() TemplateData {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	return TemplateData{
		MusicDirectory:    cfg.MPDMusicDirectory,
		PlaylistDirectory: filepath.Join(cfg.MPDDataDirectory, "playlists"),
		DBFile:            filepath.Join(cfg.MPDDataDirectory, "tag_cache"),
		StateFile:         filepath.Join(cfg.MPDDataDirectory, "state"),
		BindAddress:       cfg.MPDHost,
		Port:              cfg.MPDPort,
		LogFile:           cfg.MPDLogFile,
		Device:            cfg.ALSAHardwareDevice(),
		MixerDevice:       cfg.ALSAHardwareCard(),
		MixerControl:      cfg.MPDMixerControl,
	}
}

// LoadTemplate reads the MPD configuration template. When the file is absent
// the built-in fallback template is used so the shell stays runnable outside
// the container image.
func (m *Manager) LoadTemplate() error {
	path := m.cfg.MPDTemplatePath
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read mpd template %s: %w", path, err)
		}
		m.logger.Warn("mpd config template not found, using built-in fallback", "path", path)
		raw = []byte(fallbackTemplate)
	}
	parsed, err := template.New("mpd.conf").Parse(string(raw))
	if err != nil {
		return fmt.Errorf("parse mpd template %s: %w", path, err)
	}
	m.mu.Lock()
	m.template = parsed
	m.mu.Unlock()
	return nil
}

// RenderConfig renders the MPD configuration text.
func (m *Manager) RenderConfig() (string, error) {
	m.mu.Lock()
	parsed := m.template
	m.mu.Unlock()
	if parsed == nil {
		return "", errors.New("mpd config template is not loaded")
	}
	var buffer bytes.Buffer
	if err := parsed.Execute(&buffer, m.TemplateData()); err != nil {
		return "", fmt.Errorf("render mpd config: %w", err)
	}
	return buffer.String(), nil
}

// EnsureDirectories creates the directories MPD needs.
func (m *Manager) EnsureDirectories() error {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	for _, directory := range []string{cfg.MPDDataDirectory, filepath.Join(cfg.MPDDataDirectory, "playlists"), cfg.MPDMusicDirectory} {
		if directory == "" {
			continue
		}
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", directory, err)
		}
	}
	return nil
}

// WriteConfig renders and writes the MPD configuration file.
func (m *Manager) WriteConfig() (string, error) {
	rendered, err := m.RenderConfig()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(m.configPath), 0o755); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(m.configPath, []byte(rendered), 0o644); err != nil {
		return "", fmt.Errorf("write mpd config %s: %w", m.configPath, err)
	}
	return rendered, nil
}

// Start renders the configuration and launches `mpd --no-daemon`.
func (m *Manager) Start(ctx context.Context) error {
	if err := m.EnsureDirectories(); err != nil {
		return err
	}
	if _, err := m.WriteConfig(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	return m.spawn(ctx)
}

func (m *Manager) spawn(ctx context.Context) error {
	m.mu.Lock()
	binary := m.cfg.MPDBinary
	configPath := m.configPath
	startTimeout := m.cfg.MPDStartTimeout
	m.mu.Unlock()

	command := exec.Command(binary, "--no-daemon", configPath)
	command.Stdout = &logWriter{logger: m.logger, level: slog.LevelDebug, prefix: "mpd", tail: m.tail}
	command.Stderr = &logWriter{logger: m.logger, level: slog.LevelInfo, prefix: "mpd", tail: m.tail}
	if err := command.Start(); err != nil {
		wrapped := fmt.Errorf("start %s --no-daemon %s: %w", binary, configPath, err)
		m.mu.Lock()
		m.lastError = wrapped.Error()
		m.mu.Unlock()
		return wrapped
	}

	m.mu.Lock()
	m.command = command
	m.running = true
	m.startedAt = time.Now().UTC()
	m.mu.Unlock()
	m.logger.Info("mpd started", "pid", command.Process.Pid, "config", configPath, "mixer_mode", m.MixerMode())

	if err := m.client.WaitReady(ctx, startTimeout); err != nil {
		m.logger.Error("mpd did not become ready", "error", err, "mpd_log_tail", m.tail.String())
		_ = m.kill()
		wrapped := fmt.Errorf("%w (mpd log tail: %s)", err, m.tail.String())
		m.mu.Lock()
		m.lastError = wrapped.Error()
		m.running = false
		m.mu.Unlock()
		return wrapped
	}
	m.mu.Lock()
	m.lastError = ""
	m.mu.Unlock()
	m.logger.Info("mpd control socket is ready", "address", m.client.Address(), "protocol", m.client.Version())
	return nil
}

// Supervise keeps MPD alive until the context is cancelled, restarting it a
// bounded number of times. It never loops forever: an unbounded restart storm
// on a host without a usable sound card is explicitly forbidden by the work
// order.
func (m *Manager) Supervise(ctx context.Context) {
	m.supervise.Add(1)
	go func() {
		defer m.supervise.Done()
		for {
			m.mu.Lock()
			command := m.command
			stopped := m.stopped
			m.mu.Unlock()
			if stopped || command == nil {
				return
			}
			waitErr := command.Wait()

			m.mu.Lock()
			m.running = false
			m.command = nil
			if m.stopped {
				m.mu.Unlock()
				return
			}
			if ctx.Err() != nil {
				m.mu.Unlock()
				return
			}
			if m.restarts >= m.cfg.MPDMaxRestarts {
				m.lastError = fmt.Sprintf("mpd exited (%v) and the restart budget of %d is exhausted", waitErr, m.cfg.MPDMaxRestarts)
				m.mu.Unlock()
				m.logger.Error("mpd restart budget exhausted; playback disabled until the container restarts",
					"error", waitErr, "restarts", m.cfg.MPDMaxRestarts)
				return
			}
			m.restarts++
			attempt := m.restarts
			m.mu.Unlock()

			m.logger.Warn("mpd exited; restarting", "error", waitErr, "attempt", attempt, "max", m.cfg.MPDMaxRestarts)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * time.Second):
			}
			if err := m.spawn(ctx); err != nil {
				m.logger.Error("mpd restart failed", "error", err, "attempt", attempt)
			}
		}
	}()
}

func (m *Manager) kill() error {
	m.mu.Lock()
	command := m.command
	m.mu.Unlock()
	if command == nil || command.Process == nil {
		return nil
	}
	_ = command.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_, _ = command.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
	}
	return nil
}

// Stop terminates MPD and prevents further supervision.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	m.stopped = true
	command := m.command
	m.mu.Unlock()
	if command == nil || command.Process == nil {
		return nil
	}
	m.logger.Info("stopping mpd", "pid", command.Process.Pid)
	if err := command.Process.Signal(os.Interrupt); err != nil {
		_ = command.Process.Kill()
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		_ = command.Process.Kill()
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
	}
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
	return nil
}

// Snapshot reports the current process state.
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := Snapshot{
		Running:     m.running,
		Restarts:    m.restarts,
		ConfigPath:  m.configPath,
		LastError:   m.lastError,
		StartedAt:   m.startedAt,
		ProtocolVer: m.client.Version(),
		MixerMode:   m.mixerMode,
	}
	if snapshot.MixerMode == "" {
		snapshot.MixerMode = "hardware"
	}
	if m.command != nil && m.command.Process != nil {
		snapshot.PID = m.command.Process.Pid
	}
	return snapshot
}

// ---------------------------------------------------------------------------
// log plumbing
// ---------------------------------------------------------------------------

// logWriter forwards MPD's stdout/stderr into the container log and keeps the
// last lines in a ring buffer so a startup failure can be reported in full.
type logWriter struct {
	logger *slog.Logger
	level  slog.Level
	prefix string
	tail   *ringBuffer
	buffer bytes.Buffer
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buffer.Write(p)
	for {
		line, err := w.buffer.ReadString('\n')
		if err != nil {
			// Keep the partial line for the next write.
			w.buffer.Reset()
			w.buffer.WriteString(line)
			break
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			continue
		}
		if w.tail != nil {
			w.tail.Add(trimmed)
		}
		w.logger.Log(context.Background(), w.level, w.prefix, "line", trimmed)
	}
	return len(p), nil
}

// ringBuffer keeps the last n lines.
type ringBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newRingBuffer(max int) *ringBuffer {
	return &ringBuffer{max: max, lines: make([]string, 0, max)}
}

func (r *ringBuffer) Add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, " | ")
}

// fallbackTemplate mirrors assets/mpd.conf.template for development runs where
// the template file is not installed.
const fallbackTemplate = `music_directory     "{{.MusicDirectory}}"
playlist_directory  "{{.PlaylistDirectory}}"
db_file             "{{.DBFile}}"
state_file          "{{.StateFile}}"
bind_to_address     "{{.BindAddress}}"
port                "{{.Port}}"
filesystem_charset  "UTF-8"
auto_update         "no"
restore_paused      "no"

audio_output {
    type            "alsa"
    name            "LyraNest Local Output"
    device          "{{.Device}}"
{{if .MixerControl}}    mixer_type      "hardware"
    mixer_device    "{{.MixerDevice}}"
    mixer_control   "{{.MixerControl}}"
{{else}}    mixer_type      "software"
{{end}}    auto_resample   "no"
}
`
