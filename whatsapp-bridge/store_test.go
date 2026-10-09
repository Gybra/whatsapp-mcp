package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	adaJID   = "12025550123@s.whatsapp.net"
	bobJID   = "12025550199@s.whatsapp.net"
	teamJID  = "120363025555555555@g.us"
	otherJID = "112025550123@s.whatsapp.net" // contains Ada's number as a substring
)

var t0 = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }

func seededStore(t *testing.T) *MessageStore {
	t.Helper()
	store, err := OpenMessageStore(privateDataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	for _, c := range []struct {
		jid, name string
		last      time.Time
	}{{adaJID, "Ada", at(30)}, {bobJID, "Bob", at(10)}, {teamJID, "Team 50% done", at(40)}, {otherJID, "Not Ada", at(1)}} {
		if err := store.StoreChat(ctx, c.jid, c.name, c.last); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []StoredMessage{
		{ID: "a1", ChatJID: adaJID, Sender: "12025550123", Content: "Hello about Dexoverse", Timestamp: at(0)},
		{ID: "a2", ChatJID: adaJID, Sender: "me", Content: "Hi Ada", Timestamp: at(10), IsFromMe: true},
		{ID: "a3", ChatJID: adaJID, Sender: "12025550123", Content: "Dexoverse needs auth tests", Timestamp: at(20)},
		{ID: "a4", ChatJID: adaJID, Sender: "12025550123", Content: "", Timestamp: at(30), MediaType: "document", Filename: "spec.pdf", DirectPath: "/v/t62/spec.enc?x=1"},
		{ID: "b1", ChatJID: bobJID, Sender: "12025550199", Content: "Lunch?", Timestamp: at(10)},
		{ID: "g1", ChatJID: teamJID, Sender: "12025550123", Content: "Team update from Ada", Timestamp: at(35)},
		{ID: "g2", ChatJID: teamJID, Sender: "12025550199", Content: "100% done", Timestamp: at(40)},
		{ID: "dup", ChatJID: bobJID, Sender: "12025550199", Content: "same id, other chat", Timestamp: at(5)},
		{ID: "dup", ChatJID: teamJID, Sender: "12025550199", Content: "same id, team chat", Timestamp: at(36)},
	} {
		if err := store.StoreMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func read[T any](t *testing.T, store *MessageStore, tool, request string) T {
	t.Helper()
	result, err := store.Read(context.Background(), tool, json.RawMessage(request))
	if err != nil {
		t.Fatalf("%s %s: %v", tool, request, err)
	}
	typed, ok := result.(T)
	if !ok {
		t.Fatalf("%s returned %T", tool, result)
	}
	return typed
}

func ids(messages []MessageRecord) string {
	var out []string
	for _, m := range messages {
		out = append(out, m.ID)
	}
	return strings.Join(out, ",")
}

func TestStoreListMessages(t *testing.T) {
	store := seededStore(t)
	cases := map[string]string{
		`{"chat_jid":"` + adaJID + `","include_context":false}`:                                    "a4,a3,a2,a1",
		`{"query":"dexoverse","include_context":false}`:                                            "a3,a1",
		`{"sender_phone_number":"12025550199","include_context":false}`:                            "g2,dup,b1,dup",
		`{"after":"2026-03-01T10:15:00Z","before":"2026-03-01T10:36:00Z","include_context":false}`: "g1,a4,a3",
		`{"chat_jid":"` + adaJID + `","limit":2,"page":1,"include_context":false}`:                 "a2,a1",
		`{"query":"Lunch"}`: "dup,b1",
		`{"query":"auth tests","context_before":1,"context_after":1}`: "a2,a3,a4",
		`{"query":"auth tests","context_before":0,"context_after":0}`: "a3",
	}
	for request, want := range cases {
		if got := ids(read[[]MessageRecord](t, store, "list_messages", request)); got != want {
			t.Errorf("%s: got %s, want %s", request, got, want)
		}
	}
	messages := read[[]MessageRecord](t, store, "list_messages", `{"chat_jid":"`+adaJID+`","limit":1,"include_context":false}`)
	m := messages[0]
	if m.ChatName != "Ada" || m.SenderName != "Ada" || m.MediaType != "document" || m.Filename != "spec.pdf" || !m.Timestamp.Equal(at(30)) {
		t.Fatalf("message record: %+v", m)
	}
	if got := read[[]MessageRecord](t, store, "list_messages", `{"query":"Hi Ada","include_context":false}`); got[0].SenderName != "" || !got[0].IsFromMe {
		t.Fatalf("own message: %+v", got[0])
	}
}

func TestStoreListChats(t *testing.T) {
	store := seededStore(t)
	chats := read[[]ChatRecord](t, store, "list_chats", `{}`)
	if len(chats) != 4 || chats[0].JID != teamJID || chats[0].LastMessage == nil || chats[0].LastMessage.ID != "g2" {
		t.Fatalf("default list_chats: %+v", chats)
	}
	// Upstream crashed here: the last-message columns were selected without the join.
	noLast := read[[]ChatRecord](t, store, "list_chats", `{"include_last_message":false}`)
	if len(noLast) != 4 || noLast[0].LastMessage != nil {
		t.Fatalf("include_last_message false: %+v", noLast)
	}
	byName := read[[]ChatRecord](t, store, "list_chats", `{"sort_by":"name","include_last_message":false}`)
	if byName[0].Name != "Ada" || byName[3].Name != "Team 50% done" {
		t.Fatalf("sort by name: %v, %v", byName[0].Name, byName[3].Name)
	}
	// A literal % must not act as a wildcard.
	if got := read[[]ChatRecord](t, store, "list_chats", `{"query":"50%"}`); len(got) != 1 || got[0].JID != teamJID {
		t.Fatalf("query with %%: %+v", got)
	}
	chat := read[*ChatRecord](t, store, "get_chat", `{"chat_jid":"`+adaJID+`"}`)
	if chat == nil || chat.Name != "Ada" || chat.LastMessage.ID != "a4" || !chat.LastMessageTime.Equal(at(30)) {
		t.Fatalf("get_chat: %+v", chat)
	}
	if chat := read[*ChatRecord](t, store, "get_chat", `{"chat_jid":"`+bobJID+`","include_last_message":false}`); chat.LastMessage != nil {
		t.Fatalf("get_chat without last message: %+v", chat)
	}
	if chat := read[*ChatRecord](t, store, "get_chat", `{"chat_jid":"19999999999@s.whatsapp.net"}`); chat != nil {
		t.Fatalf("unknown chat: %+v", chat)
	}
}

func TestStoreContacts(t *testing.T) {
	store := seededStore(t)
	contacts := read[[]ContactRecord](t, store, "search_contacts", `{"query":"ada"}`)
	if len(contacts) != 2 || contacts[0].JID != adaJID || contacts[0].PhoneNumber != "12025550123" {
		t.Fatalf("search_contacts: %+v", contacts)
	}
	if groups := read[[]ContactRecord](t, store, "search_contacts", `{"query":"Team"}`); len(groups) != 0 {
		t.Fatalf("groups are not contacts: %+v", groups)
	}
	// Upstream matched the number as a substring and could return the wrong person.
	direct := read[*ChatRecord](t, store, "get_direct_chat_by_contact", `{"sender_phone_number":"12025550123"}`)
	if direct == nil || direct.JID != adaJID {
		t.Fatalf("get_direct_chat_by_contact: %+v", direct)
	}
	if direct := read[*ChatRecord](t, store, "get_direct_chat_by_contact", `{"sender_phone_number":"2025550123"}`); direct != nil {
		t.Fatalf("partial number matched %s", direct.JID)
	}
	chats := read[[]ChatRecord](t, store, "get_contact_chats", `{"jid":"`+adaJID+`"}`)
	if len(chats) != 2 || chats[0].JID != teamJID || chats[1].JID != adaJID {
		t.Fatalf("get_contact_chats: %+v", chats)
	}
	last := read[*MessageRecord](t, store, "get_last_interaction", `{"jid":"`+adaJID+`"}`)
	if last == nil || last.ID != "g1" {
		t.Fatalf("get_last_interaction: %+v", last)
	}
}

func TestStoreMessageContext(t *testing.T) {
	store := seededStore(t)
	context := read[MessageContextRecord](t, store, "get_message_context", `{"message_id":"a3","chat_jid":"`+adaJID+`","before":2,"after":5}`)
	if context.Message.ID != "a3" || ids(context.Before) != "a2,a1" || ids(context.After) != "a4" {
		t.Fatalf("context: %+v", context)
	}
	if got := read[MessageContextRecord](t, store, "get_message_context", `{"message_id":"a3"}`); got.Message.ID != "a3" || len(got.Before) != 2 {
		t.Fatalf("unique id without chat: %+v", got)
	}
	if _, err := store.Read(context2(), "get_message_context", json.RawMessage(`{"message_id":"dup"}`)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ambiguous id: %v", err)
	}
	if got := read[MessageContextRecord](t, store, "get_message_context", `{"message_id":"dup","chat_jid":"`+teamJID+`","before":0,"after":0}`); got.Message.Content != "same id, team chat" {
		t.Fatalf("id disambiguated by chat: %+v", got.Message)
	}
	if _, err := store.Read(context2(), "get_message_context", json.RawMessage(`{"message_id":"nope"}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func context2() context.Context { return context.Background() }

func TestStoreRejectsBadRequests(t *testing.T) {
	store := seededStore(t)
	for tool, request := range map[string]string{
		"drop_table":                 `{}`,
		"list_chats":                 `{"sort_by":"jid"}`,
		"list_messages":              `{"limit":0}`,
		"search_contacts":            `{"query":"` + strings.Repeat("a", 1025) + `"}`,
		"get_contact_chats":          `{"jid":"x","limit":101}`,
		"get_message_context":        `{"message_id":"a3","before":11}`,
		"get_chat":                   `{"chat_jid":"` + adaJID + `","extra":1}`,
		"get_direct_chat_by_contact": `{"sender_phone_number":"+1 202"}`,
	} {
		if _, err := store.Read(context.Background(), tool, json.RawMessage(request)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s %s: %v", tool, request, err)
		}
	}
	for _, request := range []string{`{"after":"yesterday"}`, `{"page":10001}`} {
		if _, err := store.Read(context.Background(), "list_messages", json.RawMessage(request)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("list_messages %s: %v", request, err)
		}
	}
}

func TestStoreDuplicateMessages(t *testing.T) {
	store := seededStore(t)
	ctx := context.Background()
	store.StoreMessage(ctx, StoredMessage{ID: "a1", ChatJID: adaJID, Sender: "12025550123", Content: "edited", Timestamp: at(0)})
	got := read[[]MessageRecord](t, store, "list_messages", `{"chat_jid":"`+adaJID+`","include_context":false}`)
	if ids(got) != "a4,a3,a2,a1" || got[3].Content != "edited" {
		t.Fatalf("duplicate sync: %s %+v", ids(got), got[3])
	}
	if err := store.StoreMessage(ctx, StoredMessage{ID: "empty", ChatJID: adaJID, Timestamp: at(50)}); err != nil {
		t.Fatal(err)
	}
	if got := read[[]MessageRecord](t, store, "list_messages", `{"chat_jid":"`+adaJID+`","include_context":false}`); len(got) != 4 {
		t.Fatal("an empty message without media was stored")
	}
}

func TestStoreTimestampFormatsAndLegacySchema(t *testing.T) {
	dir := privateDataDir(t)
	legacy, err := sql.Open("sqlite", filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	// The upstream schema, without direct_path, with values in formats older writers produced.
	_, err = legacy.Exec(messageSchemaV0 + `
		INSERT INTO chats VALUES ('` + adaJID + `', 'Ada', '2026-03-01 10:00:00+01:00');
		INSERT INTO chats VALUES ('` + bobJID + `', 'Bob', NULL);
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('old1', '` + adaJID + `', '12025550123', 'legacy one', '2026-03-01 09:00:00', 0);
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('old2', '` + adaJID + `', '12025550123', 'legacy two', '2026-03-01T09:30:00Z', 0);`)
	legacy.Close()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMessageStore(dir)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	defer store.Close()
	chats := read[[]ChatRecord](t, store, "list_chats", `{"sort_by":"name"}`)
	if len(chats) != 2 || chats[1].LastMessageTime != nil || chats[0].LastMessageTime == nil {
		t.Fatalf("legacy chats: %+v", chats)
	}
	if got := ids(read[[]MessageRecord](t, store, "list_messages", `{"chat_jid":"`+adaJID+`","include_context":false}`)); got != "old2,old1" {
		t.Fatalf("legacy messages: %s", got)
	}
	if err := store.StoreMessage(context.Background(), StoredMessage{ID: "new", ChatJID: adaJID, Sender: "1", Content: "x", Timestamp: at(0), DirectPath: "/v/x"}); err != nil {
		t.Fatalf("store after migration: %v", err)
	}
	if info, err := store.GetMediaInfo(context.Background(), "new", adaJID); err != nil || info.DirectPath != "/v/x" {
		t.Fatalf("direct path not persisted: %+v %v", info, err)
	}
}

// messageSchemaV0 is the upstream schema, before direct_path was added.
const messageSchemaV0 = `
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
	id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
	media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB, file_length INTEGER,
	PRIMARY KEY (id, chat_jid), FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);
`

func TestStoreUnusualDataDir(t *testing.T) {
	name := "Ada's data #1 ?50% & more"
	if runtime.GOOS == "windows" {
		name = strings.ReplaceAll(name, "?", "") // not allowed in Windows file names
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	store, err := OpenMessageStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.StoreChat(context.Background(), adaJID, "Ada", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "messages.db")); err != nil {
		t.Fatalf("database not created inside the directory: %v", err)
	}
	var fk int
	store.db.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	if fk != 1 {
		t.Fatal("foreign keys are off")
	}
}
