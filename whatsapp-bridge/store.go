package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
)

const messageSchema = `
CREATE TABLE IF NOT EXISTS chats (
	jid TEXT PRIMARY KEY,
	name TEXT,
	last_message_time TIMESTAMP
);

CREATE TABLE IF NOT EXISTS messages (
	id TEXT,
	chat_jid TEXT,
	sender TEXT,
	content TEXT,
	timestamp TIMESTAMP,
	is_from_me BOOLEAN,
	media_type TEXT,
	filename TEXT,
	url TEXT,
	media_key BLOB,
	file_sha256 BLOB,
	file_enc_sha256 BLOB,
	file_length INTEGER,
	direct_path TEXT,
	PRIMARY KEY (id, chat_jid),
	FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);
`

const messageIndexes = `
CREATE INDEX IF NOT EXISTS idx_messages_chat_time ON messages(chat_jid, timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_time ON messages(timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender);
CREATE INDEX IF NOT EXISTS idx_chats_last ON chats(last_message_time);
`

// Timestamps are written in UTC in the driver's own format, so string order is time order.
const timestampFormat = "2006-01-02 15:04:05.999999999-07:00"

const (
	defaultLimit  = 20
	maxLimit      = 100
	maxPage       = 10000
	maxContext    = 10
	maxQueryBytes = 1024
)

var phoneNumber = regexp.MustCompile(`^[1-9][0-9]{5,14}$`)

// MessageStore is the bridge's message history database.
type MessageStore struct {
	db *sql.DB
}

type StoredMessage struct {
	ID, ChatJID, Sender, Content string
	Timestamp                    time.Time
	IsFromMe                     bool
	MediaType, Filename          string
	URL, DirectPath              string
	MediaKey                     []byte
	FileSHA256, FileEncSHA256    []byte
	FileLength                   uint64
}

type MediaInfo struct {
	MediaType, Filename, URL, DirectPath string
	MediaKey, FileSHA256, FileEncSHA256  []byte
	FileLength                           uint64
}

type MessageRecord struct {
	ID         string    `json:"id"`
	ChatJID    string    `json:"chat_jid"`
	ChatName   string    `json:"chat_name,omitempty"`
	Sender     string    `json:"sender"`
	SenderName string    `json:"sender_name,omitempty"`
	Content    string    `json:"content"`
	MediaType  string    `json:"media_type,omitempty"`
	Filename   string    `json:"filename,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
	IsFromMe   bool      `json:"is_from_me"`
}

type ChatRecord struct {
	JID             string         `json:"jid"`
	Name            string         `json:"name"`
	LastMessageTime *time.Time     `json:"last_message_time"`
	LastMessage     *MessageRecord `json:"last_message,omitempty"`
}

type ContactRecord struct {
	JID         string `json:"jid"`
	PhoneNumber string `json:"phone_number"`
	Name        string `json:"name"`
}

type MessageContextRecord struct {
	Message MessageRecord   `json:"message"`
	Before  []MessageRecord `json:"before"`
	After   []MessageRecord `json:"after"`
}

// Request types of the read tools. Pointers tell an omitted field from an explicit zero or false.
type (
	ContactFilter struct {
		Query string `json:"query"`
	}
	MessageFilter struct {
		After             string `json:"after"`
		Before            string `json:"before"`
		SenderPhoneNumber string `json:"sender_phone_number"`
		ChatJID           string `json:"chat_jid"`
		Query             string `json:"query"`
		Limit             *int   `json:"limit"`
		Page              *int   `json:"page"`
		IncludeContext    *bool  `json:"include_context"`
		ContextBefore     *int   `json:"context_before"`
		ContextAfter      *int   `json:"context_after"`
	}
	ChatFilter struct {
		Query              string `json:"query"`
		Limit              *int   `json:"limit"`
		Page               *int   `json:"page"`
		IncludeLastMessage *bool  `json:"include_last_message"`
		SortBy             string `json:"sort_by"`
	}
	ChatRequest struct {
		ChatJID            string `json:"chat_jid"`
		IncludeLastMessage *bool  `json:"include_last_message"`
	}
	DirectChatRequest struct {
		SenderPhoneNumber string `json:"sender_phone_number"`
	}
	ContactChatsRequest struct {
		JID   string `json:"jid"`
		Limit *int   `json:"limit"`
		Page  *int   `json:"page"`
	}
	InteractionRequest struct {
		JID string `json:"jid"`
	}
	ContextRequest struct {
		MessageID string `json:"message_id"`
		ChatJID   string `json:"chat_jid"`
		Before    *int   `json:"before"`
		After     *int   `json:"after"`
	}
)

// OpenMessageStore opens messages.db in the private data directory, creating or migrating it.
func OpenMessageStore(dataDir string) (*MessageStore, error) {
	if err := EnsurePrivateDir(dataDir); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "messages.db")
	if strings.ContainsAny(path, "?#") {
		return nil, errors.New("the data directory path must not contain '?' or '#'")
	}
	if err := EnsurePrivateFile(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open message database: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &MessageStore{db: db}, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(messageSchema); err != nil {
		return fmt.Errorf("create message tables: %w", err)
	}
	var hasDirectPath bool
	if err := db.QueryRow(`SELECT COUNT(*) > 0 FROM pragma_table_info('messages') WHERE name = 'direct_path'`).Scan(&hasDirectPath); err != nil {
		return err
	}
	if !hasDirectPath {
		if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN direct_path TEXT`); err != nil {
			return fmt.Errorf("add direct_path: %w", err)
		}
	}
	if _, err := db.Exec(messageIndexes); err != nil {
		return fmt.Errorf("create indexes: %w", err)
	}
	return nil
}

