package alsa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/lyranest/lyranest-local-output/internal/config"
)

// AccessState describes whether a device node can be opened right now.
type AccessState string

const (
	// AccessOK means the node exists and opens read/write.
	AccessOK AccessState = "ok"
	// AccessBusy means another process holds the device exclusively. ALSA has
	// no dmix on the verified hardware, so this is expected while playing.
	AccessBusy AccessState = "busy"
	// AccessPermissionDenied means the container lacks the required rights.
	AccessPermissionDenied AccessState = "permission-denied"
	// AccessMissing means the node does not exist.
	AccessMissing AccessState = "missing"
	// AccessError is any other open failure.
	AccessError AccessState = "error"
)

// Card is one ALSA card discovered from /dev/snd.
type Card struct {
	Index           int    `json:"index"`
	ControlPath     string `json:"control_path"`
	PlaybackDevices []int  `json:"playback_devices"`
	HardwareID      string `json:"hardware_id"`
}

// Report is the sound-card probe result.
type Report struct {
	// SoundDirectory is the directory that was inspected (/dev/snd).
	SoundDirectory string `json:"sound_directory"`
	// Present is true when the directory exists and holds at least one card.
	Present bool `json:"present"`
	// Cards are the discovered cards.
	Cards []Card `json:"cards"`
	// Card / PCMDevice are the configured selection.
	Card      int `json:"card"`
	PCMDevice int `json:"pcm_device"`
	// HardwareDevice is the ALSA PCM name, e.g. "hw:0,0".
	HardwareDevice string `json:"hardware_device"`
	// ControlPath / PCMPlaybackPath are the device nodes for the selection.
	ControlPath     string `json:"control_path"`
	PCMPlaybackPath string `json:"pcm_playback_path"`
	// ControlAccess / PCMAccess are the open() results for those nodes.
	ControlAccess AccessState `json:"control_access"`
	PCMAccess     AccessState `json:"pcm_access"`
	// ControlNames are the simple mixer controls, read through the ALSA
	// control API (amixer). Never from /proc/asound.
	ControlNames []string `json:"control_names,omitempty"`
	// Reason explains a non-ready probe in operator language.
	Reason string `json:"reason,omitempty"`
}

// Ready reports whether playback can be attempted.
func (r Report) Ready() bool {
	if !r.Present {
		return false
	}
	return r.PCMAccess == AccessOK || r.PCMAccess == AccessBusy
}

var controlNodePattern = regexp.MustCompile(`^controlC(\d+)$`)
var pcmPlaybackNodePattern = regexp.MustCompile(`^pcmC(\d+)D(\d+)p$`)

// EnumerateCards lists ALSA cards using /dev/snd only. /proc/asound is
// deliberately not consulted: it is not mounted inside the container.
func EnumerateCards(soundDirectory string) ([]Card, error) {
	entries, err := os.ReadDir(soundDirectory)
	if err != nil {
		return nil, err
	}
	byIndex := map[int]*Card{}
	for _, entry := range entries {
		name := entry.Name()
		if match := controlNodePattern.FindStringSubmatch(name); match != nil {
			index, convErr := strconv.Atoi(match[1])
			if convErr != nil {
				continue
			}
			if byIndex[index] == nil {
				byIndex[index] = &Card{Index: index, PlaybackDevices: []int{}}
			}
			byIndex[index].ControlPath = filepath.Join(soundDirectory, name)
			continue
		}
		if match := pcmPlaybackNodePattern.FindStringSubmatch(name); match != nil {
			cardIndex, convErr := strconv.Atoi(match[1])
			if convErr != nil {
				continue
			}
			deviceIndex, convErr := strconv.Atoi(match[2])
			if convErr != nil {
				continue
			}
			if byIndex[cardIndex] == nil {
				byIndex[cardIndex] = &Card{Index: cardIndex, PlaybackDevices: []int{}}
			}
			byIndex[cardIndex].PlaybackDevices = append(byIndex[cardIndex].PlaybackDevices, deviceIndex)
		}
	}
	cards := make([]Card, 0, len(byIndex))
	for _, card := range byIndex {
		sort.Ints(card.PlaybackDevices)
		card.HardwareID = fmt.Sprintf("hw:%d", card.Index)
		cards = append(cards, *card)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Index < cards[j].Index })
	return cards, nil
}

