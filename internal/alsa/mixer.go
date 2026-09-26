// Package alsa implements sound-card probing and mixer initialisation.
//
// Two hard constraints from the field report shape this package:
//
//  1. /proc/asound is NOT readable inside the container, so card and control
//     enumeration goes through /dev/snd plus the ALSA control API exposed by
//     `amixer`. No code path may read /proc/asound.
//  2. `Auto-Mute Mode` defaults to Enabled on the verified Realtek ALC269VB
//     codec and silently mutes BOTH output pins (Amp-Out vals [0x80 0x80]).
//     `amixer` still reports Headphone as [on] in that state, so the only
//     reliable readiness signal is "the Disabled value was written and read
//     back". Without it the whole feature is silent while every API is green.
package alsa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lyranest/lyranest-local-output/internal/config"
)

// Runner executes an external command and returns its combined output. It is
// injectable so the mixer logic is testable without alsa-utils installed.
type Runner func(ctx context.Context, name string, args ...string) (stdout string, stderr string, err error)

// ExecRunner runs commands with os/exec.
func ExecRunner(ctx context.Context, name string, args ...string) (string, string, error) {
	command := exec.CommandContext(ctx, name, args...)
	var stdout strings.Builder
	var stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

// State is the mixer snapshot exposed through /healthz and /api/status.
type State struct {
	// Initialized is true once Init completed successfully.
	Initialized bool `json:"initialized"`
	// Verified is true when the effective playback control reports [on].
	Verified bool `json:"verified"`
	// AutoMuteFixed is the decisive readiness flag: the Auto-Mute Mode control
	// was present and now reads Disabled.
	AutoMuteFixed bool `json:"auto_mute_fixed"`
	// AutoMuteState is the observed Auto-Mute Mode value: Disabled, Enabled or
	// "unsupported" when the card has no such control (e.g. a USB DAC).
	AutoMuteState string `json:"auto_mute_state"`
	// Unmuted lists the controls that were explicitly unmuted.
	Unmuted []string `json:"unmuted"`
	// Missing lists the expected controls this card does not have.
	Missing []string `json:"missing"`
	// Warnings records non-fatal problems worth showing to the operator.
	Warnings []string `json:"warnings,omitempty"`
	// VolumePercent is the current hardware volume of the effective control.
	VolumePercent int `json:"volume_percent"`
	// Muted is the current switch state of the effective control.
	Muted bool `json:"muted"`
	// EffectiveControl is the control used for volume and verification.
	EffectiveControl string `json:"effective_control"`
	// Controls are all simple mixer controls the card exposes.
	Controls []string `json:"controls,omitempty"`
	// InitializedAt is when Init last succeeded.
	InitializedAt time.Time `json:"initialized_at,omitempty"`
	// LastError is the last fatal mixer error, if any.
	LastError string `json:"last_error,omitempty"`
}

// Mixer drives the card mixer through amixer.
type Mixer struct {
	cfg    config.Config
	logger *slog.Logger
	run    Runner

	mu    sync.Mutex
	state State
}

// New creates a Mixer bound to the configured card.
func New(cfg config.Config, logger *slog.Logger) *Mixer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Mixer{cfg: cfg, logger: logger, run: ExecRunner}
}

// WithRunner replaces the command runner (tests only).
func (m *Mixer) WithRunner(run Runner) *Mixer {
	m.run = run
	return m
}

// State returns the last known mixer snapshot.
func (m *Mixer) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *Mixer) setState(mutate func(*State)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mutate(&m.state)
}

func (m *Mixer) amixer(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-c", strconv.Itoa(m.cfg.Card)}, args...)
	stdout, stderr, err := m.run(ctx, m.cfg.AmixerPath, full...)
	if err != nil {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = strings.TrimSpace(stdout)
		}
		if message == "" {
			message = err.Error()
		}
		return stdout, fmt.Errorf("amixer %s: %s", strings.Join(args, " "), message)
	}
	return stdout, nil
}

// Controls lists the card's simple mixer controls via the ALSA control API.
func (m *Mixer) Controls(ctx context.Context) ([]string, error) {
	stdout, err := m.amixer(ctx, "scontrols")
	if err != nil {
		return nil, err
	}
	return ParseSimpleControls(stdout), nil
}

// SimpleGet reads one simple mixer control.
func (m *Mixer) SimpleGet(ctx context.Context, control string) (string, error) {
	return m.amixer(ctx, "sget", control)
}

