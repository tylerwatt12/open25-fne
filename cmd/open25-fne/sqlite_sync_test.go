package main

import (
	"runtime"
	"testing"

	"modernc.org/sqlite"
)

func TestRunSelectsSQLiteDataSyncBeforeCommandHandling(t *testing.T) {
	previous := sqlite.FdatasyncEnabled()
	if runtime.GOOS == "linux" {
		if _, err := sqlite.Fdatasync(false); err != nil {
			t.Fatal(err)
		}
		defer sqlite.Fdatasync(previous)
	}
	if err := run(nil); err == nil {
		t.Fatal("empty command unexpectedly succeeded")
	}
	want := previous
	if runtime.GOOS == "linux" {
		want = true
	}
	if sqlite.FdatasyncEnabled() != want {
		t.Fatal("CLI did not select the platform's SQLite sync before handling its command")
	}
}
