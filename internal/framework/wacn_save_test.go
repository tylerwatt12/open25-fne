package framework

import (
	"bytes"
	"testing"
)

func TestSystemSavePreservesWACNRestrictedForOperation(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	for _, wacn := range []int{0xBEE00, 0xBEE80, 0xBEEFF} {
		config := storeFixtureSystem()
		config.WACN = wacn
		raw := storeDoc(t, config)
		if err := s.SaveConfig(raw); err != nil {
			t.Fatal("operating restriction blocked administrator Save", err)
		}
		saved, err := s.Config()
		if err != nil || !bytes.Equal(raw, saved) {
			t.Fatalf("operating restriction altered saved WACN %05X: %s %v", wacn, saved, err)
		}
	}
}
