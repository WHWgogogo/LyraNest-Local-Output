// Package mpd manages the MPD playback kernel: its configuration, process
// lifecycle and the minimal protocol client the shell needs.
//
// MPD is GPL-2.0 third-party software. It runs as an independent process in an
// independent container; LyraNest neither links nor modifies it.
package mpd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable is returned when MPD cannot be reached at all.
var ErrUnavailable = errors.New("mpd is unavailable")

// CommandError is an MPD ACK response.
type CommandError struct {
	ErrorCode int
	Command   string
	Message   string
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("mpd error %d on %s: %s", e.ErrorCode, e.Command, e.Message)
}

// Pair is one `key: value` line of an MPD response, kept in order.
type Pair struct {
	Key   string
	Value string
}

// Client speaks the MPD protocol over the loopback control socket.
//
// Every command uses a short-lived connection. MPD handles that cheaply and it
// removes an entire class of stale-connection bugs; the shell is the only
// client and issues at most a few commands per user action.
type Client struct {
	addr    string
	timeout time.Duration

	mu      sync.Mutex
	version string
}

// NewClient creates a client for the given host:port.
func NewClient(addr string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{addr: addr, timeout: timeout}
}

// Address returns the configured MPD endpoint.
func (c *Client) Address() string { return c.addr }

// Version returns the protocol banner from the last successful connection.
func (c *Client) Version() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// command runs one MPD command and returns its response lines.
func (c *Client) command(ctx context.Context, args ...string) ([]Pair, error) {
	if len(args) == 0 {
		return nil, errors.New("mpd: empty command")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	dialer := net.Dialer{Timeout: c.timeout}
	connection, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %v", ErrUnavailable, c.addr, err)
	}
	defer connection.Close()

	deadline := time.Now().Add(c.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = connection.SetDeadline(deadline)

	reader := bufio.NewReader(connection)

	greeting, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("%w: read banner: %v", ErrUnavailable, err)
	}
	greeting = strings.TrimSpace(greeting)
	if !strings.HasPrefix(greeting, "OK MPD ") {
		return nil, fmt.Errorf("%w: unexpected banner %q", ErrUnavailable, greeting)
	}
	c.version = strings.TrimSpace(strings.TrimPrefix(greeting, "OK MPD "))

	// Quote arguments that contain whitespace so MPD parses them as one token.
	encoded := make([]string, 0, len(args))
	for _, arg := range args {
		encoded = append(encoded, quoteArgument(arg))
	}
	if _, err := fmt.Fprintf(connection, "%s\n", strings.Join(encoded, " ")); err != nil {
		return nil, fmt.Errorf("%w: write command: %v", ErrUnavailable, err)
	}

	pairs := []Pair{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "OK":
			return pairs, nil
		case strings.HasPrefix(line, "ACK "):
			return nil, parseAck(line)
		case line == "":
			continue
		default:
			key, value, found := strings.Cut(line, ": ")
			if !found {
				pairs = append(pairs, Pair{Key: line})
				continue
			}
			pairs = append(pairs, Pair{Key: key, Value: value})
		}
	}
}

func quoteArgument(arg string) string {
	if arg == "" {
		return `""`
	}
	if strings.ContainsAny(arg, " \t\"") {
		return `"` + strings.ReplaceAll(arg, `"`, `\"`) + `"`
	}
	return arg
}

func parseAck(line string) error {
	// ACK [50@0] {play} No song playing
	rest := strings.TrimPrefix(line, "ACK ")
	code := 0
	if open := strings.Index(rest, "["); open >= 0 {
		if close := strings.Index(rest[open:], "]"); close > 0 {
			token := rest[open+1 : open+close]
			if at := strings.Index(token, "@"); at > 0 {
				token = token[:at]
			}
			if parsed, err := strconv.Atoi(token); err == nil {
				code = parsed
			}
		}
	}
	command := ""
	if open := strings.Index(rest, "{"); open >= 0 {
		if close := strings.Index(rest[open:], "}"); close > 0 {
			command = rest[open+1 : open+close]
		}
	}
	message := line
	if open := strings.Index(rest, "}"); open >= 0 {
		message = strings.TrimSpace(rest[open+1:])
	}
	return &CommandError{ErrorCode: code, Command: command, Message: message}
}

