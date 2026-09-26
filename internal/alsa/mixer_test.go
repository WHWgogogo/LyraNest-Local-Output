package alsa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/lyranest/lyranest-local-output/internal/config"
)

func TestParseSimpleControls(t *testing.T) {
	output := `Simple mixer control 'Master',0
Simple mixer control 'Headphone',0
Simple mixer control 'Auto-Mute Mode',0
`
	controls := ParseSimpleControls(output)
	want := []string{"Master", "Headphone", "Auto-Mute Mode"}
	if len(controls) != len(want) {
		t.Fatalf("controls = %v, want %v", controls, want)
	}
	for index := range want {
		if controls[index] != want[index] {
			t.Fatalf("controls[%d] = %q, want %q", index, controls[index], want[index])
		}
	}
}

func TestParseItemValue(t *testing.T) {
	output := `Simple mixer control 'Auto-Mute Mode',0
  Capabilities: enum
  Items: 'Disabled' 'Enabled'
  Item0: 'Disabled'
`
	value, ok := ParseItemValue(output, 0)
	if !ok || value != "Disabled" {
		t.Fatalf("ParseItemValue = %q, %v", value, ok)
	}
	if _, ok := ParseItemValue(output, 3); ok {
		t.Fatal("expected Item3 to be absent")
	}
}

func TestParsePercentAndSwitchState(t *testing.T) {
	output := `Simple mixer control 'Headphone',0
  Limits: Playback 0 - 87
  Front Left: Playback 65 [75%] [-16.50dB] [on]
  Front Right: Playback 65 [75%] [-16.50dB] [on]
`
	percent, ok := ParsePercent(output)
	if !ok || percent != 75 {
		t.Fatalf("ParsePercent = %d, %v", percent, ok)
	}
	muted, ok := ParseSwitchState(output)
	if !ok || muted {
		t.Fatalf("ParseSwitchState = %v, %v; want unmuted", muted, ok)
	}

	mutedOutput := `  Mono: Playback 0 [0%] [-65.25dB] [off]`
	muted, ok = ParseSwitchState(mutedOutput)
	if !ok || !muted {
		t.Fatalf("ParseSwitchState = %v, %v; want muted", muted, ok)
	}
}

// fakeAmixer emulates the subset of alsa-utils the shell uses.
type fakeAmixer struct {
	mu       sync.Mutex
	autoMute string
	controls []string
	switches map[string]string
	percent  map[string]int
	failSet  map[string]error
	calls    []string
}

func newFakeAmixer() *fakeAmixer {
	return &fakeAmixer{
		autoMute: "Enabled",
		controls: []string{"Master", "Headphone", "Speaker", "Auto-Mute Mode"},
		switches: map[string]string{"Master": "off", "Headphone": "off", "Speaker": "off"},
		percent:  map[string]int{"Master": 70, "Headphone": 65, "Speaker": 65},
		failSet:  map[string]error{},
	}
}

