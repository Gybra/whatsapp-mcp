package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

var (
	ErrMediaNotFound    = errors.New("media_not_found")
	ErrUnsupportedMedia = errors.New("unsupported_media")
	ErrSecretRejected   = errors.New("secret_rejected")
)

const (
	outboxName    = "outbox"
	downloadsName = "downloads"
)

// DownloadedMedia is a received attachment saved under the private downloads directory.
type DownloadedMedia struct {
	AttachmentView
	Path string `json:"path"`
}

type sendableType struct {
	extensions []string
	mime       string
	matches    func([]byte) bool
}

// sendableTypes lists the first-release attachment formats. The extension and the content must agree.
var sendableTypes = []sendableType{
	{[]string{".jpg", ".jpeg"}, "image/jpeg", func(b []byte) bool { return bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}) }},
	{[]string{".png"}, "image/png", func(b []byte) bool { return bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) }},
	{[]string{".gif"}, "image/gif", func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
	}},
	{[]string{".webp"}, "image/webp", func(b []byte) bool { return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" }},
	{[]string{".mp4"}, "video/mp4", func(b []byte) bool { return len(b) >= 12 && string(b[4:8]) == "ftyp" }},
	{[]string{".pdf"}, "application/pdf", func(b []byte) bool { return bytes.HasPrefix(b, []byte("%PDF-")) }},
	{[]string{".txt"}, "text/plain", func(b []byte) bool { return utf8.Valid(b) && !bytes.ContainsRune(b, 0) }},
	{[]string{".ogg", ".opus"}, opusMIME, func(b []byte) bool { _, err := AnalyzeOpus(b); return err == nil }},
}

var (
	secretNames      = []string{".env", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "auth.json", "credentials.json", ".npmrc", ".pypirc"}
	privateKeyHeader = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	downloadExt      = regexp.MustCompile(`^\.[a-z0-9]{1,8}$`)
)

// RejectObviousSecrets refuses well-known credential files and PEM private keys.
// It is a guard against obvious mistakes, not a secret scanner: the user's review is what matters.
func RejectObviousSecrets(filename string, data []byte) error {
	base := strings.ToLower(filepath.Base(filename))
	if slices.Contains(secretNames, base) || strings.HasPrefix(base, ".env.") || privateKeyHeader.Match(data) {
		return ErrSecretRejected
	}
	return nil
}

// FreezeSend validates a proposed send and captures the exact bytes that would
// be sent. Attachments come only from regular files placed directly in the outbox.
// Nothing reaches WhatsApp here: uploads happen only after approval.
func FreezeSend(ctx context.Context, dataDir string, in SendInput) (FrozenDraft, error) {
	draft := FrozenDraft{Input: in, Payload: ApprovalPayload{RecipientJID: recipientJID(in.Recipient), Message: in.Message, Kind: in.Kind}}
	if err := RejectObviousSecrets("", []byte(in.Message)); err != nil {
		return FrozenDraft{}, err
	}
	if in.Kind == "text" {
		return draft, nil
	}
	if in.Kind == "voice" && in.Message != "" {
		return FrozenDraft{}, fmt.Errorf("%w: voice messages have no caption", ErrInvalidRequest)
	}
	if err := RejectObviousSecrets(in.MediaID, nil); err != nil {
		return FrozenDraft{}, err
	}
	if !validMediaID(in.MediaID) {
		return FrozenDraft{}, fmt.Errorf("%w: media_id must be the name of a file in the outbox", ErrInvalidRequest)
	}
	data, err := readOutboxFile(dataDir, in.MediaID)
	if err != nil {
		return FrozenDraft{}, err
	}
	if err := RejectObviousSecrets(in.MediaID, data); err != nil {
		return FrozenDraft{}, err
	}
	mime, data, err := finalMedia(ctx, dataDir, in, data)
	if err != nil {
		return FrozenDraft{}, err
	}
	draft.Media = data
	draft.Payload.Attachment = &AttachmentView{ID: in.MediaID, Name: in.MediaID, MIME: mime, Size: int64(len(data)), SHA256: sha256Hex(data)}
	return draft, nil
}

// recipientJID maps a phone number to its WhatsApp user JID; JIDs pass through.
func recipientJID(recipient string) string {
	if strings.Contains(recipient, "@") {
		return recipient
	}
	return recipient + "@s.whatsapp.net"
}

func validMediaID(id string) bool {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || strings.HasPrefix(id, ".") || strings.TrimSpace(id) != id {
		return false
	}
	return !strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '/' || r == '\\' })
}

