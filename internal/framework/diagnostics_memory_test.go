package framework

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestProcessDiagnosticsAreBoundedIndependentAndNeverRevived(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configuration.db")
	s, err := OpenStore(path, RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	connection, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	done := make(chan error, 1)
	go func() {
		for i := 1; i <= 150; i++ {
			if err := s.AddEvent("connection", fmt.Sprintf("diagnostic %d", i)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("diagnostic capture attempted the occupied SQLite writer")
	}
	events, err := s.Events(1000)
	if err != nil || len(events) != diagnosticLimit || events[0].ID != 150 || events[len(events)-1].ID != 51 {
		t.Fatal("diagnostics not bounded newest-first", events, err)
	}
	events[0].Message = "mutated caller copy"
	again, _ := s.Events(1)
	if again[0].Message != "diagnostic 150" {
		t.Fatal("caller changed the owning diagnostic ring")
	}
	var tables int
	if err := s.readDB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='framework_events'").Scan(&tables); err != nil || tables != 0 {
		t.Fatal("configuration still owns durable logs", tables, err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if events, err := reopened.Events(100); err != nil || len(events) != 0 {
		t.Fatal("old diagnostics revived", events, err)
	}
}

func TestProcessDiagnosticsConcurrentSnapshots(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	var group sync.WaitGroup
	for i := range 20 {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			for j := range 20 {
				if err := s.AddEvent("connection", fmt.Sprintf("%d/%d", i, j)); err != nil {
					t.Error(err)
				}
				rows, _ := s.Events(6)
				for n := 1; n < len(rows); n++ {
					if rows[n-1].ID <= rows[n].ID {
						t.Error("snapshot order changed")
					}
				}
			}
		}(i)
	}
	group.Wait()
	rows, _ := s.Events(100)
	if len(rows) != 100 || rows[0].ID != 400 {
		t.Fatal("lost or unbounded process diagnostics", rows)
	}
}
