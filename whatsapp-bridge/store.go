package main

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

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
	PRIMARY KEY (id, chat_jid),
	FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);
`

// MessageStore is the bridge's message history database.
type MessageStore struct {
	db *sql.DB
}

// OpenMessageStore opens messages.db in the private data directory, creating it if needed.
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
	if _, err := db.Exec(messageSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create message tables: %w", err)
	}
	return &MessageStore{db: db}, nil
}

func (store *MessageStore) Close() error {
	return store.db.Close()
}
