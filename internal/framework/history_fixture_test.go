package framework

import (
	"context"
	"errors"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

// Ordinary tests explicitly prepare their disposable history. Production
// construction and radio work deliberately never wait for this initialization.
func waitHistoryInitialization(t *testing.T, store *activity.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		err := store.InitializationError()
		if err == nil {
			return
		}
		if !errors.Is(err, activity.ErrInitializing) {
			t.Fatal("fixture history initialization failed", err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture history did not initialize", ctx.Err())
		case <-ticker.C:
		}
	}
}