func (store *MessageStore) Close() error {
	return store.db.Close()
}

func (store *MessageStore) StoreChat(ctx context.Context, jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
		 ON CONFLICT(jid) DO UPDATE SET name = excluded.name,
		   last_message_time = MAX(COALESCE(chats.last_message_time, ''), excluded.last_message_time)`,
		jid, name, formatTimestamp(lastMessageTime))
	return err
}

// StoreMessage inserts or replaces a message. Messages with neither text nor media are skipped.
func (store *MessageStore) StoreMessage(ctx context.Context, m StoredMessage) error {
	if m.Content == "" && m.MediaType == "" {
		return nil
	}
	_, err := store.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO messages
		 (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, direct_path)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ChatJID, m.Sender, m.Content, formatTimestamp(m.Timestamp), m.IsFromMe, m.MediaType, m.Filename,
		m.URL, m.MediaKey, m.FileSHA256, m.FileEncSHA256, m.FileLength, m.DirectPath)
	return err
}

func (store *MessageStore) chatName(ctx context.Context, jid string) (string, error) {
	var name sql.NullString
	err := store.db.QueryRowContext(ctx, `SELECT name FROM chats WHERE jid = ?`, jid).Scan(&name)
	return name.String, err
}

func (store *MessageStore) GetMediaInfo(ctx context.Context, id, chatJID string) (MediaInfo, error) {
	var info MediaInfo
	var mediaType, filename, url, directPath sql.NullString
	var length sql.NullInt64
	err := store.db.QueryRowContext(ctx,
		`SELECT media_type, filename, url, direct_path, media_key, file_sha256, file_enc_sha256, file_length
		 FROM messages WHERE id = ? AND chat_jid = ?`, id, chatJID).
		Scan(&mediaType, &filename, &url, &directPath, &info.MediaKey, &info.FileSHA256, &info.FileEncSHA256, &length)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaInfo{}, ErrNotFound
	}
	info.MediaType, info.Filename, info.URL, info.DirectPath = mediaType.String, filename.String, url.String, directPath.String
	if length.Int64 > 0 {
		info.FileLength = uint64(length.Int64)
	}
	return info, err
}

// Read answers one of the eight read tools. Unknown tools or arguments fail with ErrInvalidRequest.
func (store *MessageStore) Read(ctx context.Context, tool string, request json.RawMessage) (any, error) {
	switch tool {
	case "search_contacts":
		return readWith(request, func(f ContactFilter) (any, error) { return store.SearchContacts(ctx, f) })
	case "list_messages":
		return readWith(request, func(f MessageFilter) (any, error) { return store.ListMessages(ctx, f) })
	case "list_chats":
		return readWith(request, func(f ChatFilter) (any, error) { return store.ListChats(ctx, f) })
	case "get_chat":
		return readWith(request, func(r ChatRequest) (any, error) { return store.GetChat(ctx, r) })
	case "get_direct_chat_by_contact":
		return readWith(request, func(r DirectChatRequest) (any, error) { return store.GetDirectChat(ctx, r.SenderPhoneNumber) })
	case "get_contact_chats":
		return readWith(request, func(r ContactChatsRequest) (any, error) { return store.GetContactChats(ctx, r) })
	case "get_last_interaction":
		return readWith(request, func(r InteractionRequest) (any, error) { return store.GetLastInteraction(ctx, r.JID) })
	case "get_message_context":
		return readWith(request, func(r ContextRequest) (any, error) { return store.GetMessageContext(ctx, r) })
	}
	return nil, ErrInvalidRequest
}

func readWith[T any](request json.RawMessage, query func(T) (any, error)) (any, error) {
	var args T
	decoder := json.NewDecoder(bytes.NewReader(request))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return nil, ErrInvalidRequest
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrInvalidRequest
	}
	return query(args)
}

// messageColumns selects a MessageRecord; sender names come from the sender's direct chat.
const messageColumns = `m.id, m.chat_jid, COALESCE(c.name, ''), COALESCE(m.sender, ''),
	CASE WHEN m.is_from_me THEN '' ELSE COALESCE(s.name, '') END,
	COALESCE(m.content, ''), COALESCE(m.media_type, ''), COALESCE(m.filename, ''), m.timestamp, COALESCE(m.is_from_me, 0)`

const messageFrom = ` FROM messages m
	LEFT JOIN chats c ON c.jid = m.chat_jid
	LEFT JOIN chats s ON s.jid = m.sender || '@s.whatsapp.net'`

func (store *MessageStore) SearchContacts(ctx context.Context, f ContactFilter) ([]ContactRecord, error) {
	if len(f.Query) > maxQueryBytes {
		return nil, ErrInvalidRequest
	}
	rows, err := store.db.QueryContext(ctx,
		`SELECT jid, COALESCE(name, '') FROM chats
		 WHERE (LOWER(name) LIKE LOWER(?) ESCAPE '\' OR jid LIKE ? ESCAPE '\') AND jid NOT LIKE '%@g.us'
		 ORDER BY name, jid LIMIT 50`, likePattern(f.Query), likePattern(f.Query))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	contacts := []ContactRecord{}
	for rows.Next() {
		var c ContactRecord
		if err := rows.Scan(&c.JID, &c.Name); err != nil {
			return nil, err
		}
		if user, server, _ := strings.Cut(c.JID, "@"); server == "s.whatsapp.net" {
			c.PhoneNumber = user
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

func (store *MessageStore) ListMessages(ctx context.Context, f MessageFilter) ([]MessageRecord, error) {
	limit, page, err := paging(f.Limit, f.Page)
	if err != nil || len(f.Query) > maxQueryBytes {
		return nil, ErrInvalidRequest
	}
	before, after := contextSize(f.ContextBefore, 1), contextSize(f.ContextAfter, 1)
	if before < 0 || after < 0 {
		return nil, ErrInvalidRequest
	}
	var where []string
	var args []any
	for _, bound := range []struct{ value, op string }{{f.After, ">"}, {f.Before, "<"}} {
		if bound.value == "" {
			continue
		}
		t, err := parseUserTime(bound.value)
		if err != nil {
			return nil, ErrInvalidRequest
		}
		where, args = append(where, "m.timestamp "+bound.op+" ?"), append(args, formatTimestamp(t))
	}
	if f.SenderPhoneNumber != "" {
		where, args = append(where, "m.sender = ?"), append(args, f.SenderPhoneNumber)
	}
	if f.ChatJID != "" {
		where, args = append(where, "m.chat_jid = ?"), append(args, f.ChatJID)
	}
	if f.Query != "" {
		where, args = append(where, `LOWER(m.content) LIKE LOWER(?) ESCAPE '\'`), append(args, likePattern(f.Query))
	}
	query := "SELECT " + messageColumns + messageFrom
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY m.timestamp DESC LIMIT ? OFFSET ?"
	matches, err := store.queryMessages(ctx, query, append(args, limit, page*limit)...)
	if err != nil || (f.IncludeContext != nil && !*f.IncludeContext) {
		return matches, err
	}
	var withContext []MessageRecord
	seen := map[[2]string]bool{}
	for _, match := range matches {
		around, err := store.surrounding(ctx, match, before, after)
		if err != nil {
			return nil, err
		}
		// Chronological order: "before" rows arrive newest first.
		var group []MessageRecord
		for i := len(around.Before) - 1; i >= 0; i-- {
			group = append(group, around.Before[i])
		}
		group = append(append(group, match), around.After...)
		for _, m := range group {
			if key := [2]string{m.ChatJID, m.ID}; !seen[key] {
				seen[key] = true
				withContext = append(withContext, m)
			}
		}
	}
	return withContext, nil
}

