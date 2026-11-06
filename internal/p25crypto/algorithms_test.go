package p25crypto

import "testing"

func TestTrafficAlgorithmCatalogueAndExactKeySizes(t *testing.T) {
	for _, algorithm := range Algorithms() {
		for _, size := range []int{0, algorithm.KeyBytes - 1, algorithm.KeyBytes, algorithm.KeyBytes + 1} {
			material := make([]byte, size)
			if algorithm.ID == 0x83 && size == algorithm.KeyBytes {
				copy(material, []byte("01234567ABCDEFGHabcdefgh"))
			}
			got := ValidateKey(algorithm.ID, material)
			if (got == nil) != (algorithm.Supported && size == algorithm.KeyBytes) {
				t.Fatalf("algorithm %x size %d accepted=%v", algorithm.ID, size, got == nil)
			}
		}
		if ValidSelection(true, algorithm.ID, 7) != algorithm.Supported {
			t.Fatalf("algorithm %x admission disagrees", algorithm.ID)
		}
	}
	for _, id := range []int{0, Clear, 256, -1} {
		if Supported(id) || ValidateKey(id, make([]byte, 32)) == nil {
			t.Fatalf("unsupported algorithm %d accepted", id)
		}
	}
	for _, selection := range []struct {
		secure         bool
		algorithm, key int
	}{{true, 0, 7}, {true, 132, -1}, {true, 132, 65536}, {false, 132, 7}, {false, 132, 0}, {false, 0, 7}} {
		if ValidSelection(selection.secure, selection.algorithm, selection.key) {
			t.Fatalf("bad selection accepted: %+v", selection)
		}
	}
	if !ValidSelection(true, 129, 0) || !ValidSelection(true, 132, 0) || !ValidSelection(true, 170, 0) {
		t.Fatal("encrypted KID0 rejected")
	}
	if !ValidSelection(false, 0, 0) {
		t.Fatal("clear selection rejected")
	}
	values := Algorithms()
	values[0].Supported = false
	if !Supported(0x81) {
		t.Fatal("caller mutated shared catalog")
	}
}

// BAAC-D 2.6 reserves null MI for clear traffic; AAAD-B 3 maps the effective
// 64 bits to the first eight CAI octets. Exercise every nonzero boundary bit.
func TestVoiceMessageIndicatorEffectiveBits(t *testing.T) {
	for _, size := range []int{0, 8, 9, 10} {
		if ValidMessageIndicator(make([]byte, size)) {
			t.Fatalf("null or wrong-size MI accepted: %d", size)
		}
	}
	for bit := 0; bit < 72; bit++ {
		mi := make([]byte, 9)
		mi[bit/8] = 1 << (bit % 8)
		if ValidMessageIndicator(mi) != (bit < 64) {
			t.Fatalf("MI effective bit %d was misclassified", bit)
		}
	}
}