// probeAccess opens a device node read/write and classifies the outcome.
func probeAccess(path string) AccessState {
	handle, err := os.OpenFile(path, os.O_RDWR, 0)
	if err == nil {
		handle.Close()
		return AccessOK
	}
	switch {
	case errors.Is(err, syscall.EBUSY):
		return AccessBusy
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM), os.IsPermission(err):
		return AccessPermissionDenied
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOENT):
		return AccessMissing
	default:
		return AccessError
	}
}

// Probe inspects the host sound card and returns a report. The returned error
// is non-nil only when the card is unusable, and its message is written to be
// shown verbatim to the operator in `docker logs`.
func Probe(ctx context.Context, cfg config.Config, mixer *Mixer, logger *slog.Logger) (Report, error) {
	if logger == nil {
		logger = slog.Default()
	}
	report := Report{
		SoundDirectory:  cfg.SoundDirectory,
		Card:            cfg.Card,
		PCMDevice:       cfg.PCMDevice,
		HardwareDevice:  cfg.ALSAHardwareDevice(),
		ControlPath:     cfg.ControlDevicePath(),
		PCMPlaybackPath: cfg.PCMPlaybackDevicePath(),
		Cards:           []Card{},
	}

	cards, err := EnumerateCards(cfg.SoundDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report.Reason = fmt.Sprintf("%s does not exist: this host has no sound card, so the local-output container does not apply. Do not enable the deploy/docker-compose.local-output.yml overlay on this machine.", cfg.SoundDirectory)
			return report, errors.New(report.Reason)
		}
		report.Reason = fmt.Sprintf("cannot read %s: %v", cfg.SoundDirectory, err)
		return report, errors.New(report.Reason)
	}
	report.Cards = cards
	report.Present = len(cards) > 0
	if !report.Present {
		report.Reason = fmt.Sprintf("%s exists but contains no ALSA card (no controlC*/pcmC*D*p node): this host has no usable sound card.", cfg.SoundDirectory)
		return report, errors.New(report.Reason)
	}

	selected := false
	for _, card := range cards {
		if card.Index == cfg.Card {
			selected = true
			break
		}
	}
	if !selected {
		indices := make([]string, 0, len(cards))
		for _, card := range cards {
			indices = append(indices, strconv.Itoa(card.Index))
		}
		report.Reason = fmt.Sprintf("card %d not found; %s exposes cards [%s]. Set ALSA_CARD to one of them.", cfg.Card, cfg.SoundDirectory, strings.Join(indices, ", "))
		return report, errors.New(report.Reason)
	}

	report.ControlAccess = probeAccess(report.ControlPath)
	report.PCMAccess = probeAccess(report.PCMPlaybackPath)

	if report.PCMAccess == AccessMissing {
		report.Reason = fmt.Sprintf("%s is missing: card %d has no playback device %d.", report.PCMPlaybackPath, cfg.Card, cfg.PCMDevice)
		return report, errors.New(report.Reason)
	}
	if report.PCMAccess == AccessPermissionDenied || report.ControlAccess == AccessPermissionDenied {
		report.Reason = fmt.Sprintf("cannot open %s / %s: permission denied. Pass the device with devices: [\"/dev/snd:/dev/snd\"] and keep the container running as root.", report.ControlPath, report.PCMPlaybackPath)
		return report, errors.New(report.Reason)
	}
	if report.PCMAccess == AccessError {
		report.Reason = fmt.Sprintf("cannot open %s: unknown ALSA error.", report.PCMPlaybackPath)
		return report, errors.New(report.Reason)
	}
	if report.PCMAccess == AccessBusy {
		logger.Warn("playback device is currently busy; continuing because ALSA has no dmix and a stale holder is not fatal",
			"device", report.HardwareDevice)
	}

	if mixer != nil {
		controls, controlErr := mixer.Controls(ctx)
		if controlErr != nil {
			logger.Warn("could not enumerate mixer controls through amixer", "error", controlErr)
		} else {
			report.ControlNames = controls
		}
	}
	return report, nil
}
