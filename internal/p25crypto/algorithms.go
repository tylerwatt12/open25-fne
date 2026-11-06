// Package p25crypto describes supported Phase 1 traffic-key identities. It
// contains no traffic-key storage and does not grant access to key material.
package p25crypto

import "fmt"

const Clear = 0x80

type Algorithm struct {
	ID        int    `json:"algorithm_id"`
	Name      string `json:"name"`
	KeyBytes  int    `json:"key_bytes"`
	Supported bool   `json:"supported"`
}

// Catalogue includes requested named algorithms even when their Phase 1
// framing has not been implemented. A name alone never enables transmission.
var catalogue = []Algorithm{
	{0x81, "DES-OFB", 8, true},
	{0x82, "2-key 3DES", 16, false},
	{0x83, "3-key 3DES", 24, true},
	{0x84, "AES-256", 32, true},
	{0x85, "AES-128", 16, false},
	{0x89, "AES-128-OFB", 16, false},
	{0x9f, "DES-XL", 8, false},
	{0xaa, "ADP", 5, true},
}

type Identity struct {
	AlgorithmID int
	KeyID       int
}

func Pair(algorithmID, keyID int) Identity { return Identity{algorithmID, keyID} }
func Algorithms() []Algorithm              { return append([]Algorithm(nil), catalogue...) }
func Lookup(id int) (Algorithm, bool) {
	for _, algorithm := range catalogue {
		if algorithm.ID == id {
			return algorithm, true
		}
	}
	return Algorithm{}, false
}
func Supported(id int) bool { a, ok := Lookup(id); return ok && a.Supported }
func ValidateKey(id int, material []byte) error {
	a, ok := Lookup(id)
	if !ok || !a.Supported {
		return fmt.Errorf("traffic encryption algorithm is not supported")
	}
	if len(material) != a.KeyBytes {
		return fmt.Errorf("%s keys contain exactly %d hexadecimal characters", a.Name, a.KeyBytes*2)
	}
	// AAAD-B-2015 B.1, PDF45/printed35: ALGID83 uses keying option1.
	// DES parity occupies the low bit of each octet. Equal effective
	// subkeys must never masquerade as independent three-key TDEA.
	if id == 0x83 {
		for first := 0; first < 3; first++ {
			for second := first + 1; second < 3; second++ {
				var difference byte
				for i := 0; i < 8; i++ {
					difference |= (material[first*8+i] ^ material[second*8+i]) & 0xfe
				}
				if difference == 0 {
					return fmt.Errorf("3-key 3DES requires three distinct DES subkeys; use DES-OFB for a single DES key")
				}
			}
		}
	}
	return nil
}
func ValidSelection(secure bool, algorithmID, keyID int) bool {
	if !secure {
		return algorithmID == 0 && keyID == 0
	}
	return Supported(algorithmID) && keyID >= 0 && keyID <= 65535
}

// ValidMessageIndicator checks the 64 effective MI bits used by the supported
// Phase 1 voice algorithms. A null encrypted MI is reserved for clear traffic
// (TIA-102.BAAC-D 2.6; TIA-102.AAAD-B 3/3.1). The final CAI octet cannot make
// an all-zero cipher initialization valid.
func ValidMessageIndicator(mi []byte) bool {
	if len(mi) != 9 {
		return false
	}
	var effective byte
	for _, value := range mi[:8] {
		effective |= value
	}
	return effective != 0
}
