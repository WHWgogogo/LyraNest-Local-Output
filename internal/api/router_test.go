package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lyranest/lyranest-local-output/internal/alsa"
	"github.com/lyranest/lyranest-local-output/internal/config"
	"github.com/lyranest/lyranest-local-output/internal/mpd"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeKernel struct {
	snapshot mpd.Snapshot
	client   *mpd.Client
}

func (f fakeKernel) Snapshot() mpd.Snapshot { return f.snapshot }
func (f fakeKernel) Client() *mpd.Client    { return f.client }

type fakeAmixer struct {
	mu       sync.Mutex
	autoMute string
	switches map[string]string
	percent  map[string]int
}

func newFakeAmixer() *fakeAmixer {
	return &fakeAmixer{
		autoMute: "Enabled",
		switches: map[string]string{"Master": "off", "Headphone": "off", "Speaker": "off"},
		percent:  map[string]int{"Master": 70, "Headphone": 65, "Speaker": 65},
	}
}

func (f *fakeAmixer) run(_ context.Context, name string, args ...string) (string, string, error) {
	if name != "amixer" {
		return "", "", fmt.Errorf("unexpected binary %q", name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) >= 2 && args[0] == "-c" {
		args = args[2:]
	}
	switch args[0] {
	case "scontrols":
		return "Simple mixer control 'Master',0\nSimple mixer control 'Headphone',0\nSimple mixer control 'Speaker',0\nSimple mixer control 'Auto-Mute Mode',0\n", "", nil
	case "sget":
		control := args[1]
		if control == "Auto-Mute Mode" {
			return fmt.Sprintf("Simple mixer control 'Auto-Mute Mode',0\n  Item0: '%s'\n", f.autoMute), "", nil
		}
		return fmt.Sprintf("Simple mixer control '%s',0\n  Front Left: Playback %d [%d%%] [0.00dB] [%s]\n", control, f.percent[control], f.percent[control], f.switches[control]), "", nil
	case "sset":
		control := args[1]
		if control == "Auto-Mute Mode" {
			f.autoMute = args[2]
			return "", "", nil
		}
		for _, value := range args[2:] {
			switch {
			case value == "unmute":
				f.switches[control] = "on"
			case value == "mute":
				f.switches[control] = "off"
			case strings.HasSuffix(value, "%"):
				var percent int
				fmt.Sscanf(strings.TrimSuffix(value, "%"), "%d", &percent)
				f.percent[control] = percent
			}
		}
		return "", "", nil
	}
	return "", "", errors.New("unsupported amixer command")
}

// fakeMPD is a tiny MPD server for HTTP-layer tests.
type fakeMPD struct {
	listener net.Listener

	mu       sync.Mutex
	commands []string
	volume   int
	state    string
	busy     bool
}

func newFakeMPD(t *testing.T) *fakeMPD {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeMPD{listener: listener, state: "stop", volume: 50}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go server.handle(connection)
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return server
}

func (s *fakeMPD) handle(connection net.Conn) {
	defer connection.Close()
	fmt.Fprint(connection, "OK MPD 0.23.5\n")
	reader := bufio.NewReader(connection)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		s.mu.Lock()
		s.commands = append(s.commands, line)
		busy := s.busy
		s.mu.Unlock()
		if busy {
			fmt.Fprint(connection, "ACK [50@0] {add} Resource busy\n")
			continue
		}
		fields := strings.Fields(line)
		switch fields[0] {
		case "ping":
			fmt.Fprint(connection, "OK\n")
		case "status":
			s.mu.Lock()
			volume, state := s.volume, s.state
			s.mu.Unlock()
			fmt.Fprintf(connection, "volume: %d\nstate: %s\nelapsed: 1.000\nduration: 100.000\naudio: 44100:16:2\nOK\n", volume, state)
		case "currentsong":
			fmt.Fprint(connection, "file: http://127.0.0.1:8080/stream/1\nTitle: T\nArtist: A\nOK\n")
		case "play":
			s.mu.Lock()
			s.state = "play"
			s.mu.Unlock()
			fmt.Fprint(connection, "OK\n")
		case "pause":
			s.mu.Lock()
			if len(fields) > 1 && fields[1] == "1" {
				s.state = "pause"
			} else {
				s.state = "play"
			}
			s.mu.Unlock()
			fmt.Fprint(connection, "OK\n")
		case "stop":
			s.mu.Lock()
			s.state = "stop"
			s.mu.Unlock()
			fmt.Fprint(connection, "OK\n")
		case "setvol":
			var volume int
			fmt.Sscanf(fields[1], "%d", &volume)
			s.mu.Lock()
			s.volume = volume
			s.mu.Unlock()
			fmt.Fprint(connection, "OK\n")
		case "clear", "add", "next", "previous", "seekcur", "repeat", "random", "single", "consume":
			fmt.Fprint(connection, "OK\n")
		default:
			fmt.Fprintf(connection, "ACK [5@0] {%s} unknown command\n", fields[0])
		}
	}
}