// SimpleSet writes one simple mixer control. Extra arguments are passed
// verbatim, e.g. SimpleSet(ctx, "Master", "80%", "unmute").
func (m *Mixer) SimpleSet(ctx context.Context, control string, values ...string) (string, error) {
	args := append([]string{"sset", control}, values...)
	return m.amixer(ctx, args...)
}

// Init performs the mandatory mixer initialisation sequence.
//
// Order matters and mirrors the field-verified recipe:
//
//	amixer -c 0 sset "Auto-Mute Mode" Disabled
//	amixer -c 0 sset "Master"    80% unmute
//	amixer -c 0 sset "Headphone" unmute
//	amixer -c 0 sset "Speaker"   unmute
//
// Missing controls are reported as warnings rather than errors: USB DACs and
// HDMI cards legitimately lack some of them. A failure to disable Auto-Mute
// Mode on a card that HAS that control is fatal, because it is the documented
// silent-failure trap.
func (m *Mixer) Init(ctx context.Context) (State, error) {
	state := State{
		AutoMuteState: "unsupported",
		Unmuted:       []string{},
		Missing:       []string{},
		Warnings:      []string{},
	}

	controls, err := m.Controls(ctx)
	if err != nil {
		state.LastError = err.Error()
		m.setState(func(s *State) { *s = state })
		return state, fmt.Errorf("enumerate ALSA controls on card %d: %w", m.cfg.Card, err)
	}
	state.Controls = controls
	known := make(map[string]bool, len(controls))
	for _, control := range controls {
		known[control] = true
	}
	m.logger.Info("alsa mixer controls discovered", "card", m.cfg.Card, "controls", strings.Join(controls, ", "))

	// Step 1 (decisive): disable Auto-Mute Mode.
	if !known[m.cfg.AutoMuteControl] {
		state.Warnings = append(state.Warnings, fmt.Sprintf("card %d has no %q control; skipping the Auto-Mute fix (expected on USB DACs)", m.cfg.Card, m.cfg.AutoMuteControl))
		m.logger.Warn("auto-mute control absent; skipping fix", "card", m.cfg.Card, "control", m.cfg.AutoMuteControl)
	} else if !m.cfg.FixAutoMute {
		state.Warnings = append(state.Warnings, "Auto-Mute fix disabled by configuration; audio may stay silent")
		m.logger.Warn("auto-mute fix disabled by configuration")
	} else {
		if _, err := m.SimpleSet(ctx, m.cfg.AutoMuteControl, m.cfg.AutoMuteDisabledValue); err != nil {
			state.LastError = err.Error()
			m.setState(func(s *State) { *s = state })
			return state, fmt.Errorf("disable %q on card %d: %w", m.cfg.AutoMuteControl, m.cfg.Card, err)
		}
		observed, readErr := m.SimpleGet(ctx, m.cfg.AutoMuteControl)
		if readErr != nil {
			state.LastError = readErr.Error()
			m.setState(func(s *State) { *s = state })
			return state, fmt.Errorf("read back %q on card %d: %w", m.cfg.AutoMuteControl, m.cfg.Card, readErr)
		}
		value, ok := ParseItemValue(observed, 0)
		if !ok {
			state.LastError = "could not parse Auto-Mute Mode value"
			m.setState(func(s *State) { *s = state })
			return state, errors.New("could not parse Auto-Mute Mode value from amixer output")
		}
		state.AutoMuteState = value
		if !strings.EqualFold(value, m.cfg.AutoMuteDisabledValue) {
			state.LastError = fmt.Sprintf("Auto-Mute Mode is %q, want %q", value, m.cfg.AutoMuteDisabledValue)
			m.setState(func(s *State) { *s = state })
			return state, fmt.Errorf("Auto-Mute Mode is %q after write, want %q", value, m.cfg.AutoMuteDisabledValue)
		}
		state.AutoMuteFixed = true
		m.logger.Info("auto-mute disabled (this is the step that makes the 3.5mm jack audible)",
			"card", m.cfg.Card, "control", m.cfg.AutoMuteControl, "value", value)
	}

	// Step 2: unmute the output controls.
	type target struct {
		name   string
		values []string
	}
	candidates := []string{
		m.cfg.HeadphoneControl,
		m.cfg.MasterControl,
		m.cfg.SpeakerControl,
		"PCM",
		"Playback",
		"Line Out",
		"Lineout",
		"DAC",
		"Front",
		"Digital",
	}
	var targets []target
	for _, c := range candidates {
		if c != "" && known[c] {
			targets = append(targets, target{
				name:   c,
				values: []string{fmt.Sprintf("%d%%", m.cfg.DefaultVolume), "unmute"},
			})
		}
	}
	if len(targets) == 0 && len(controls) > 0 {
		for _, c := range controls {
			if c != m.cfg.AutoMuteControl {
				targets = append(targets, target{
					name:   c,
					values: []string{fmt.Sprintf("%d%%", m.cfg.DefaultVolume), "unmute"},
				})
			}
		}
	}
	for _, item := range targets {
		if item.name == "" || !known[item.name] {
			continue
		}
		if _, err := m.SimpleSet(ctx, item.name, item.values...); err != nil {
			// Some controls (such as volume-only PCM without an on/off switch) fail on "unmute".
			// Try volume and unmute separately.
			_, vErr := m.SimpleSet(ctx, item.name, fmt.Sprintf("%d%%", m.cfg.DefaultVolume))
			_, uErr := m.SimpleSet(ctx, item.name, "unmute")
			if vErr != nil && uErr != nil {
				state.Warnings = append(state.Warnings, err.Error())
				m.logger.Warn("mixer control adjust warning", "control", item.name, "error", err)
				continue
			}
		}
		state.Unmuted = append(state.Unmuted, item.name)
		m.logger.Info("mixer control adjusted", "control", item.name, "values", strings.Join(item.values, " "))
	}

	// Step 3: verify. Headphone is the jack we care about; Master is the
	// fallback for cards without a Headphone control; then PCM, etc.
	effective := ""
	for _, candidate := range candidates {
		if candidate != "" && known[candidate] {
			effective = candidate
			break
		}
	}
	if effective == "" && len(controls) > 0 {
		for _, c := range controls {
			if c != m.cfg.AutoMuteControl {
				effective = c
				break
			}
		}
	}

	if effective == "" {
		m.logger.Warn("no hardware playback mixer control found on this card; falling back to software volume", "card", m.cfg.Card)
		state.EffectiveControl = ""
		state.Verified = true
		state.Initialized = true
		state.InitializedAt = time.Now().UTC()
		m.setState(func(s *State) { *s = state })
		return state, nil
	}

	state.EffectiveControl = effective
	state.Verified = true
	if snapshot, readErr := m.SimpleGet(ctx, effective); readErr != nil {
		state.Warnings = append(state.Warnings, readErr.Error())
	} else {
		if muted, hasSwitch := ParseSwitchState(snapshot); hasSwitch {
			state.Muted = muted
			state.Verified = !muted
		} else {
			state.Muted = false
			state.Verified = true
		}
		if percent, ok := ParsePercent(snapshot); ok {
			state.VolumePercent = percent
		}
	}
	if !state.Verified {
		state.LastError = fmt.Sprintf("mixer control %q reports [off]", effective)
		m.setState(func(s *State) { *s = state })
		return state, fmt.Errorf("mixer control %q reports [off]; refusing to start playback", effective)
	}
	if !state.AutoMuteFixed && known[m.cfg.AutoMuteControl] {
		state.LastError = "auto-mute fix did not take effect"
		m.setState(func(s *State) { *s = state })
		return state, errors.New("auto-mute fix did not take effect")
	}

	state.Initialized = true
	state.InitializedAt = time.Now().UTC()
	m.setState(func(s *State) { *s = state })
	m.logger.Info("mixer initialisation complete",
		"card", m.cfg.Card,
		"auto_mute_fixed", state.AutoMuteFixed,
		"auto_mute_state", state.AutoMuteState,
		"effective_control", state.EffectiveControl,
		"volume_percent", state.VolumePercent,
	)
	return state, nil
}

