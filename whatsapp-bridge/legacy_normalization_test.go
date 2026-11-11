package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func seedLegacySenderFixture(t *testing.T, ms *MessageStore) string {
	t.Helper()
	whatsappDBPath := filepath.Join(t.TempDir(), "whatsapp.db")
	waDB, err := sql.Open("sqlite3", whatsappDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waDB.Exec(`
		CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT NOT NULL);
		INSERT INTO whatsmeow_lid_map (lid, pn) VALUES ('111', '222');
	`); err != nil {
		t.Fatal(err)
	}
	_ = waDB.Close()
	if _, err := ms.db.Exec(`
		INSERT INTO chats (jid, name) VALUES ('222@s.whatsapp.net', 'Peer');
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('m1', '222@s.whatsapp.net', '111', 'x', '2026-03-01T10:00:00Z', 0);
	`); err != nil {
		t.Fatal(err)
	}
	return whatsappDBPath
}

func TestStartLegacyNormalizationRunsInBackgroundAndReleasesWhatsAppDB(t *testing.T) {
	ms := newTestMessageStore(t)
	whatsappDBPath := seedLegacySenderFixture(t, ms)

	stop, done := ms.startLegacyNormalization(whatsappDBPath, testLogger())
	defer stop()

	// Handlers keep writing while the migration runs.
	if err := ms.StoreChat("333@s.whatsapp.net", "Other", time.Now()); err != nil {
		t.Fatalf("concurrent StoreChat failed: %v", err)
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("background normalization did not finish")
	}

	var sender string
	if err := ms.db.QueryRow(`SELECT sender FROM messages WHERE id = 'm1'`).Scan(&sender); err != nil {
		t.Fatal(err)
	}
	if sender != "222" {
		t.Fatalf("sender = %q, want 222", sender)
	}
	var attached int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM pragma_database_list WHERE name NOT IN ('main', 'temp')`).Scan(&attached); err != nil {
		t.Fatal(err)
	}
	if attached != 0 {
		t.Fatalf("%d databases still attached after normalization", attached)
	}
	// On Windows a leaked ATTACH keeps whatsapp.db open and this fails.
	if err := os.Remove(whatsappDBPath); err != nil {
		t.Fatalf("whatsapp.db still held open after normalization: %v", err)
	}
}

func TestStartLegacyNormalizationStopCancelsAndKeepsStoreUsable(t *testing.T) {
	ms := newTestMessageStore(t)
	whatsappDBPath := seedLegacySenderFixture(t, ms)

	stop, done := ms.startLegacyNormalization(whatsappDBPath, testLogger())
	stop()
	select {
	case <-done:
	default:
		t.Fatal("stop returned before the goroutine exited")
	}
	if err := ms.StoreChat("444@s.whatsapp.net", "", time.Now()); err != nil {
		t.Fatalf("store unusable after cancelled normalization: %v", err)
	}
}
