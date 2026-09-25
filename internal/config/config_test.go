package config

import "testing"

func TestDefaultMatchesVerifiedHardware(t *testing.T) {
	cfg := Default()
	if cfg.Port != 8091 {
		t.Fatalf("default port = %d, want 8091", cfg.Port)
	}
	if cfg.Card != 0 || cfg.PCMDevice != 0 {
		t.Fatalf("default card/pcm = %d/%d, want 0/0", cfg.Card, cfg.PCMDevice)
	}
	if got := cfg.ALSAHardwareDevice(); got != "hw:0,0" {
		t.Fatalf("ALSAHardwareDevice() = %q, want hw:0,0", got)
	}
	if got := cfg.ALSAHardwareCard(); got != "hw:0" {
		t.Fatalf("ALSAHardwareCard() = %q, want hw:0", got)
	}
	if cfg.AutoMuteControl != "Auto-Mute Mode" {
		t.Fatalf("AutoMuteControl = %q, want Auto-Mute Mode", cfg.AutoMuteControl)
	}
	if !cfg.FixAutoMute {
		t.Fatal("FixAutoMute must default to true: without it the 3.5mm jack stays muted")
	}
	if cfg.MPDMixerControl != "Headphone" {
		t.Fatalf("MPDMixerControl = %q, want Headphone", cfg.MPDMixerControl)
	}
	if cfg.SoundDirectory != "/dev/snd" {
		t.Fatalf("SoundDirectory = %q, want /dev/snd", cfg.SoundDirectory)
	}
	if got := cfg.ControlDevicePath(); got != "/dev/snd/controlC0" {
		t.Fatalf("ControlDevicePath() = %q", got)
	}
	if got := cfg.PCMPlaybackDevicePath(); got != "/dev/snd/pcmC0D0p" {
		t.Fatalf("PCMPlaybackDevicePath() = %q", got)
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("LOCAL_OUTPUT_PORT", "9001")
	t.Setenv("ALSA_CARD", "2")
	t.Setenv("ALSA_PCM_DEVICE", "3")
	t.Setenv("MPD_MIXER_CONTROL", "Master")
	t.Setenv("ALSA_FIX_AUTO_MUTE", "false")
	t.Setenv("LOCAL_OUTPUT_TEST_TONE_HZ", "1000")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9001 {
		t.Fatalf("Port = %d", cfg.Port)
	}
	if cfg.Card != 2 || cfg.PCMDevice != 3 {
		t.Fatalf("card/pcm = %d/%d", cfg.Card, cfg.PCMDevice)
	}
	if got := cfg.ALSAHardwareDevice(); got != "hw:2,3" {
		t.Fatalf("ALSAHardwareDevice() = %q", got)
	}
	if cfg.MPDMixerControl != "Master" {
		t.Fatalf("MPDMixerControl = %q", cfg.MPDMixerControl)
	}
	if cfg.FixAutoMute {
		t.Fatal("ALSA_FIX_AUTO_MUTE=false must disable the fix")
	}
	if cfg.TestToneFrequencyHz != 1000 {
		t.Fatalf("TestToneFrequencyHz = %d", cfg.TestToneFrequencyHz)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Setenv("LOCAL_OUTPUT_PORT", "70000")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for an out-of-range port")
	}
}
