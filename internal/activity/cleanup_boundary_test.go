package activity

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"open25.local/open25-fne/internal/historypriority"
)

func TestCleanupCompletesAFullFinalBatchAndDetectsOneRemainingRow(t *testing.T) {
	for _, table := range []string{"radio_events", "calls", "position_history"} {
		for _, count := range []int{maintenanceBatch, maintenanceBatch + 1} {
			t.Run(fmt.Sprintf("%s/%d", table, count), func(t *testing.T) {
				var turns atomic.Int32
				allowNext := make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(allowNext) }) }
				s, err := open(filepath.Join(t.TempDir(), "activity.db"), true, func(ctx context.Context) (*historypriority.Turn, error) {
					// Initialization, then one cleanup turn. Hold any next turn
					// so a redundant empty pass cannot mask false pending status.
					if turns.Add(1) == 3 {
						select {
						case <-allowNext:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					return &historypriority.Turn{}, ctx.Err()
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { release(); _ = s.Close() })
				waitActivityReady(t, s)
				if table == "radio_events" {
					seedArchiveRows(t, s, count, time.Now().Add(-40*24*time.Hour))
				} else {
					// Offline fixture preparation while the owning writer is idle.
					tx, err := s.db.Begin()
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					for i := range count {
						e := event(fmt.Sprintf("expired-%02d", i), "call_start", "site-1", time.Now().Add(-40*24*time.Hour))
						e.CallID = e.ID
						if table == "position_history" {
							e.Type = "location"
							e.Details = map[string]any{"latitude": 41.5, "longitude": -81.7, "accuracy": 5, "fix_at": e.At.Format(time.RFC3339Nano)}
						}
						if err := projectHistory(tx, e); err != nil {
							t.Fatal(err)
						}
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
				}
				var seeded int
				if err := s.readDB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&seeded); err != nil || seeded != count {
					t.Fatal("fixture did not prepare the requested eligible records", seeded, err)
				}
				if err := s.RequestCleanup(); err != nil {
					t.Fatal(err)
				}
				removedCount := func(c CleanupStatus) int64 {
					switch table {
					case "calls":
						return c.Removed.Calls
					case "position_history":
						return c.Removed.Locations
					default:
						return c.Removed.Events
					}
				}
				first := waitCleanupState(t, s, func(c CleanupStatus) bool { return !c.Running && removedCount(c) == maintenanceBatch })
				var remaining int
				if err := s.readDB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&remaining); err != nil || remaining != count-maintenanceBatch {
					t.Fatal("cleanup changed its 32-row deletion bound", remaining, err)
				}
				if count == maintenanceBatch {
					if first.Pending || first.LastCompletedAt == nil {
						t.Fatal("full final batch falsely stayed pending", first)
					}
				} else {
					if !first.Pending || first.LastCompletedAt != nil {
						t.Fatal("33rd eligible row was hidden", first)
					}
					release()
					last := waitCleanupState(t, s, func(c CleanupStatus) bool { return !c.Pending && !c.Running && c.LastCompletedAt != nil })
					if removedCount(last) != int64(count) {
						t.Fatal("cleanup omitted its remaining row", last)
					}
				}
				release()
			})
		}
	}
}

func TestCleanupExtraRowDetectionDoesNotPublishRolledBackProgress(t *testing.T) {
	s := activityOpen(t)
	seedArchiveRows(t, s, maintenanceBatch+1, time.Now().Add(-40*24*time.Hour))
	if _, err := s.db.Exec("CREATE TRIGGER reject_incoming BEFORE INSERT ON radio_events BEGIN SELECT RAISE(ABORT,'injected incoming failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(context.Background(), coalescedEvents("new", 2)...); err == nil {
		t.Fatal("failed transaction falsely acknowledged the incoming batch")
	}
	var remaining int
	if err := s.readDB.QueryRow("SELECT COUNT(*) FROM radio_events").Scan(&remaining); err != nil || remaining != maintenanceBatch+1 {
		t.Fatal("rollback lost existing records", remaining, err)
	}
	status := s.CleanupStatus()
	if !status.Pending || status.Running || status.LastCompletedAt != nil || status.Removed.Events != 0 || status.LastError == "" {
		t.Fatal("rollback published cleanup progress or completion", status)
	}
}