func (store *MessageStore) ListChats(ctx context.Context, f ChatFilter) ([]ChatRecord, error) {
	limit, page, err := paging(f.Limit, f.Page)
	if err != nil || len(f.Query) > maxQueryBytes {
		return nil, ErrInvalidRequest
	}
	order := "c.last_message_time DESC"
	switch f.SortBy {
	case "", "last_active":
	case "name":
		order = "c.name"
	default:
		return nil, ErrInvalidRequest
	}
	query := `SELECT c.jid FROM chats c`
	var args []any
	if f.Query != "" {
		query += ` WHERE LOWER(c.name) LIKE LOWER(?) ESCAPE '\' OR c.jid LIKE ? ESCAPE '\'`
		args = append(args, likePattern(f.Query), likePattern(f.Query))
	}
	query += " ORDER BY " + order + " LIMIT ? OFFSET ?"
	return store.chatsByJID(ctx, query, f.IncludeLastMessage, append(args, limit, page*limit)...)
}

func (store *MessageStore) GetChat(ctx context.Context, r ChatRequest) (*ChatRecord, error) {
	chats, err := store.chatsByJID(ctx, `SELECT jid FROM chats WHERE jid = ?`, r.IncludeLastMessage, r.ChatJID)
	if err != nil || len(chats) == 0 {
		return nil, err
	}
	return &chats[0], nil
}

