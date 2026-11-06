package radio

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// NativeClient serializes one native endpoint's token. Native REST tokens are
// host-bound; separately authenticating parallel clients would invalidate each
// other. Every request is sent in one write for the native HTTP parser.
type NativeClient struct {
	mu                       sync.Mutex
	address, password, token string
	dispatchUntil            atomic.Int64
	lastContact              atomic.Int64
}

var nativePath = regexp.MustCompile(`^/[A-Za-z0-9/_-]+$`)

type NativeError struct {
	HTTPStatus int
	Status     int
	Message    string
	Possible   bool
	Response   map[string]json.RawMessage
}

func (e *NativeError) Error() string { return e.Message }

func NewNativeClient(address, password string) (*NativeClient, error) {
	host, port, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || port == "" || password == "" {
		return nil, errors.New("native control must use a private loopback endpoint")
	}
	return &NativeClient{address: address, password: password}, nil
}
func (c *NativeClient) Address() string { return c.address }
func (c *NativeClient) Get(ctx context.Context, path string) (map[string]json.RawMessage, error) {
	return c.request(ctx, http.MethodGet, path, nil)
}
func (c *NativeClient) Put(ctx context.Context, path string, value any) (map[string]json.RawMessage, error) {
	return c.request(ctx, http.MethodPut, path, value)
}
func (c *NativeClient) PUT(ctx context.Context, path string, value any) (map[string]json.RawMessage, error) {
	return c.Put(ctx, path, value)
}

func (c *NativeClient) request(ctx context.Context, method, path string, value any) (map[string]json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !nativePath.MatchString(path) {
		return nil, errors.New("invalid native control path")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if c.token == "" {
			sum := sha256.Sum256([]byte(c.password))
			reply, err := c.exchange(ctx, http.MethodPut, "/auth", "", map[string]string{"auth": hex.EncodeToString(sum[:])})
			if err != nil {
				return nil, err
			}
			if json.Unmarshal(reply["token"], &c.token) != nil || c.token == "" || strings.ContainsAny(c.token, "\r\n") || len(c.token) > 128 {
				c.token = ""
				return nil, errors.New("native authentication did not return a valid token")
			}
		}
		reply, err := c.exchange(ctx, method, path, c.token, value)
		var native *NativeError
		if errors.As(err, &native) && (native.HTTPStatus == 401 || native.Status == 401) {
			c.token = ""
			continue
		}
		if err == nil && (path == "/p25/dispatch/grant" || path == "/p25/dispatch/renew") {
			var accepted bool
			var state string
			_ = json.Unmarshal(reply["accepted"], &accepted)
			_ = json.Unmarshal(reply["state"], &state)
			if accepted || state == "granted" {
				c.dispatchUntil.Store(time.Now().Add(1500 * time.Millisecond).UnixNano())
			}
		}
		if err == nil && path == "/p25/dispatch/release" {
			c.dispatchUntil.Store(0)
		}
		if err == nil {
			c.lastContact.Store(time.Now().UnixNano())
		}
		return reply, err
	}
	return nil, errors.New("native authentication was rejected")
}

func (c *NativeClient) exchange(ctx context.Context, method, path, token string, value any) (map[string]json.RawMessage, error) {
	body := []byte{}
	if value != nil {
		var err error
		body, err = json.Marshal(value)
		limit := 4096
		if path == "/p25/aliases" {
			limit = 4 << 20
		}
		if err != nil || len(body) > limit {
			return nil, errors.New("native request is too large")
		}
	}
	conn, err := (&net.Dialer{Timeout: 800 * time.Millisecond}).DialContext(ctx, "tcp", c.address)
	if err != nil {
		return nil, &NativeError{Message: "Native radio control endpoint is unavailable."}
	}
	defer conn.Close()
	deadline := time.Now().Add(1200 * time.Millisecond)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	header := ""
	if token != "" {
		header = "X-DVM-Auth-Token: " + token + "\r\n"
	}
	request := fmt.Sprintf("%s %s HTTP/1.0\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nContent-Length: %d\r\n%sConnection: close\r\n\r\n%s", method, path, len(body), header, body)
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	n, err := io.WriteString(conn, request)
	if err != nil || n != len(request) {
		return nil, &NativeError{Message: "Native request was incomplete; its outcome is unconfirmed.", Possible: n > 0}
	}
	maxBody := 16384
	if path == "/p25/report-affiliations" {
		maxBody = 1 << 20
	}
	response, err := http.ReadResponse(bufio.NewReader(io.LimitReader(conn, int64(maxBody+16384))), &http.Request{Method: method})
	if err != nil {
		return nil, &NativeError{Message: "Native response was unavailable; its outcome is unconfirmed.", Possible: true}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBody+1)))
	if err != nil || len(raw) > maxBody {
		return nil, &NativeError{Message: "Native response exceeded the allowed size.", Possible: true}
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, &NativeError{Message: "Native response was invalid.", Possible: true}
	}
	status := 0
	_ = json.Unmarshal(result["status"], &status)
	if response.StatusCode != 200 || status != 200 {
		message := "Native radio control rejected the request."
		var detail string
		_ = json.Unmarshal(result["message"], &detail)
		if detail != "" && len(detail) < 400 && !strings.Contains(strings.ToLower(detail), "password") {
			message = detail
		}
		return result, &NativeError{HTTPStatus: response.StatusCode, Status: status, Message: message, Possible: true, Response: result}
	}
	return result, nil
}

func (c *NativeClient) DispatchGrant(ctx context.Context, request any) (map[string]json.RawMessage, error) {
	return c.Put(ctx, "/p25/dispatch/grant", request)
}
func (c *NativeClient) DispatchRenew(ctx context.Context, request any) (map[string]json.RawMessage, error) {
	return c.Put(ctx, "/p25/dispatch/renew", request)
}
func (c *NativeClient) DispatchRelease(ctx context.Context, request any) (map[string]json.RawMessage, error) {
	return c.Put(ctx, "/p25/dispatch/release", request)
}
func (c *NativeClient) DispatchStatus(ctx context.Context, request any) (map[string]json.RawMessage, error) {
	return c.Put(ctx, "/p25/dispatch/status", request)
}

// DispatchActive yields status polling during a bounded native transmit lease.
func (c *NativeClient) DispatchActive() bool   { return time.Now().UnixNano() < c.dispatchUntil.Load() }
func (c *NativeClient) LastContact() time.Time { return time.Unix(0, c.lastContact.Load()) }