// SetVolume applies a hardware volume through amixer. It is used at startup and
// as a fallback when MPD's hardware mixer is unavailable; the plugin API routes
// volume through MPD so that both stay consistent.
func (m *Mixer) SetVolume(ctx context.Context, percent int) (State, error) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	control := m.State().EffectiveControl
	if control == "" {
		m.setState(func(s *State) {
			s.VolumePercent = percent
			s.Muted = false
			s.Verified = true
		})
		return m.State(), nil
	}
	if _, err := m.SimpleSet(ctx, control, fmt.Sprintf("%d%%", percent), "unmute"); err != nil {
		if _, err2 := m.SimpleSet(ctx, control, fmt.Sprintf("%d%%", percent)); err2 != nil {
			return m.State(), err
		}
	}
	m.setState(func(s *State) {
		s.VolumePercent = percent
		s.Muted = false
		s.Verified = true
	})
	return m.State(), nil
}

// SetMute toggles the hardware switch of the effective control.
func (m *Mixer) SetMute(ctx context.Context, muted bool) (State, error) {
	control := m.State().EffectiveControl
	if control == "" {
		m.setState(func(s *State) {
			s.Muted = muted
			s.Verified = !muted
		})
		return m.State(), nil
	}
	value := "unmute"
	if muted {
		value = "mute"
	}
	if _, err := m.SimpleSet(ctx, control, value); err != nil {
		return m.State(), err
	}
	m.setState(func(s *State) {
		s.Muted = muted
		s.Verified = !muted
	})
	return m.State(), nil
}