// GetDirectChat finds the direct chat with exactly this phone number.
func (store *MessageStore) GetDirectChat(ctx context.Context, phone string) (*ChatRecord, error) {
	if !phoneNumber.MatchString(phone) {
		return nil, ErrInvalidRequest
	}
	return store.GetChat(ctx, ChatRequest{ChatJID: phone + "@s.whatsapp.net"})
}

func (store *MessageStore) GetContactChats(ctx context.Context, r ContactChatsRequest) ([]ChatRecord, error) {
	limit, page, err := paging(r.Limit, r.Page)
	if err != nil || r.JID == "" {
		return nil, ErrInvalidRequest
	}
	return store.chatsByJID(ctx,
		`SELECT c.jid FROM chats c
		 WHERE c.jid = ? OR EXISTS (SELECT 1 FROM messages m WHERE m.chat_jid = c.jid AND m.sender = ?)
		 ORDER BY c.last_message_time DESC LIMIT ? OFFSET ?`,
		nil, r.JID, jidUser(r.JID), limit, page*limit)
}

func (store *MessageStore) GetLastInteraction(ctx context.Context, jid string) (*MessageRecord, error) {
	if jid == "" {
		return nil, ErrInvalidRequest
	}
	messages, err := store.queryMessages(ctx,
		"SELECT "+messageColumns+messageFrom+" WHERE m.sender = ? OR m.chat_jid = ? ORDER BY m.timestamp DESC LIMIT 1",
		jidUser(jid), jid)
	if err != nil || len(messages) == 0 {
		return nil, err
	}
	return &messages[0], nil
}

// GetMessageContext returns a message with its neighbours. An ID present in several chats needs chat_jid.
func (store *MessageStore) GetMessageContext(ctx context.Context, r ContextRequest) (MessageContextRecord, error) {
	before, after := contextSize(r.Before, 5), contextSize(r.After, 5)
	if r.MessageID == "" || before < 0 || after < 0 {
		return MessageContextRecord{}, ErrInvalidRequest
	}
	query := "SELECT " + messageColumns + messageFrom + " WHERE m.id = ?"
	args := []any{r.MessageID}
	if r.ChatJID != "" {
		query, args = query+" AND m.chat_jid = ?", append(args, r.ChatJID)
	}
	matches, err := store.queryMessages(ctx, query+" LIMIT 2", args...)
	switch {
	case err != nil:
		return MessageContextRecord{}, err
	case len(matches) == 0:
		return MessageContextRecord{}, ErrNotFound
	case len(matches) > 1:
		return MessageContextRecord{}, fmt.Errorf("%w: message id is in several chats; pass chat_jid", ErrInvalidRequest)
	}
	return store.surrounding(ctx, matches[0], before, after)
}