// IsNotFound reports whether an error is MPD's "No such song/file" style ACK.
func IsNotFound(err error) bool {
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	lower := strings.ToLower(commandErr.Message)
	return commandErr.ErrorCode == 50 || strings.Contains(lower, "no such") || strings.Contains(lower, "not found")
}

// IsPermissionDenied reports whether MPD refused access to a resource.
func IsPermissionDenied(err error) bool {
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	lower := strings.ToLower(commandErr.Message)
	return commandErr.ErrorCode == 56 || strings.Contains(lower, "permission") || strings.Contains(lower, "denied") || strings.Contains(lower, "access")
}

// IsDeviceBusy reports whether ALSA refused to open the exclusive device.
func IsDeviceBusy(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "resource busy") || strings.Contains(lower, "device or resource busy") || strings.Contains(lower, "busy")
}

// Ping verifies the control socket answers.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.command(ctx, "ping")
	return err
}

// WaitReady polls the control socket until MPD answers or the context ends.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		lastErr = c.Ping(attemptCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mpd did not become ready within %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Status returns the MPD status map.
func (c *Client) Status(ctx context.Context) (map[string]string, error) {
	pairs, err := c.command(ctx, "status")
	if err != nil {
		return nil, err
	}
	return pairsToMap(pairs), nil
}

// CurrentSong returns the currently loaded song, or nil when the queue is idle.
func (c *Client) CurrentSong(ctx context.Context) (map[string]string, error) {
	pairs, err := c.command(ctx, "currentsong")
	if err != nil {
		return nil, err
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	return pairsToMap(pairs), nil
}

func pairsToMap(pairs []Pair) map[string]string {
	values := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		values[pair.Key] = pair.Value
	}
	return values
}

// Clear empties the queue.
func (c *Client) Clear(ctx context.Context) error {
	_, err := c.command(ctx, "clear")
	return err
}

// Add appends one URI (local path or http(s) stream) to the queue.
func (c *Client) Add(ctx context.Context, uri string) error {
	_, err := c.command(ctx, "add", uri)
	return err
}

// Play starts playback of the current queue position.
func (c *Client) Play(ctx context.Context) error {
	_, err := c.command(ctx, "play")
	return err
}

// Pause sets the pause flag.
func (c *Client) Pause(ctx context.Context, paused bool) error {
	value := "0"
	if paused {
		value = "1"
	}
	_, err := c.command(ctx, "pause", value)
	return err
}

// Stop stops playback and clears the "playing" state.
func (c *Client) Stop(ctx context.Context) error {
	_, err := c.command(ctx, "stop")
	return err
}

// Next advances to the next queue entry.
func (c *Client) Next(ctx context.Context) error {
	_, err := c.command(ctx, "next")
	return err
}

// Previous goes back one queue entry.
func (c *Client) Previous(ctx context.Context) error {
	_, err := c.command(ctx, "previous")
	return err
}

// SeekCur seeks within the current song.
func (c *Client) SeekCur(ctx context.Context, position time.Duration) error {
	seconds := position.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	_, err := c.command(ctx, "seekcur", strconv.FormatFloat(seconds, 'f', 3, 64))
	return err
}

// SetVolume sets the hardware mixer volume (0-100).
func (c *Client) SetVolume(ctx context.Context, volume int) error {
	if volume < 0 {
		volume = 0
	}
	if volume > 100 {
		volume = 100
	}
	_, err := c.command(ctx, "setvol", strconv.Itoa(volume))
	return err
}

// SetRepeat toggles queue repeat.
func (c *Client) SetRepeat(ctx context.Context, enabled bool) error {
	_, err := c.command(ctx, "repeat", boolFlag(enabled))
	return err
}

// SetRandom toggles shuffle.
func (c *Client) SetRandom(ctx context.Context, enabled bool) error {
	_, err := c.command(ctx, "random", boolFlag(enabled))
	return err
}

func boolFlag(enabled bool) string {
	if enabled {
		return "1"
	}
	return "0"
}

// SetSingle toggles single-song playback.
func (c *Client) SetSingle(ctx context.Context, enabled bool) error {
	_, err := c.command(ctx, "single", boolFlag(enabled))
	return err
}

// SetConsume toggles consume mode.
func (c *Client) SetConsume(ctx context.Context, enabled bool) error {
	_, err := c.command(ctx, "consume", boolFlag(enabled))
	return err
}
