package framework

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in on the supported target, using the real separately verified payload.
// This generates PCM only; no service, audio hardware or RF path is opened.
func TestPackagedSpeechRuntimeThroughApplicationPCM(t *testing.T) {
	binary := os.Getenv("TRUNKSYSTEM_TEST_SPEECH_BINARY")
	if binary == "" {
		t.Skip("set TRUNKSYSTEM_TEST_SPEECH_BINARY to the packaged launcher on Linux arm64")
	}
	root := t.TempDir()
	s := &Service{config: FileConfig{StateDir: root, Media: MediaOptions{SpeechBinary: binary}}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pcm, err := s.synthesizeSpeech(ctx, "Radio test ready. Please check the console.")
	if err != nil {
		t.Fatal(err)
	}
	frames := len(pcm) / 2
	if frames < 16000 || frames > 8000*10 || speechRMS(pcm) < 500 {
		t.Fatalf("real speech was not usable 8kHz PCM: frames=%d RMS=%f", frames, speechRMS(pcm))
	}
	leftovers, err := filepath.Glob(filepath.Join(root, "speech-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatal("speech plaintext output retained", leftovers, err)
	}
	t.Logf("actual packaged speech converted to mono PCM16/8000Hz: frames=%d duration=%.3fs RMS=%.1f", frames, float64(frames)/8000, speechRMS(pcm))
}
