package mpd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMPD is a minimal in-process MPD server good enough for the shell's
// command set.
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
	go server.serve()
	t.Cleanup(func() { listener.Close() })
	return server
}

func (s *fakeMPD) addr() string { return s.listener.Addr().String() }

func (s *fakeMPD) serve() {
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(connection)
	}
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
		if line == "close" {
			return
		}
		s.mu.Lock()
		s.commands = append(s.commands, line)
		busy := s.busy
		s.mu.Unlock()

		if busy {
			fmt.Fprint(connection, "ACK [50@0] {add} Resource busy\n")
			continue
		}

		command := strings.Fields(line)
		switch command[0] {
		case "ping":
			fmt.Fprint(connection, "OK\n")
		case "status":
			s.mu.Lock()
			volume, state := s.volume, s.state
			s.mu.Unlock()
			fmt.Fprintf(connection, "volume: %d\nrepeat: 0\nrandom: 0\nsingle: 0\nconsume: 0\nstate: %s\nelapsed: 3.500\nduration: 210.000\nbitrate: 900\naudio: 44100:16:2\nOK\n", volume, state)
		case "currentsong":
			fmt.Fprint(connection, "file: http://127.0.0.1:8080/stream/1\nTitle: Test Track\nArtist: Test Artist\nAlbum: Test Album\nduration: 210.000\nOK\n")
		case "clear", "play", "pause", "stop", "next", "previous", "seekcur", "repeat", "random", "single", "consume", "add":
			if command[0] == "play" {
				s.mu.Lock()
				s.state = "play"
				s.mu.Unlock()
			}
			if command[0] == "pause" && len(command) > 1 && command[1] == "1" {
				s.mu.Lock()
				s.state = "pause"
				s.mu.Unlock()
			}
			if command[0] == "stop" {
				s.mu.Lock()
				s.state = "stop"
				s.mu.Unlock()
			}
			fmt.Fprint(connection, "OK\n")
		case "setvol":
			if len(command) > 1 {
				var volume int
				fmt.Sscanf(command[1], "%d", &volume)
				s.mu.Lock()
				s.volume = volume
				s.mu.Unlock()
			}
			fmt.Fprint(connection, "OK\n")
		default:
			fmt.Fprintf(connection, "ACK [5@0] {%s} unknown command\n", command[0])
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

func TestClientRoundTrip(t *testing.T) {
	server := newFakeMPD(t)
	client := NewClient(server.addr(), 3*time.Second)
	ctx := context.Background()

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if client.Version() != "0.23.5" {
		t.Fatalf("Version = %q", client.Version())
	}
	status, err := client.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status["state"] != "stop" || status["volume"] != "50" {
		t.Fatalf("status = %v", status)
	}
	song, err := client.CurrentSong(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if song["Title"] != "Test Track" || song["Artist"] != "Test Artist" {
		t.Fatalf("song = %v", song)
	}
	if err := client.SetVolume(ctx, 70); err != nil {
		t.Fatal(err)
	}
	status, err = client.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status["volume"] != "70" {
		t.Fatalf("volume after setvol = %q", status["volume"])
	}
	if err := client.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Add(ctx, "http://127.0.0.1:8080/stream/1"); err != nil {
		t.Fatal(err)
	}
	if err := client.Play(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = client.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status["state"] != "play" {
		t.Fatalf("state = %q", status["state"])
	}
	if err := client.SeekCur(ctx, 90*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestClientClampsVolume(t *testing.T) {
	server := newFakeMPD(t)
	client := NewClient(server.addr(), 3*time.Second)
	if err := client.SetVolume(context.Background(), 250); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, command := range server.recorded() {
		if command == "setvol 100" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a clamped setvol 100, got %v", server.recorded())
	}
}

func TestClientQuotesArgumentsWithSpaces(t *testing.T) {
	if got := quoteArgument("http://host/a b.mp3"); got != `"http://host/a b.mp3"` {
		t.Fatalf("quoteArgument = %s", got)
	}
	if got := quoteArgument("plain"); got != "plain" {
		t.Fatalf("quoteArgument = %s", got)
	}
}

func TestClientReportsUnavailable(t *testing.T) {
	// Port 1 is reserved and never listening in this test environment.
	client := NewClient("127.0.0.1:1", 500*time.Millisecond)
	err := client.Ping(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Ping error = %v, want ErrUnavailable", err)
	}
}

func TestClientClassifiesDeviceBusy(t *testing.T) {
	server := newFakeMPD(t)
	server.setBusy(true)
	client := NewClient(server.addr(), 3*time.Second)

	err := client.Add(context.Background(), "http://127.0.0.1:8080/stream/1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsDeviceBusy(err) {
		t.Fatalf("IsDeviceBusy(%v) = false", err)
	}
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("error is not a CommandError: %v", err)
	}
	if commandErr.ErrorCode != 50 || commandErr.Command != "add" {
		t.Fatalf("CommandError = %+v", commandErr)
	}
}

func TestParseAck(t *testing.T) {
	err := parseAck("ACK [56@0] {add} Permission denied")
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("parseAck did not return a CommandError: %v", err)
	}
	if commandErr.ErrorCode != 56 || commandErr.Command != "add" || commandErr.Message != "Permission denied" {
		t.Fatalf("CommandError = %+v", commandErr)
	}
	if !IsPermissionDenied(err) {
		t.Fatal("IsPermissionDenied = false")
	}
	if IsNotFound(err) {
		t.Fatal("IsNotFound must be false for a permission error")
	}
}