// Refresh re-reads the effective control so /api/status and /healthz report the
// hardware state rather than a stale snapshot.
func (m *Mixer) Refresh(ctx context.Context) (State, error) {
	control := m.State().EffectiveControl
	if control == "" {
		return m.State(), nil
	}
	output, err := m.SimpleGet(ctx, control)
	if err != nil {
		return m.State(), err
	}
	percent, hasPercent := ParsePercent(output)
	muted, hasSwitch := ParseSwitchState(output)
	m.setState(func(s *State) {
		if hasPercent {
			s.VolumePercent = percent
		}
		if hasSwitch {
			s.Muted = muted
			s.Verified = !muted
		} else {
			s.Muted = false
			s.Verified = true
		}
		s.EffectiveControl = control
	})
	return m.State(), nil
}

// ---------------------------------------------------------------------------
// amixer output parsing (pure helpers, unit tested)
// ---------------------------------------------------------------------------

// ParseSimpleControls extracts control names from `amixer scontrols` output.
//
//	Simple mixer control 'Master',0
func ParseSimpleControls(output string) []string {
	controls := []string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Simple mixer control ") {
			continue
		}
		rest := strings.TrimPrefix(line, "Simple mixer control ")
		start := strings.Index(rest, "'")
		if start < 0 {
			continue
		}
		end := strings.Index(rest[start+1:], "'")
		if end < 0 {
			continue
		}
		name := rest[start+1 : start+1+end]
		if name != "" {
			controls = append(controls, name)
		}
	}
	return controls
}

// ParseItemValue reads `ItemN: 'Value'` from `amixer sget <enum control>`.
func ParseItemValue(output string, index int) (string, bool) {
	prefix := fmt.Sprintf("Item%d:", index)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		start := strings.Index(rest, "'")
		if start < 0 {
			return rest, rest != ""
		}
		end := strings.Index(rest[start+1:], "'")
		if end < 0 {
			return rest, rest != ""
		}
		return rest[start+1 : start+1+end], true
	}
	return "", false
}

// ParsePercent reads the last `[NN%]` value in an `amixer sget` output.
func ParsePercent(output string) (int, bool) {
	best := -1
	for _, line := range strings.Split(output, "\n") {
		rest := line
		for {
			open := strings.Index(rest, "[")
			if open < 0 {
				break
			}
			close := strings.Index(rest[open:], "]")
			if close < 0 {
				break
			}
			token := rest[open+1 : open+close]
			if strings.HasSuffix(token, "%") {
				if value, err := strconv.Atoi(strings.TrimSuffix(token, "%")); err == nil {
					best = value
				}
			}
			rest = rest[open+close+1:]
		}
	}
	if best < 0 {
		return 0, false
	}
	return best, true
}

// ParseSwitchState reads the last `[on]` / `[off]` token in an `amixer sget`
// output and returns true when the control is MUTED.
func ParseSwitchState(output string) (muted bool, ok bool) {
	found := false
	muted = false
	for _, line := range strings.Split(output, "\n") {
		rest := line
		for {
			open := strings.Index(rest, "[")
			if open < 0 {
				break
			}
			close := strings.Index(rest[open:], "]")
			if close < 0 {
				break
			}
			token := rest[open+1 : open+close]
			switch token {
			case "on":
				found = true
				muted = false
			case "off":
				found = true
				muted = true
			}
			rest = rest[open+close+1:]
		}
	}
	return muted, found
}
