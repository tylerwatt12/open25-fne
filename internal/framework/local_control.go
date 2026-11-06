package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"open25.local/open25-fne/internal/radio"
)

type localRadioRequest struct {
	ChannelID string `json:"channel_id,omitempty"`
}

type localRadioResponse struct {
	Radio radio.Status `json:"radio"`
	Error string       `json:"error,omitempty"`
}

// The Site's private socket is a local administration interface. The operating
// system's owner permissions provide access; it is never exposed on TCP.
func (s *Service) startLocalControl(ctx context.Context) (func(), error) {
	c := s.Config()
	if c.Role != RoleSite {
		return func() {}, nil
	}
	if s.lock == nil {
		return nil, errors.New("local radio control requires the exclusive service lock")
	}
	directory, err := os.Stat(c.StateDir)
	if err != nil {
		return nil, err
	}
	if !directory.IsDir() || directory.Mode().Perm()&0077 != 0 {
		return nil, errors.New("local radio control requires an owner-only state directory")
	}
	path := filepath.Join(c.StateDir, "control.sock")
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("control.sock exists and is not a socket; preserving it")
		}
		// OpenService already owns service.lock, so no other legitimate Site
		// process can still be serving this data directory.
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("local radio control: %w", err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	server := &http.Server{
		Handler:           s.localRadioHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      75 * time.Second,
		IdleTimeout:       10 * time.Second,
		MaxHeaderBytes:    4096,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.runtimeError(fmt.Errorf("local radio control: %w", err))
		}
	}()
	return func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
		<-done
	}, nil
}

func (s *Service) localRadioHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		write := func(status int, err error) {
			out := localRadioResponse{Radio: s.radioStatus()}
			if err != nil {
				out.Error = err.Error()
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(out)
		}
		if s.Config().Role != RoleSite {
			write(http.StatusNotFound, errors.New("radio operation belongs to a Site"))
			return
		}
		if r.URL.Path == "/status" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				write(http.StatusMethodNotAllowed, errors.New("use GET for status"))
				return
			}
			write(http.StatusOK, nil)
			return
		}
		if r.URL.Path != "/start" && r.URL.Path != "/stop" {
			write(http.StatusNotFound, errors.New("unknown local radio operation"))
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			write(http.StatusMethodNotAllowed, errors.New("use POST to start or stop"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		raw, err := io.ReadAll(r.Body)
		var input localRadioRequest
		if err == nil {
			err = DecodeStrict(raw, &input)
		}
		if err != nil || len(input.ChannelID) > 128 || strings.TrimSpace(input.ChannelID) != input.ChannelID {
			write(http.StatusBadRequest, errors.New("provide a valid channel_id or an empty object"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		if r.URL.Path == "/start" {
			err = s.startRadio(ctx, input.ChannelID)
		} else {
			err = s.stopRadio(ctx, input.ChannelID)
		}
		if err != nil {
			s.runtimeError(err)
			write(http.StatusServiceUnavailable, err)
			return
		}
		write(http.StatusOK, nil)
	})
}

// ControlRadio contacts an already-running Site. It deliberately reads the
// service file without opening its database or acquiring its process lock.
func ControlRadio(ctx context.Context, configPath, action, channel string) (radio.Status, error) {
	var result localRadioResponse
	if action != "start" && action != "stop" && action != "status" {
		return result.Radio, errors.New("use radio start, stop or status")
	}
	if action == "status" && channel != "" {
		return result.Radio, errors.New("--channel is available only for start or stop")
	}
	c, err := ReadFileConfig(configPath)
	if err != nil {
		return result.Radio, err
	}
	if c.Role != RoleSite {
		return result.Radio, errors.New("provide a Site service configuration")
	}
	socket := filepath.Join(c.StateDir, "control.sock")
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 70 * time.Second}
	method := http.MethodPost
	var body io.Reader
	if action == "status" {
		method = http.MethodGet
	} else {
		raw, _ := json.Marshal(localRadioRequest{ChannelID: channel})
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://site/"+action, body)
	if err != nil {
		return result.Radio, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return result.Radio, fmt.Errorf("contact the running Site: %w", err)
	}
	defer response.Body.Close()
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return result.Radio, err
	}
	if response.StatusCode != http.StatusOK {
		if result.Error == "" {
			result.Error = response.Status
		}
		return result.Radio, errors.New(result.Error)
	}
	return result.Radio, nil
}