func (f *fakeAmixer) run(_ context.Context, name string, args ...string) (string, string, error) {
	if name != "amixer" {
		return "", "", fmt.Errorf("unexpected binary %q", name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Drop the "-c <card>" prefix.
	if len(args) >= 2 && args[0] == "-c" {
		args = args[2:]
	}
	if len(args) == 0 {
		return "", "", errors.New("no arguments")
	}
	f.calls = append(f.calls, strings.Join(args, " "))

	switch args[0] {
	case "scontrols":
		var builder strings.Builder
		for _, control := range f.controls {
			fmt.Fprintf(&builder, "Simple mixer control '%s',0\n", control)
		}
		return builder.String(), "", nil
	case "sget":
		control := args[1]
		switch control {
		case "Auto-Mute Mode":
			return fmt.Sprintf("Simple mixer control 'Auto-Mute Mode',0\n  Items: 'Disabled' 'Enabled'\n  Item0: '%s'\n", f.autoMute), "", nil
		default:
			return fmt.Sprintf("Simple mixer control '%s',0\n  Front Left: Playback %d [%d%%] [0.00dB] [%s]\n", control, f.percent[control], f.percent[control], f.switches[control]), "", nil
		}
	case "sset":
		control := args[1]
		if err, ok := f.failSet[control]; ok {
			return "", "amixer: error", err
		}
		switch control {
		case "Auto-Mute Mode":
			if len(args) > 2 {
				f.autoMute = args[2]
			}
		default:
			for _, value := range args[2:] {
				if value == "unmute" {
					f.switches[control] = "on"
				}
				if value == "mute" {
					f.switches[control] = "off"
				}
				if strings.HasSuffix(value, "%") {
					var percent int
					fmt.Sscanf(strings.TrimSuffix(value, "%"), "%d", &percent)
					f.percent[control] = percent
				}
			}
		}
		return "", "", nil
	default:
		return "", "", fmt.Errorf("unsupported amixer command %q", args[0])
	}
}

func testConfig() config.Config {
	cfg := config.Default()
	return cfg
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestInitDisablesAutoMuteAndUnmutes(t *testing.T) {
	fake := newFakeAmixer()
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)

	state, err := mixer.Init(context.Background())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !state.AutoMuteFixed {
		t.Fatal("AutoMuteFixed must be true after a successful Init")
	}
	if state.AutoMuteState != "Disabled" {
		t.Fatalf("AutoMuteState = %q", state.AutoMuteState)
	}
	if !state.Initialized || !state.Verified {
		t.Fatalf("state = %+v", state)
	}
	if state.EffectiveControl != "Headphone" {
		t.Fatalf("EffectiveControl = %q, want Headphone", state.EffectiveControl)
	}
	if len(state.Unmuted) != 3 {
		t.Fatalf("Unmuted = %v, want Master/Headphone/Speaker", state.Unmuted)
	}
	if fake.autoMute != "Disabled" {
		t.Fatalf("fake autoMute = %q", fake.autoMute)
	}
	if fake.switches["Headphone"] != "on" {
		t.Fatal("Headphone must be unmuted")
	}

	// The Auto-Mute fix must be the first write, before any unmute.
	if len(fake.calls) < 2 || !strings.HasPrefix(fake.calls[1], "sset Auto-Mute Mode Disabled") {
		t.Fatalf("first mixer write = %v, want the Auto-Mute fix", fake.calls)
	}
}

func TestInitFailsWhenAutoMuteCannotBeDisabled(t *testing.T) {
	fake := newFakeAmixer()
	fake.failSet["Auto-Mute Mode"] = errors.New("amixer: Unable to find simple control")
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)

	_, err := mixer.Init(context.Background())
	if err == nil {
		t.Fatal("Init must fail when the Auto-Mute fix cannot be applied: otherwise the container is green but silent")
	}
}

func TestInitFailsWhenAutoMuteReadBackStaysEnabled(t *testing.T) {
	fake := newFakeAmixer()
	// Simulate a card that accepts the write but keeps the old value.
	mixer := New(testConfig(), testLogger()).WithRunner(func(ctx context.Context, name string, args ...string) (string, string, error) {
		stdout, stderr, err := fake.run(ctx, name, args...)
		if len(args) >= 4 && args[0] == "-c" && args[2] == "sget" && args[3] == "Auto-Mute Mode" {
			return "Simple mixer control 'Auto-Mute Mode',0\n  Item0: 'Enabled'\n", stderr, err
		}
		return stdout, stderr, err
	})

	_, err := mixer.Init(context.Background())
	if err == nil {
		t.Fatal("Init must fail when Auto-Mute Mode still reads Enabled")
	}
}

func TestInitToleratesCardsWithoutAutoMute(t *testing.T) {
	fake := newFakeAmixer()
	fake.controls = []string{"Master", "Speaker"}
	fake.switches["Headphone"] = "on"
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)

	state, err := mixer.Init(context.Background())
	if err != nil {
		t.Fatalf("Init on a card without Auto-Mute Mode: %v", err)
	}
	if state.AutoMuteFixed {
		t.Fatal("AutoMuteFixed must stay false when the card has no such control")
	}
	if state.AutoMuteState != "unsupported" {
		t.Fatalf("AutoMuteState = %q, want unsupported", state.AutoMuteState)
	}
	if state.EffectiveControl != "Master" {
		t.Fatalf("EffectiveControl = %q, want the Master fallback", state.EffectiveControl)
	}
}

func TestSetVolumeAndRefresh(t *testing.T) {
	fake := newFakeAmixer()
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)
	if _, err := mixer.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	state, err := mixer.SetVolume(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if state.VolumePercent != 42 {
		t.Fatalf("VolumePercent = %d", state.VolumePercent)
	}

	fake.mu.Lock()
	fake.percent["Headphone"] = 55
	fake.mu.Unlock()
	refreshed, err := mixer.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.VolumePercent != 55 {
		t.Fatalf("refreshed VolumePercent = %d, want 55", refreshed.VolumePercent)
	}
}

func TestInitSupportsPCMControlOnly(t *testing.T) {
	fake := newFakeAmixer()
	fake.controls = []string{"PCM"}
	fake.switches = map[string]string{"PCM": "on"}
	fake.percent = map[string]int{"PCM": 80}
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)

	state, err := mixer.Init(context.Background())
	if err != nil {
		t.Fatalf("Init on an ARM card with PCM only: %v", err)
	}
	if state.EffectiveControl != "PCM" {
		t.Fatalf("EffectiveControl = %q, want PCM", state.EffectiveControl)
	}
	if !state.Verified {
		t.Fatal("state.Verified must be true for PCM")
	}
}

func TestInitToleratesCardsWithoutAnyHardwareMixer(t *testing.T) {
	fake := newFakeAmixer()
	fake.controls = []string{}
	fake.switches = map[string]string{}
	fake.percent = map[string]int{}
	mixer := New(testConfig(), testLogger()).WithRunner(fake.run)

	state, err := mixer.Init(context.Background())
	if err != nil {
		t.Fatalf("Init on a card without hardware mixer: %v", err)
	}
	if state.EffectiveControl != "" {
		t.Fatalf("EffectiveControl = %q, want empty (software volume fallback)", state.EffectiveControl)
	}
	if !state.Verified {
		t.Fatal("state.Verified must be true when falling back to software volume")
	}
}
