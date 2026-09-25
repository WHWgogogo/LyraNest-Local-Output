package alsa

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lyranest/lyranest-local-output/internal/config"
)

func TestEnumerateCardsUsesOnlyDevSnd(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"controlC0", "pcmC0D0c", "pcmC0D0p", "controlC2", "pcmC2D3p", "seq", "timer"} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cards, err := EnumerateCards(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("cards = %+v, want 2", cards)
	}
	if cards[0].Index != 0 || len(cards[0].PlaybackDevices) != 1 || cards[0].PlaybackDevices[0] != 0 {
		t.Fatalf("card 0 = %+v", cards[0])
	}
	if cards[1].Index != 2 || len(cards[1].PlaybackDevices) != 1 || cards[1].PlaybackDevices[0] != 3 {
		t.Fatalf("card 2 = %+v", cards[1])
	}
	if cards[1].HardwareID != "hw:2" {
		t.Fatalf("HardwareID = %q", cards[1].HardwareID)
	}
}

func TestProbeReportsMissingSoundDirectory(t *testing.T) {
	cfg := config.Default()
	cfg.SoundDirectory = filepath.Join(t.TempDir(), "not-there")

	report, err := Probe(context.Background(), cfg, nil, testLogger())
	if err == nil {
		t.Fatal("Probe must fail when /dev/snd is absent")
	}
	if report.Present {
		t.Fatal("Present must be false")
	}
	if report.Reason == "" {
		t.Fatal("Reason must explain the failure to the operator")
	}
}

func TestProbeReportsMissingCard(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"controlC0", "pcmC0D0p"} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.SoundDirectory = directory
	cfg.Card = 1

	report, err := Probe(context.Background(), cfg, nil, testLogger())
	if err == nil {
		t.Fatal("Probe must fail when the configured card does not exist")
	}
	if report.Reason == "" {
		t.Fatal("Reason must list the cards that do exist")
	}
}

func TestProbeSucceedsWithPlayableCard(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"controlC0", "pcmC0D0p"} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.SoundDirectory = directory

	fake := newFakeAmixer()
	mixer := New(cfg, testLogger()).WithRunner(fake.run)

	report, err := Probe(context.Background(), cfg, mixer, testLogger())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !report.Ready() {
		t.Fatalf("report is not ready: %+v", report)
	}
	if report.HardwareDevice != "hw:0,0" {
		t.Fatalf("HardwareDevice = %q", report.HardwareDevice)
	}
	if len(report.ControlNames) != 4 {
		t.Fatalf("ControlNames = %v, want the 4 fake controls", report.ControlNames)
	}
}

func TestProbeReportsMissingPlaybackDevice(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "controlC0"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.SoundDirectory = directory

	report, err := Probe(context.Background(), cfg, nil, testLogger())
	if err == nil {
		t.Fatal("Probe must fail when the card has no playback device")
	}
	if report.Ready() {
		t.Fatal("report must not be ready")
	}
}
