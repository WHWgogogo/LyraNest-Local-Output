// Command local-output is the self-developed shell of the LyraNest
// "server-side 3.5mm local audio output" sidecar.
//
// Responsibilities, in startup order:
//
//  1. probe the host sound card through /dev/snd (NEVER /proc/asound, which is
//     not readable inside the container);
//  2. force `Auto-Mute Mode = Disabled` and unmute Master/Headphone/Speaker,
//     then verify the result — without this the whole feature is silent while
//     every API answers "ok";
//  3. render /etc/mpd.conf and supervise the MPD playback kernel;
//  4. serve the LyraNest plugin-centre HTTP API on port 8091.
//
// MPD is GPL-2.0 third-party software executed as an independent process; this
// program neither links nor modifies it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lyranest/lyranest-local-output/internal/alsa"
	"github.com/lyranest/lyranest-local-output/internal/api"
	"github.com/lyranest/lyranest-local-output/internal/config"
	"github.com/lyranest/lyranest-local-output/internal/logging"
	"github.com/lyranest/lyranest-local-output/internal/mpd"
)

// Exit codes are stable so the compose restart policy and the operator can
// tell the failure classes apart.
const (
	exitOK               = 0
	exitConfig           = 2
	exitSoundCardMissing = 3
	exitMixerNotReady    = 4
	exitKernelFailed     = 5
)

func main() {
	// `local-output healthcheck` is what the container healthcheck runs.
	if len(os.Args) > 1 && strings.EqualFold(os.Args[1], "healthcheck") {
		os.Exit(runHealthcheck())
	}

	showVersion := flag.Bool("version", false, "print the shell version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("local-output " + api.Version)
		return
	}

	if err := run(); err != nil {
		// run() already logged the operator-facing detail.
		os.Exit(exitCodeFor(err))
	}
}

// exitError carries the process exit code alongside the error.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func exitCodeFor(err error) int {
	var coded *exitError
	if errors.As(err, &coded) {
		return coded.code
	}
	return 1
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid configuration:", err)
		return &exitError{code: exitConfig, err: err}
	}
	logger := logging.New(cfg.LogLevel)
	slog.SetDefault(logger)

	logger.Info("lyranest local-output starting",
		"version", api.Version,
		"port", cfg.Port,
		"alsa_device", cfg.ALSAHardwareDevice(),
		"mpd", cfg.MPDAddress(),
		"music_directory", cfg.MPDMusicDirectory,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Step 1: sound card self-check.
	mixer := alsa.New(cfg, logger)
	report, probeErr := alsa.Probe(ctx, cfg, mixer, logger)
	if probeErr != nil {
		logger.Error("sound card self-check failed: this container does not apply to this host",
			"error", probeErr,
			"hint", "if the host has no sound card, do not enable deploy/docker-compose.local-output.yml",
		)
		return &exitError{code: exitSoundCardMissing, err: probeErr}
	}
	logger.Info("sound card ready",
		"device", report.HardwareDevice,
		"pcm_access", report.PCMAccess,
		"control_access", report.ControlAccess,
		"controls", strings.Join(report.ControlNames, ", "),
	)

	// Step 2 (the decisive step): mixer initialisation.
	mixerState, mixerErr := mixer.Init(ctx)
	if mixerErr != nil {
		logger.Error("mixer initialisation failed; refusing to start the playback kernel to avoid a silent-but-green container",
			"error", mixerErr,
			"auto_mute_state", mixerState.AutoMuteState,
			"hint", "check `amixer -c "+fmt.Sprint(cfg.Card)+" scontrols` on the host and set ALSA_* control names to match",
		)
		return &exitError{code: exitMixerNotReady, err: mixerErr}
	}
	if !mixerState.AutoMuteFixed {
		logger.Warn("Auto-Mute Mode was not disabled; if there is no sound, this is the first thing to check",
			"auto_mute_state", mixerState.AutoMuteState)
	}

	// Step 3: MPD playback kernel.
	manager := mpd.NewManager(cfg, logger)
	if err := manager.LoadTemplate(); err != nil {
		logger.Error("could not load the MPD configuration template", "error", err)
		return &exitError{code: exitKernelFailed, err: err}
	}
	manager.SetMixerControl(mixerControlFor(report, cfg))
	if err := manager.Start(ctx); err != nil {
		logger.Error("could not start the MPD playback kernel", "error", err)
		return &exitError{code: exitKernelFailed, err: err}
	}
	manager.Supervise(ctx)

	// Apply the configured default volume through MPD so the plugin-centre
	// volume slider starts from a predictable value.
	client := manager.Client()
	if err := client.SetVolume(ctx, cfg.DefaultVolume); err != nil {
		logger.Warn("could not apply the default volume", "error", err, "volume", cfg.DefaultVolume)
	}

	// Step 4: HTTP API.
	tone := api.NewToneProvider(cfg)
	server := api.New(api.Options{
		Config:  cfg,
		Logger:  logger,
		Report:  report,
		Mixer:   mixer,
		Manager: manager,
		Tone:    tone,
	})
	httpServer := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           server.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http api listening", "address", cfg.Addr())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			logger.Error("http api failed", "error", err)
			_ = manager.Stop(context.Background())
			return &exitError{code: exitKernelFailed, err: err}
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown was not clean", "error", err)
	}
	if err := manager.Stop(shutdownCtx); err != nil {
		logger.Warn("could not stop the playback kernel cleanly", "error", err)
	}
	logger.Info("lyranest local-output stopped")
	return nil
}

// mixerControlFor decides whether MPD may use a hardware mixer. MPD refuses to
// start when a configured hardware mixer control does not exist, so an absent
// control degrades to MPD's software mixer instead of a crash loop.
func mixerControlFor(report alsa.Report, cfg config.Config) string {
	if strings.TrimSpace(cfg.MPDMixerControl) == "" {
		return ""
	}
	for _, control := range report.ControlNames {
		if control == cfg.MPDMixerControl {
			return cfg.MPDMixerControl
		}
	}
	if len(report.ControlNames) == 0 {
		// The control list could not be read; keep the configured value so the
		// behaviour matches the documented default.
		return cfg.MPDMixerControl
	}
	return ""
}

// runHealthcheck probes the local HTTP API. It is used by the container
// healthcheck and returns a process exit code.
func runHealthcheck() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: invalid configuration:", err)
		return 1
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: request failed:", err)
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: unhealthy status", response.StatusCode)
		return 1
	}
	return 0
}
