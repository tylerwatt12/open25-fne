package framework

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"open25.local/open25-fne/internal/dispatch"
)

const maxSpeechCharacters = 500

func (s *Service) speechAvailable() bool {
	path := s.Config().Media.SpeechBinary
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

// Speech generation is offline and file-only. Text is stdin, never an argument,
// shell command, SSML document, filename, URL or selected system audio device.
func (s *Service) synthesizeSpeech(ctx context.Context, text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxSpeechCharacters || strings.IndexFunc(text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0 {
		return nil, errors.New("enter 1 to 500 characters of announcement text")
	}
	if !s.speechAvailable() {
		return nil, errors.New("the packaged speech synthesizer is not configured at this Application Server")
	}
	s.mu.Lock()
	if s.speechSlots == nil {
		s.speechSlots = make(chan struct{}, 2)
	}
	slots := s.speechSlots
	s.mu.Unlock()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, errors.New("the speech generator is busy; try again shortly")
	}
	dir, err := os.MkdirTemp(s.Config().StateDir, "speech-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	output := filepath.Join(dir, "announcement.wav")
	renderCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(renderCtx, s.Config().Media.SpeechBinary, "-v", "en-us", "-s", "175", "-w", output, "--stdin")
	cmd.WaitDelay = 250 * time.Millisecond
	cmd.Stdin = strings.NewReader(text)
	// A failing tool's diagnostics may contain the input text; do not publish it.
	var diagnostic limitedSpeechOutput
	cmd.Stderr = &diagnostic
	if err = cmd.Run(); err != nil {
		if renderCtx.Err() != nil {
			return nil, renderCtx.Err()
		}
		return nil, errors.New("speech generation failed")
	}
	info, err := os.Stat(output)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, errors.New("speech output exceeds the supported size")
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		return nil, err
	}
	return speechPCM(raw)
}

type limitedSpeechOutput struct{ n int }

func (w *limitedSpeechOutput) Write(b []byte) (int, error) {
	w.n += len(b)
	if w.n > 16384 {
		return 0, errors.New("speech diagnostic output exceeded limit")
	}
	return len(b), nil
}

// speechPCM accepts only the synthesizer's mono PCM16 WAV and converts it to
// the existing 8 kHz radio path with a windowed low-pass interpolation filter.
func speechPCM(raw []byte) ([]byte, error) {
	bad := errors.New("speech output must be mono PCM16 WAV, up to 30 seconds")
	if len(raw) < 44 || len(raw) > 4<<20 || string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(raw[4:8]))+8 != uint64(len(raw)) {
		return nil, bad
	}
	rate := 0
	var data []byte
	format := false
	for at := 12; at < len(raw); {
		if len(raw)-at < 8 {
			return nil, bad
		}
		size := uint64(binary.LittleEndian.Uint32(raw[at+4 : at+8]))
		end := uint64(at+8) + size
		next := end + size%2
		if end > uint64(len(raw)) || next > uint64(len(raw)) {
			return nil, bad
		}
		body := raw[at+8 : int(end)]
		switch string(raw[at : at+4]) {
		case "fmt ":
			if format || len(body) < 16 || binary.LittleEndian.Uint16(body) != 1 || binary.LittleEndian.Uint16(body[2:]) != 1 || binary.LittleEndian.Uint16(body[14:]) != 16 {
				return nil, bad
			}
			rate = int(binary.LittleEndian.Uint32(body[4:]))
			if rate < 8000 || rate > 48000 || binary.LittleEndian.Uint32(body[8:]) != uint32(rate*2) || binary.LittleEndian.Uint16(body[12:]) != 2 {
				return nil, bad
			}
			format = true
		case "data":
			if !format || data != nil || size == 0 || size%2 != 0 {
				return nil, bad
			}
			data = body
		}
		at = int(next)
	}
	if !format || len(data) == 0 || len(data) > rate*2*30 {
		return nil, errors.New("announcement exceeds 30 seconds; shorten the text")
	}
	if rate == 8000 {
		return bytes.Clone(data), nil
	}
	input := len(data) / 2
	count := input * 8000 / rate
	out := make([]byte, count*2)
	cutoff := .45 * 8000 / float64(rate)
	const half = 32
	for n := 0; n < count; n++ {
		center := float64(n) * float64(rate) / 8000
		start := int(math.Floor(center))
		total, weight := 0.0, 0.0
		for j := start - half + 1; j <= start+half; j++ {
			if j < 0 || j >= input {
				continue
			}
			delta := float64(j) - center
			x := 2 * cutoff * delta
			sinc := 1.0
			if x != 0 {
				sinc = math.Sin(math.Pi*x) / (math.Pi * x)
			}
			win := .54 + .46*math.Cos(math.Pi*delta/half)
			w := 2 * cutoff * sinc * win
			sample := int16(binary.LittleEndian.Uint16(data[j*2:]))
			total += float64(sample) * w
			weight += w
		}
		value := 0.0
		if weight != 0 {
			value = total / weight
		}
		value = math.Max(-32768, math.Min(32767, math.Round(value)))
		binary.LittleEndian.PutUint16(out[n*2:], uint16(int16(value)))
	}
	return out, nil
}
func (s *Service) speechHTTP(w http.ResponseWriter, r *http.Request, p dispatch.Principal, sessionKey string) {
	if r.URL.Path == "/api/dispatch/speech" {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		respond(w, 200, map[string]any{"available": s.speechAvailable(), "max_characters": maxSpeechCharacters, "max_seconds": 30})
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	var input struct {
		Text        string `json:"text"`
		Talkgroups  []int  `json:"talkgroups"`
		Secure      bool   `json:"secure"`
		AlgorithmID int    `json:"algorithm_id"`
		KeyID       int    `json:"key_id"`
	}
	if err := readJSON(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	if !p.CanTransmit {
		fail(w, 403, dispatch.ErrPermission)
		return
	}
	pcm, err := s.synthesizeSpeech(r.Context(), input.Text)
	if err != nil {
		fail(w, 400, err)
		return
	}
	frames, err := s.hub.PlayPCM(r.Context(), p, sessionKey, input.Talkgroups, pcm, input.Secure, input.AlgorithmID, input.KeyID)
	if err != nil {
		fail(w, 409, err)
		return
	}
	respond(w, 200, map[string]any{"completed": true, "frames": frames, "duration_ms": frames * 20})
}
