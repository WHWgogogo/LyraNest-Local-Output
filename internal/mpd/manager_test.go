package mpd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lyranest/lyranest-local-output/internal/config"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.MPDTemplatePath = filepath.Join(root, "mpd.conf.template")
	cfg.MPDConfPath = filepath.Join(root, "etc", "mpd.conf")
	cfg.MPDDataDirectory = filepath.Join(root, "var", "lib", "mpd")
	cfg.MPDMusicDirectory = filepath.Join(root, "music")
	cfg.MPDStartTimeout = 500 * time.Millisecond
	return cfg
}

func TestRenderConfigUsesVerifiedAudioOutput(t *testing.T) {
	cfg := testConfig(t)
	template := `music_directory "{{.MusicDirectory}}"
port "{{.Port}}"
audio_output {
    device "{{.Device}}"
{{if .MixerControl}}    mixer_type "hardware"
    mixer_control "{{.MixerControl}}"
{{else}}    mixer_type "software"
{{end}}}
`
	if err := os.WriteFile(cfg.MPDTemplatePath, []byte(template), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, nil)
	if err := manager.LoadTemplate(); err != nil {
		t.Fatal(err)
	}
	rendered, err := manager.RenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`device "hw:0,0"`, `mixer_type "hardware"`, `mixer_control "Headphone"`} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("rendered config missing %q:\n%s", expected, rendered)
		}
	}
}

func TestRenderConfigFallsBackToSoftwareMixer(t *testing.T) {
	cfg := testConfig(t)
	if err := os.WriteFile(cfg.MPDTemplatePath, []byte("{{if .MixerControl}}hardware:{{.MixerControl}}{{else}}software{{end}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, nil)
	if err := manager.LoadTemplate(); err != nil {
		t.Fatal(err)
	}
	manager.SetMixerControl("")
	if manager.MixerMode() != "software" {
		t.Fatalf("MixerMode = %q", manager.MixerMode())
	}
	rendered, err := manager.RenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	if rendered != "software" {
		t.Fatalf("rendered = %q", rendered)
	}
}

func TestLoadTemplateUsesBuiltInFallback(t *testing.T) {
	cfg := testConfig(t)
	manager := NewManager(cfg, nil)
	if err := manager.LoadTemplate(); err != nil {
		t.Fatalf("LoadTemplate without a file: %v", err)
	}
	rendered, err := manager.RenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, `device          "hw:0,0"`) {
		t.Fatalf("fallback template missing the ALSA device:\n%s", rendered)
	}
	if !strings.Contains(rendered, `auto_resample   "no"`) {
		t.Fatalf("fallback template must keep bit-perfect output:\n%s", rendered)
	}
}

func TestWriteConfigAndEnsureDirectories(t *testing.T) {
	cfg := testConfig(t)
	manager := NewManager(cfg, nil)
	if err := manager.LoadTemplate(); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureDirectories(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.WriteConfig(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.MPDConfPath); err != nil {
		t.Fatalf("config not written: %v", err)
	}
	for _, directory := range []string{cfg.MPDMusicDirectory, cfg.MPDDataDirectory, filepath.Join(cfg.MPDDataDirectory, "playlists")} {
		if info, err := os.Stat(directory); err != nil || !info.IsDir() {
			t.Fatalf("directory %s missing: %v", directory, err)
		}
	}
}

func TestStartReportsMissingBinary(t *testing.T) {
	cfg := testConfig(t)
	cfg.MPDBinary = filepath.Join(t.TempDir(), "does-not-exist")
	manager := NewManager(cfg, nil)
	if err := manager.LoadTemplate(); err != nil {
		t.Fatal(err)
	}
	err := manager.Start(context.Background())
	if err == nil {
		t.Fatal("Start must fail when the MPD binary is missing")
	}
	if snapshot := manager.Snapshot(); snapshot.Running {
		t.Fatalf("snapshot reports a running kernel: %+v", snapshot)
	}
}

func TestSnapshotDefaults(t *testing.T) {
	cfg := testConfig(t)
	manager := NewManager(cfg, nil)
	snapshot := manager.Snapshot()
	if snapshot.Running {
		t.Fatal("a fresh manager must not report a running kernel")
	}
	if snapshot.MixerMode != "hardware" {
		t.Fatalf("MixerMode = %q", snapshot.MixerMode)
	}
	if snapshot.ConfigPath != cfg.MPDConfPath {
		t.Fatalf("ConfigPath = %q", snapshot.ConfigPath)
	}
}