// readOutboxFile reads one regular, singly linked file through an os.Root
// confined to the outbox, and checks the opened file is the one inspected.
func readOutboxFile(dataDir, id string) ([]byte, error) {
	outbox := filepath.Join(dataDir, outboxName)
	if err := EnsurePrivateDir(outbox); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(outbox)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrMediaNotFound
	}
	if err != nil {
		return nil, err
	}
	notPlain := fmt.Errorf("%w: only regular files copied into the outbox can be sent", ErrInvalidRequest)
	if !info.Mode().IsRegular() || linkCount(info) > 1 {
		return nil, notPlain
	}
	if info.Size() > maxMediaBytes {
		return nil, fmt.Errorf("%w: attachments are limited to 16 MiB", ErrUnsupportedMedia)
	}
	f, err := root.OpenFile(id, os.O_RDONLY|openNonBlocking, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil || !os.SameFile(info, opened) {
		return nil, notPlain
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMediaBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMediaBytes {
		return nil, fmt.Errorf("%w: attachments are limited to 16 MiB", ErrUnsupportedMedia)
	}
	return data, nil
}

func finalMedia(ctx context.Context, dataDir string, in SendInput, data []byte) (string, []byte, error) {
	ext := strings.ToLower(filepath.Ext(in.MediaID))
	if in.Kind == "file" || ext == ".ogg" || ext == ".opus" {
		for _, t := range sendableTypes {
			if slices.Contains(t.extensions, ext) && t.matches(data) && (in.Kind == "file" || t.mime == opusMIME) {
				return t.mime, data, nil
			}
		}
		return "", nil, fmt.Errorf("%w: send JPEG, PNG, GIF, WebP, MP4, PDF, UTF-8 text or Ogg Opus files", ErrUnsupportedMedia)
	}
	format := voiceInputFormat(ext, data)
	if format == "" {
		return "", nil, fmt.Errorf("%w: voice messages accept Ogg Opus, MP3, M4A or WAV files", ErrUnsupportedMedia)
	}
	tmp, err := os.MkdirTemp(dataDir, ".voice-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(tmp)
	source := filepath.Join(tmp, "input")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		return "", nil, err
	}
	out, err := ConvertVoice(ctx, source, format, tmp)
	if err != nil {
		return "", nil, err
	}
	converted, err := os.ReadFile(out)
	return opusMIME, converted, err
}

// voiceInputFormat returns the ffmpeg demuxer for a supported audio file, or "".
func voiceInputFormat(ext string, data []byte) string {
	switch {
	case ext == ".mp3" && (bytes.HasPrefix(data, []byte("ID3")) || len(data) > 1 && data[0] == 0xff && data[1]&0xe0 == 0xe0):
		return "mp3"
	case ext == ".m4a" && len(data) >= 12 && string(data[4:8]) == "ftyp":
		return "mov"
	case ext == ".wav" && len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return "wav"
	}
	return ""
}

// SaveDownloadedMedia stores a received attachment under a generated name. The
// sender's filename is kept only as metadata and never used to build the path.
func SaveDownloadedMedia(dataDir, originalName string, data []byte) (DownloadedMedia, error) {
	if len(data) > maxMediaBytes {
		return DownloadedMedia{}, fmt.Errorf("%w: attachments are limited to 16 MiB", ErrUnsupportedMedia)
	}
	dir := filepath.Join(dataDir, downloadsName)
	if err := EnsurePrivateDir(dir); err != nil {
		return DownloadedMedia{}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return DownloadedMedia{}, err
	}
	defer root.Close()
	random := make([]byte, 16)
	rand.Read(random)
	id := hex.EncodeToString(random)
	name := id
	if ext := strings.ToLower(filepath.Ext(originalName)); downloadExt.MatchString(ext) {
		name += ext
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return DownloadedMedia{}, err
	}
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		root.Remove(name)
		return DownloadedMedia{}, err
	}
	path := filepath.Join(dir, name)
	if err := EnsurePrivateFile(path); err != nil {
		return DownloadedMedia{}, err
	}
	displayName := strings.ToValidUTF8(originalName, "\uFFFD")
	if len(displayName) > 255 {
		displayName = displayName[:255]
	}
	return DownloadedMedia{
		AttachmentView: AttachmentView{ID: id, Name: displayName, MIME: http.DetectContentType(data), Size: int64(len(data)), SHA256: sha256Hex(data)},
		Path:           path,
	}, nil
}
