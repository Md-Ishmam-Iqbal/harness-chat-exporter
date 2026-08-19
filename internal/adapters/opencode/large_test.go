//go:build large

package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/domain"
)

func TestLargeSQLiteStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE session (id TEXT PRIMARY KEY, time_created INTEGER, time_updated INTEGER)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, data TEXT)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created INTEGER, data TEXT)`,
		`CREATE INDEX message_session_idx ON message(session_id)`,
		`CREATE INDEX part_message_idx ON part(message_id)`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	sessionInsert, _ := tx.Prepare(`INSERT INTO session VALUES(?,?,?)`)
	messageInsert, _ := tx.Prepare(`INSERT INTO message VALUES(?,?,?,?)`)
	partInsert, _ := tx.Prepare(`INSERT INTO part VALUES(?,?,?,?,?)`)
	for sessionIndex := 0; sessionIndex < 5000; sessionIndex++ {
		sessionID := fmt.Sprintf("ses_%05d", sessionIndex)
		messageID := fmt.Sprintf("msg_%05d", sessionIndex)
		base := int64(1786000000000 + sessionIndex*1000)
		if _, err := sessionInsert.Exec(sessionID, base, base+999); err != nil {
			t.Fatal(err)
		}
		if _, err := messageInsert.Exec(messageID, sessionID, base, `{"role":"assistant"}`); err != nil {
			t.Fatal(err)
		}
		for partIndex := 0; partIndex < 80; partIndex++ {
			partID := fmt.Sprintf("prt_%05d_%03d", sessionIndex, partIndex)
			if _, err := partInsert.Exec(partID, messageID, sessionID, base+int64(partIndex), `{"type":"text","text":"generated"}`); err != nil {
				t.Fatal(err)
			}
		}
	}
	sessionInsert.Close()
	messageInsert.Close()
	partInsert.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	adapter := New(path)
	detection := adapter.Detect(context.Background(), domain.Environment{Variables: map[string]string{}})
	var selected domain.SessionReference
	discovered := 0
	if err := adapter.Discover(context.Background(), domain.DiscoveryOptions{Roots: detection.Roots}, func(reference domain.SessionReference) error {
		discovered++
		if reference.NativeSessionID == "ses_02500" {
			selected = reference
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if discovered != 5000 || selected.NativeSessionID == "" {
		t.Fatalf("discovered=%d selected=%q", discovered, selected.NativeSessionID)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	emitted := int64(0)
	result := adapter.Parse(context.Background(), selected, func(_ context.Context, _ domain.NativeEvent) error {
		emitted++
		return nil
	})
	if result.Err != nil || emitted != 82 { // session + message metadata + 80 text parts
		t.Fatalf("large parse emitted=%d result=%#v", emitted, result)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.Alloc > before.Alloc+150<<20 {
		t.Fatalf("retained heap grew by more than 150 MiB: before=%d after=%d", before.Alloc, after.Alloc)
	}
}
