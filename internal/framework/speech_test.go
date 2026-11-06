package framework

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
)

func speechTestWAV(rate, samples int, frequency float64) []byte {
	raw := make([]byte, 44+samples*2)
	copy(raw, "RIFF")
	binary.LittleEndian.PutUint32(raw[4:], uint32(len(raw)-8))
	copy(raw[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(raw[16:], 16)
	binary.LittleEndian.PutUint16(raw[20:], 1)
	binary.LittleEndian.PutUint16(raw[22:], 1)
	binary.LittleEndian.PutUint32(raw[24:], uint32(rate))
	binary.LittleEndian.PutUint32(raw[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(raw[32:], 2)
	binary.LittleEndian.PutUint16(raw[34:], 16)
	copy(raw[36:], "data")
	binary.LittleEndian.PutUint32(raw[40:], uint32(samples*2))
	for i := 0; i < samples; i++ {
		value := int16(math.Sin(2*math.Pi*frequency*float64(i)/float64(rate)) * 12000)
		binary.LittleEndian.PutUint16(raw[44+i*2:], uint16(value))
	}
	return raw
}
func speechRMS(pcm []byte) float64 {
	total := 0.0
	for i := 0; i < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		total += v * v
	}
	return math.Sqrt(total / float64(len(pcm)/2))
}
func TestSpeechPCMResamplesAndRejectsOutOfBandAliasing(t *testing.T) {
	for _, rate := range []int{8000, 11025, 16000, 22050, 48000} {
		pcm, err := speechPCM(speechTestWAV(rate, rate, 675))
		if err != nil || len(pcm) != 16000 {
			t.Fatalf("rate %d: %d %v", rate, len(pcm), err)
		}
		if power := speechRMS(pcm); power < 8000 || power > 9000 {
			t.Fatal("speech-band signal attenuated", rate, power)
		}
	}
	pass, _ := speechPCM(speechTestWAV(22050, 22050, 675))
	stop, _ := speechPCM(speechTestWAV(22050, 22050, 6000))
	if speechRMS(stop) > speechRMS(pass)*.03 {
		t.Fatal("downsampling aliased out-of-band audio", speechRMS(stop))
	}
}
func TestSpeechPCMRejectsMalformedAndOverlongOutput(t *testing.T) {
	tooLong := speechTestWAV(8000, 8000*30+1, 675)
	if _, err := speechPCM(tooLong); err == nil {
		t.Fatal("overlong speech accepted")
	}
	for _, modify := range []func([]byte){func(b []byte) { b[22] = 2 }, func(b []byte) { b[20] = 3 }, func(b []byte) { b[34] = 8 }, func(b []byte) { b[4] = 0 }, func(b []byte) { b[24] = 1; b[25] = 0; b[26] = 0; b[27] = 0 }} {
		raw := speechTestWAV(22050, 22050, 675)
		modify(raw)
		if _, err := speechPCM(raw); err == nil {
			t.Fatal("malformed output accepted")
		}
	}
}
func speechEngineFixture(t *testing.T, script string) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "speech-engine")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return &Service{config: FileConfig{StateDir: root, Media: MediaOptions{SpeechBinary: path}}}, root
}
func TestSpeechSynthesisUsesStdinAndPrivateBoundedOutput(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture.wav")
	captured := filepath.Join(root, "input.txt")
	if err := os.WriteFile(fixture, speechTestWAV(22050, 2205, 675), 0600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("out=''\nwhile [ $# -gt 0 ]; do\n if [ \"$1\" = '-w' ]; then shift; out=$1; fi\n shift\ndone\ncat > %q\ncp %q \"$out\"\n", captured, fixture)
	s, state := speechEngineFixture(t, script)
	text := "Literal $(not-a-command) & --help <text>"
	pcm, err := s.synthesizeSpeech(context.Background(), text)
	if err != nil || len(pcm) != 1600 {
		t.Fatalf("%d %v", len(pcm), err)
	}
	got, _ := os.ReadFile(captured)
	if string(got) != text {
		t.Fatal("speech text became executable arguments")
	}
	paths, _ := filepath.Glob(filepath.Join(state, "speech-*"))
	for _, path := range paths {
		if info, _ := os.Stat(path); info != nil && info.IsDir() {
			t.Fatal("temporary speech retained", path)
		}
	}
	for _, text := range []string{"", strings.Repeat("a", 501), "bad\x00text"} {
		if _, err := s.synthesizeSpeech(context.Background(), text); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
}
func TestSpeechCancellationAndMissingEngine(t *testing.T) {
	s, _ := speechEngineFixture(t, "sleep 5\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.synthesizeSpeech(ctx, "test"); err == nil || time.Since(start) > time.Second {
		t.Fatal("speech process did not cancel promptly", err)
	}
	s.config.Media.SpeechBinary = ""
	if s.speechAvailable() {
		t.Fatal("missing engine advertised")
	}
	if _, err := s.synthesizeSpeech(context.Background(), "test"); err == nil {
		t.Fatal("missing engine silently synthesized")
	}
}

type speechTestRouter struct {
	begins    atomic.Int32
	frames    atomic.Int32
	ends      atomic.Int32
	secure    atomic.Bool
	algorithm atomic.Int32
	key       atomic.Int32
}

func (r *speechTestRouter) Begin(_ context.Context, q dispatch.CallRequest) (dispatch.Grant, error) {
	r.begins.Add(1)
	r.secure.Store(q.Secure)
	r.algorithm.Store(int32(q.AlgorithmID))
	r.key.Store(int32(q.KeyID))
	return dispatch.Grant{ID: q.ID, Talkgroups: q.Talkgroups, Secure: q.Secure, AlgorithmID: q.AlgorithmID, KeyID: q.KeyID}, nil
}
func (r *speechTestRouter) Write(_ context.Context, _ dispatch.Grant, _ dispatch.Frame) error {
	r.frames.Add(1)
	return nil
}
func (r *speechTestRouter) Renew(context.Context, dispatch.Grant) error { return nil }
func (r *speechTestRouter) End(context.Context, dispatch.Grant) error   { r.ends.Add(1); return nil }
func TestSpeechHTTPUsesExistingCallFloorAndPermission(t *testing.T) {
	root := t.TempDir()
	wave := filepath.Join(root, "short.wav")
	if err := os.WriteFile(wave, speechTestWAV(8000, 800, 675), 0600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("out=''\nwhile [ $# -gt 0 ]; do\n if [ \"$1\" = '-w' ]; then shift; out=$1; fi\n shift\ndone\ncat >/dev/null\ncp %q \"$out\"\n", wave)
	s, _ := speechEngineFixture(t, script)
	router := &speechTestRouter{}
	s.hub = dispatch.New(dispatch.Options{Router: router})
	defer s.hub.Close()
	p := dispatch.Principal{UserID: "dispatcher", RadioID: 10002, TransmitGroups: []int{1001}, CanTransmit: true, CanSecure: true}
	run := func(body string, p dispatch.Principal) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/dispatch/tts", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.speechHTTP(w, r, p, "test-session")
		return w
	}
	w := run(`{"text":"Test","talkgroups":[1001],"secure":true,"algorithm_id":129,"key_id":0}`, p)
	if w.Code != 200 || router.begins.Load() != 1 || router.frames.Load() != 5 || !router.secure.Load() || router.ends.Load() != 1 {
		t.Fatalf("%d %s begins%d frames%d ends%d", w.Code, w.Body.String(), router.begins.Load(), router.frames.Load(), router.ends.Load())
	}
	if router.algorithm.Load() != 129 || router.key.Load() != 0 {
		t.Fatal("speech lost operator-selected key")
	}
	p.CanTransmit = false
	w = run(`{"text":"Test","talkgroups":[1001]}`, p)
	if w.Code != 403 || router.begins.Load() != 1 {
		t.Fatal("receive-only user transmitted")
	}
	p.CanTransmit = true
	w = run(`{"text":"Test","talkgroups":[1002]}`, p)
	if w.Code != 409 || router.begins.Load() != 1 {
		t.Fatal("unpermitted talkgroup reached allocator")
	}
	p.CanSecure = false
	w = run(`{"text":"Test","talkgroups":[1001],"secure":true}`, p)
	if w.Code != 409 || router.begins.Load() != 2 || router.frames.Load() != 5 || router.ends.Load() != 2 {
		t.Fatal("secure permission bypassed")
	}
}