func (s *fakeMPD) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.commands...)
}

func (s *fakeMPD) setBusy(busy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = busy
}

func (s *fakeMPD) setVolume(volume int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volume = volume
}

func (s *fakeMPD) setState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

func newTestServer(t *testing.T, token string) (*Server, *fakeMPD, *fakeAmixer) {
	t.Helper()
	mpdServer := newFakeMPD(t)
	amixer := newFakeAmixer()

	cfg := config.Default()
	cfg.Token = token
	cfg.TestToneSeconds = 1
	cfg.TestToneTTL = time.Minute

	mixer := alsa.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).WithRunner(amixer.run)
	if _, err := mixer.Init(context.Background()); err != nil {
		t.Fatalf("mixer init: %v", err)
	}

	report := alsa.Report{
		SoundDirectory:  "/dev/snd",
		Present:         true,
		Cards:           []alsa.Card{{Index: 0, HardwareID: "hw:0", PlaybackDevices: []int{0}}},
		Card:            0,
		PCMDevice:       0,
		HardwareDevice:  "hw:0,0",
		ControlAccess:   alsa.AccessOK,
		PCMAccess:       alsa.AccessOK,
		ControlNames:    []string{"Master", "Headphone", "Speaker", "Auto-Mute Mode"},
	}

	server := New(Options{
		Config:  cfg,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Report:  report,
		Mixer:   mixer,
		Manager: fakeKernel{snapshot: mpd.Snapshot{Running: true, PID: 4242}, client: mpd.NewClient(mpdServer.listener.Addr().String(), 3*time.Second)},
		Tone:    NewToneProvider(cfg),
	})
	return server, mpdServer, amixer
}

func doRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, recorder.Body.String())
	}
	return payload
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestHealthzReady(t *testing.T) {
	server, _, _ := newTestServer(t, "")
	recorder := doRequest(t, server.Routes(), http.MethodGet, "/healthz", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	payload := decodeJSON(t, recorder)
	if payload["status"] != "ok" || payload["ready"] != true {
		t.Fatalf("payload = %v", payload)
	}
	if payload["auto_mute_fixed"] != true {
		t.Fatalf("auto_mute_fixed must be exposed at the top level: %v", payload)
	}
	mixerPayload, ok := payload["mixer"].(map[string]any)
	if !ok || mixerPayload["auto_mute_state"] != "Disabled" {
		t.Fatalf("mixer payload = %v", payload["mixer"])
	}
}

func TestHealthzDegradedWithoutSoundCard(t *testing.T) {
	server, _, _ := newTestServer(t, "")
	server.SetReport(alsa.Report{Present: false, Reason: "no card"})

	recorder := doRequest(t, server.Routes(), http.MethodGet, "/healthz", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
	payload := decodeJSON(t, recorder)
	if payload["status"] != "unhealthy" {
		t.Fatalf("status = %v", payload["status"])
	}
	if payload["ready"] != false {
		t.Fatal("ready must be false")
	}
}

func TestTokenGuard(t *testing.T) {
	server, _, _ := newTestServer(t, "s3cret")
	handler := server.Routes()

	recorder := doRequest(t, handler, http.MethodGet, "/api/status", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d", recorder.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.Header.Set("x-local-output-token", "s3cret")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK {
		t.Fatalf("status with token = %d (%s)", authorized.Code, authorized.Body.String())
	}

	// Health must stay unauthenticated for the container healthcheck.
	health := doRequest(t, handler, http.MethodGet, "/healthz", "")
	if health.Code != http.StatusOK {
		t.Fatalf("healthz = %d", health.Code)
	}
}

func TestPlayReplacesCurrentTrack(t *testing.T) {
	server, kernel, _ := newTestServer(t, "")
	recorder := doRequest(t, server.Routes(), http.MethodPost, "/api/play",
		`{"url":"http://127.0.0.1:8080/stream/42","title":"Song","artist":"Artist"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	commands := kernel.recorded()
	clearIndex, addIndex, playIndex := -1, -1, -1
	for index, command := range commands {
		switch {
		case command == "clear":
			clearIndex = index
		case strings.HasPrefix(command, "add "):
			addIndex = index
		case command == "play":
			playIndex = index
		}
	}
	if clearIndex < 0 || addIndex < 0 || playIndex < 0 {
		t.Fatalf("expected clear/add/play, got %v", commands)
	}
	if !(clearIndex < addIndex && addIndex < playIndex) {
		t.Fatalf("replace semantics must clear then add then play: %v", commands)
	}
	if commands[addIndex] != "add http://127.0.0.1:8080/stream/42" {
		t.Fatalf("add command = %q", commands[addIndex])
	}
}

func TestPlayRewritesLibraryPathForTheKernel(t *testing.T) {
	server, kernel, _ := newTestServer(t, "")
	recorder := doRequest(t, server.Routes(), http.MethodPost, "/api/play",
		`{"url":"file:///music/%E5%AE%89%E9%9D%9C%20-%20%E5%91%A8%E6%9D%B0%E5%80%AB.flac"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	// MPD denies absolute local paths, so the queue must receive the
	// music_directory-relative form (quoted because the name contains spaces).
	if !containsCommand(kernel.recorded(), `add "安靜 - 周杰倫.flac"`) {
		t.Fatalf("kernel commands = %v", kernel.recorded())
	}
}

func TestPlayRejectsBadURL(t *testing.T) {
	server, _, _ := newTestServer(t, "")
	handler := server.Routes()
	for _, body := range []string{`{"url":""}`, `{"url":"ftp://host/x"}`, `{"url":"http:///nohost"}`, `{}`} {
		recorder := doRequest(t, handler, http.MethodPost, "/api/play", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s => status %d", body, recorder.Code)
		}
	}
}

func TestPlayReportsDeviceBusyAsConflict(t *testing.T) {
	server, kernel, _ := newTestServer(t, "")
	kernel.setBusy(true)
	recorder := doRequest(t, server.Routes(), http.MethodPost, "/api/play", `{"url":"http://127.0.0.1:8080/stream/1"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	payload := decodeJSON(t, recorder)
	if payload["code"] != "device_busy" {
		t.Fatalf("code = %v", payload["code"])
	}
}

func TestControlActions(t *testing.T) {
	server, kernel, _ := newTestServer(t, "")
	handler := server.Routes()

	for _, action := range []string{"pause", "resume", "stop", "next", "prev", "play", "toggle"} {
		recorder := doRequest(t, handler, http.MethodPost, "/api/control", fmt.Sprintf(`{"action":%q}`, action))
		if recorder.Code != http.StatusOK {
			t.Fatalf("action %s => %d (%s)", action, recorder.Code, recorder.Body.String())
		}
	}
	recorder := doRequest(t, handler, http.MethodPost, "/api/control", `{"action":"seek","position":30}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("seek => %d (%s)", recorder.Code, recorder.Body.String())
	}
	found := false
	for _, command := range kernel.recorded() {
		if command == "seekcur 30.000" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected seekcur 30.000, got %v", kernel.recorded())
	}

	recorder = doRequest(t, handler, http.MethodPost, "/api/control", `{"action":"fly-to-moon"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown action => %d", recorder.Code)
	}
}

func TestVolumeAndMute(t *testing.T) {
	server, kernel, _ := newTestServer(t, "")
	handler := server.Routes()

	recorder := doRequest(t, handler, http.MethodPost, "/api/volume", `{"volume":64}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("volume => %d (%s)", recorder.Code, recorder.Body.String())
	}
	if !containsCommand(kernel.recorded(), "setvol 64") {
		t.Fatalf("commands = %v", kernel.recorded())
	}

	kernel.setVolume(64)
	if recorder := doRequest(t, handler, http.MethodPost, "/api/volume", `{"mute":true}`); recorder.Code != http.StatusOK {
		t.Fatalf("mute => %d (%s)", recorder.Code, recorder.Body.String())
	}
	if !containsCommand(kernel.recorded(), "setvol 0") {
		t.Fatalf("commands = %v", kernel.recorded())
	}

	if recorder := doRequest(t, handler, http.MethodPost, "/api/volume", `{"mute":false}`); recorder.Code != http.StatusOK {
		t.Fatalf("unmute => %d (%s)", recorder.Code, recorder.Body.String())
	}
	if !containsCommand(kernel.recorded(), "setvol 64") {
		t.Fatalf("unmute must restore the previous level: %v", kernel.recorded())
	}

	if recorder := doRequest(t, handler, http.MethodPost, "/api/volume", `{"volume":300}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range volume => %d", recorder.Code)
	}
	if recorder := doRequest(t, handler, http.MethodPost, "/api/volume", `{}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty volume request => %d", recorder.Code)
	}
}

func TestTestPlayPublishesToneAndPlaysIt(t *testing.T) {
	server, kernel, _ := newTestServer(t, "")
	handler := server.Routes()

	recorder := doRequest(t, handler, http.MethodPost, "/api/test-play", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("test-play => %d (%s)", recorder.Code, recorder.Body.String())
	}
	payload := decodeJSON(t, recorder)
	toneURL, _ := payload["url"].(string)
	if !strings.HasPrefix(toneURL, "http://127.0.0.1:8091/tone/") || !strings.HasSuffix(toneURL, "/tone.wav") {
		t.Fatalf("tone url = %q", toneURL)
	}
	if !containsCommand(kernel.recorded(), "add "+toneURL) {
		t.Fatalf("kernel commands = %v", kernel.recorded())
	}

	// The tone must actually be fetchable at the published URL.
	token := strings.TrimSuffix(strings.TrimPrefix(toneURL, "http://127.0.0.1:8091/tone/"), "/tone.wav")
	toneRecorder := doRequest(t, handler, http.MethodGet, "/tone/"+token+"/tone.wav", "")
	if toneRecorder.Code != http.StatusOK {
		t.Fatalf("tone fetch => %d", toneRecorder.Code)
	}
	audio := toneRecorder.Body.Bytes()
	if len(audio) < 44 || string(audio[0:4]) != "RIFF" || string(audio[8:12]) != "WAVE" {
		t.Fatalf("tone is not a WAV file: % x", audio[:min(16, len(audio))])
	}
	if contentType := toneRecorder.Header().Get("Content-Type"); contentType != "audio/wav" {
		t.Fatalf("content type = %q", contentType)
	}
	if recorder := doRequest(t, handler, http.MethodGet, "/tone/deadbeef/tone.wav", ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown tone token => %d", recorder.Code)
	}
}

func TestStatusReportsPlaybackAndMixer(t *testing.T) {
	server, kernel, _ := newTestServer(t, "")
	kernel.setState("play")
	recorder := doRequest(t, server.Routes(), http.MethodGet, "/api/status", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	payload := decodeJSON(t, recorder)
	playback, ok := payload["playback"].(map[string]any)
	if !ok {
		t.Fatalf("playback payload = %v", payload["playback"])
	}
	if playback["state"] != "play" || playback["title"] != "T" {
		t.Fatalf("playback = %v", playback)
	}
	if payload["auto_mute_fixed"] != true {
		t.Fatalf("auto_mute_fixed = %v", payload["auto_mute_fixed"])
	}
}

func TestDevicesListsCards(t *testing.T) {
	server, _, _ := newTestServer(t, "")
	recorder := doRequest(t, server.Routes(), http.MethodGet, "/api/devices", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("devices = %d", recorder.Code)
	}
	payload := decodeJSON(t, recorder)
	if payload["exclusive"] != true {
		t.Fatal("devices must advertise the exclusive ALSA device")
	}
}

func TestToneIsAClickFreeSine(t *testing.T) {
	cfg := config.Default()
	cfg.TestToneSeconds = 1
	cfg.TestToneFrequencyHz = 440
	provider := NewToneProvider(cfg)
	audio, err := provider.wav()
	if err != nil {
		t.Fatal(err)
	}
	if len(audio) != 44+44100*2*2 {
		t.Fatalf("wav size = %d", len(audio))
	}
	if _, ok := provider.Lookup("nope"); ok {
		t.Fatal("Lookup must reject an unknown token before Enable")
	}
	if _, err := provider.Enable(); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePlayURI(t *testing.T) {
	const musicDirectory = "/music"
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"http stream passes through", "http://host:8080/stream/x.mp3?token=abc", "http://host:8080/stream/x.mp3?token=abc"},
		{"https stream passes through", "https://host/x", "https://host/x"},
		{"file url is rewritten to the music directory", "file:///music/Album/song.flac", "Album/song.flac"},
		{"file url with encoded spaces", "file:///music/Album/a%20b.flac", "Album/a b.flac"},
		{"absolute path is rewritten", "/music/Album/song.flac", "Album/song.flac"},
		{"relative path is used as-is", "Album/song.flac", "Album/song.flac"},
		{"relative path with spaces", "Album/a b.flac", "Album/a b.flac"},
		{"relative traversal is neutralised", "Album/../song.flac", "song.flac"},
		{"leading traversal is confined to the music directory", "../secret.flac", "secret.flac"},
	}
	for _, testCase := range cases {
		got, err := resolvePlayURI(testCase.raw, musicDirectory)
		if err != nil {
			t.Fatalf("%s: resolvePlayURI(%q) = %v", testCase.name, testCase.raw, err)
		}
		if got != testCase.want {
			t.Fatalf("%s: resolvePlayURI(%q) = %q, want %q", testCase.name, testCase.raw, got, testCase.want)
		}
	}

	invalid := []string{"", "ftp://host/x", "http://", "file://relative", "file:///etc/passwd", "/etc/passwd", "/music"}
	for _, raw := range invalid {
		if got, err := resolvePlayURI(raw, musicDirectory); err == nil {
			t.Fatalf("resolvePlayURI(%q) must fail, got %q", raw, got)
		}
	}
}

func TestRedactURLStripsSecrets(t *testing.T) {
	redacted := redactURL("http://user:pass@host/stream?token=abc")
	if strings.Contains(redacted, "pass") || strings.Contains(redacted, "abc") {
		t.Fatalf("redactURL leaked credentials: %s", redacted)
	}
}

func containsCommand(commands []string, want string) bool {
	for _, command := range commands {
		if command == want {
			return true
		}
	}
	return false
}