func (store *MessageStore) surrounding(ctx context.Context, m MessageRecord, before, after int) (MessageContextRecord, error) {
	result := MessageContextRecord{Message: m, Before: []MessageRecord{}, After: []MessageRecord{}}
	if before > 0 {
		earlier, err := store.queryMessages(ctx,
			"SELECT "+messageColumns+messageFrom+" WHERE m.chat_jid = ? AND m.timestamp < ? ORDER BY m.timestamp DESC LIMIT ?",
			m.ChatJID, formatTimestamp(m.Timestamp), before)
		if err != nil {
			return result, err
		}
		result.Before = earlier
	}
	if after > 0 {
		later, err := store.queryMessages(ctx,
			"SELECT "+messageColumns+messageFrom+" WHERE m.chat_jid = ? AND m.timestamp > ? ORDER BY m.timestamp ASC LIMIT ?",
			m.ChatJID, formatTimestamp(m.Timestamp), after)
		if err != nil {
			return result, err
		}
		result.After = later
	}
	return result, nil
}

func (store *MessageStore) queryMessages(ctx context.Context, query string, args ...any) ([]MessageRecord, error) {
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []MessageRecord{}
	for rows.Next() {
		var m MessageRecord
		var timestamp sql.NullString
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.ChatName, &m.Sender, &m.SenderName, &m.Content, &m.MediaType, &m.Filename, &timestamp, &m.IsFromMe); err != nil {
			return nil, err
		}
		if t, ok := parseStoredTime(timestamp); ok {
			m.Timestamp = t
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// chatsByJID runs a query selecting chat JIDs and loads each chat, with its last message unless excluded.
func (store *MessageStore) chatsByJID(ctx context.Context, query string, includeLast *bool, args ...any) ([]ChatRecord, error) {
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var jids []string
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			rows.Close()
			return nil, err
		}
		jids = append(jids, jid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	chats := []ChatRecord{}
	for _, jid := range jids {
		chat := ChatRecord{JID: jid}
		var name, last sql.NullString
		if err := store.db.QueryRowContext(ctx, `SELECT name, last_message_time FROM chats WHERE jid = ?`, jid).Scan(&name, &last); err != nil {
			return nil, err
		}
		chat.Name = name.String
		if t, ok := parseStoredTime(last); ok {
			chat.LastMessageTime = &t
		}
		if includeLast == nil || *includeLast {
			latest, err := store.queryMessages(ctx, "SELECT "+messageColumns+messageFrom+" WHERE m.chat_jid = ? ORDER BY m.timestamp DESC LIMIT 1", jid)
			if err != nil {
				return nil, err
			}
			if len(latest) > 0 {
				chat.LastMessage = &latest[0]
			}
		}
		chats = append(chats, chat)
	}
	return chats, nil
}

func paging(limit, page *int) (int, int, error) {
	l, p := defaultLimit, 0
	if limit != nil {
		l = *limit
	}
	if page != nil {
		p = *page
	}
	if l < 1 || l > maxLimit || p < 0 || p > maxPage {
		return 0, 0, ErrInvalidRequest
	}
	return l, p, nil
}

// contextSize returns the requested size, the default when omitted, or -1 when out of range.
func contextSize(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	if *value < 0 || *value > maxContext {
		return -1
	}
	return *value
}

// likePattern matches query as a literal substring in a LIKE ... ESCAPE '\' clause.
func likePattern(query string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	return "%" + escaped + "%"
}

func jidUser(jid string) string {
	user, _, _ := strings.Cut(jid, "@")
	return user
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format(timestampFormat)
}

var storedTimeFormats = []string{timestampFormat, time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02"}

// parseStoredTime reads timestamps written by this bridge, the upstream bridge or older drivers.
func parseStoredTime(value sql.NullString) (time.Time, bool) {
	if !value.Valid {
		return time.Time{}, false
	}
	for _, layout := range storedTimeFormats {
		if t, err := time.Parse(layout, value.String); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseUserTime accepts ISO-8601 timestamps with or without a zone, and plain dates (UTC).
func parseUserTime(value string) (time.Time, error) {
	if !utf8.ValidString(value) || len(value) > 64 {
		return time.Time{}, ErrInvalidRequest
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, ErrInvalidRequest
}
