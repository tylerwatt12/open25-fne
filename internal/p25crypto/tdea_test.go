package p25crypto

import (
	"bytes"
	"crypto/cipher"
	"crypto/des"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// AAAD-B-2015 B.1, PDF45/printed35; effective DES key bits exclude parity.
func TestTDEAKeyingOptionOne(t *testing.T) {
	key, _ := hex.DecodeString("0123456789ABCDEF23456789ABCDEF01456789ABCDEF0123")
	if !Supported(0x83) || !ValidSelection(true, 0x83, 0) || ValidateKey(0x83, key) != nil {
		t.Fatal("published three-key TDEA key or encrypted KID zero rejected")
	}
	for first := 0; first < 3; first++ {
		for second := first + 1; second < 3; second++ {
			for _, parity := range []byte{0, 1} {
				invalid := append([]byte(nil), key...)
				for i := 0; i < 8; i++ {
					invalid[second*8+i] = invalid[first*8+i] ^ parity
				}
				if ValidateKey(0x83, invalid) == nil {
					t.Fatalf("equal effective subkeys %d/%d with parity change %d accepted", first, second, parity)
				}
			}
		}
	}
	for i := range key {
		key[i] ^= 1
	}
	if ValidateKey(0x83, key) != nil {
		t.Fatal("parity-only change of three distinct subkeys rejected")
	}
	if ValidateKey(0x81, key[:8]) != nil || Pair(0x83, 0) == Pair(0x81, 0) {
		t.Fatal("single DES or algorithm/key pair identity changed")
	}
}

// Independent standard-library cipher implementation checks the transcription
// of every B.4 output register and FDMA word, not generated native expectations.
// Exact designation/path/pages are retained beside the fixture.
func TestTDEAPublishedFDMAOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/phase1-tdea-published-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Key, MI         string
		OutputRegisters []string `json:"output_registers"`
		Voice           []struct {
			Index         int
			Plain, Cipher string
		}
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	decode := func(text string) []byte {
		t.Helper()
		value, e := hex.DecodeString(text)
		if e != nil {
			t.Fatal(e)
		}
		return value
	}
	key, mi := decode(fixture.Key), decode(fixture.MI)
	block, err := des.NewTripleDESCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	stream := make([]byte, 224)
	cipher.NewOFB(block, mi[:8]).XORKeyStream(stream, stream)
	if len(fixture.OutputRegisters) != 28 || len(fixture.Voice) != 18 {
		t.Fatal("incomplete published fixture")
	}
	for i, expected := range fixture.OutputRegisters {
		if !bytes.Equal(stream[i*8:i*8+8], decode(expected)) {
			t.Fatalf("published output register %d disagrees", i+1)
		}
	}
	for _, row := range fixture.Voice {
		offset := 19 + row.Index%9*11
		if row.Index >= 9 {
			offset += 101
		}
		if row.Index%9 == 8 {
			offset += 2
		}
		word := decode(row.Plain)
		for i := range word {
			word[i] ^= stream[offset+i]
		}
		if !bytes.Equal(word, decode(row.Cipher)) {
			t.Fatalf("published FDMA word %d disagrees", row.Index)
		}
	}
}
