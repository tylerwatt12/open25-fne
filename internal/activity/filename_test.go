package activity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestActivityLiteralFilenamesStayIndependentThroughRebuild(t *testing.T) {
	// The driver treats an unescaped '?' as connection options. These two
	// filenames previously opened the same truncated name instead of these files.
	directory := filepath.Join(t.TempDir(), "[lab] ? % #")
	for _, name := range []string{"first", "second"} {
		path := filepath.Join(directory, name+".db")
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		e := event(name, "call_start", "site-1", time.Now())
		e.CallID = "call-" + name
		if err = s.Append(e); err != nil {
			s.Close()
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := RebuildHistory(filepath.Join(directory, "first.db")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		path := filepath.Join(directory, name+".db")
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) < 16 || string(raw[:16]) != "SQLite format 3\x00" {
			t.Fatal("database was not written to its literal filename", err)
		}
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := s.Events(Query{})
		if err != nil || len(rows) != 1 || rows[0].ID != name {
			s.Close()
			t.Fatal("observations crossed database filenames", name, rows, err)
		}
		calls, err := s.CallHistory(Query{})
		if err != nil || len(calls.Calls) != 1 || calls.Calls[0].ID != "call-"+name {
			s.Close()
			t.Fatal("rebuild changed the other database or lost the call", name, calls, err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
