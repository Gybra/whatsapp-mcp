package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// ErrSendFailed means the message was definitely not sent, for example because the upload failed.
var ErrSendFailed = errors.New("send_failed")

const (
	eventTimeout     = 30 * time.Second
	maxDirectPathLen = 2048
)

type RuntimeConfig struct {
	DataDir string
	Logger  *slog.Logger
}

// WASendTransport is the part of the WhatsApp client that sends user-visible messages.
type WASendTransport interface {
	Upload(ctx context.Context, plaintext []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

// WhatsAppRuntime owns the WhatsApp session. It stores incoming messages and
// sends only drafts that the approval manager has consumed.
type WhatsAppRuntime struct {
	cfg       RuntimeConfig
	store     *MessageStore
	container *sqlstore.Container
	client    *whatsmeow.Client
	logger    *slog.Logger

	// Network-facing operations, replaced in tests.
	transport   WASendTransport
	connected   func() bool
	ownUser     func() string
	resolvePN   func(context.Context, types.JID) (types.JID, error)
	contactName func(context.Context, types.JID) string
	groupName   func(context.Context, types.JID) string
	download    func(context.Context, whatsmeow.DownloadableMessage) ([]byte, error)

	mu    sync.Mutex
	state string
}

func NewWhatsAppRuntime(cfg RuntimeConfig, store *MessageStore) (*WhatsAppRuntime, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	session := filepath.Join(cfg.DataDir, "whatsapp.db")
	if err := EnsurePrivateFile(session); err != nil {
		return nil, err
	}
	waLogger := newRedactingLogger(logger)
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite3", session+"?_foreign_keys=on", waLogger.Sub("Database"))
	if err != nil {
		return nil, fmt.Errorf("open session database: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		container.Close()
		return nil, fmt.Errorf("load session: %w", err)
	}
	client := whatsmeow.NewClient(device, waLogger.Sub("Client"))
	client.SetMediaHTTPClient(newMediaHTTPClient())
	r := &WhatsAppRuntime{
		cfg: cfg, store: store, container: container, client: client, logger: logger,
		transport: client,
		connected: client.IsConnected,
		ownUser: func() string {
			if id := client.Store.ID; id != nil {
				return id.User
			}
			return ""
		},
		// The LID and contact stores exist only once the device is paired.
		resolvePN: func(ctx context.Context, jid types.JID) (types.JID, error) {
			if client.Store.LIDs == nil {
				return types.EmptyJID, nil
			}
			return client.Store.LIDs.GetPNForLID(ctx, jid)
		},
		contactName: func(ctx context.Context, jid types.JID) string {
			if client.Store.Contacts == nil {
				return ""
			}
			contact, err := client.Store.Contacts.GetContact(ctx, jid)
			if err != nil {
				return ""
			}
			return firstNonEmpty(contact.FullName, contact.PushName, contact.BusinessName)
		},
		groupName: func(ctx context.Context, jid types.JID) string {
			info, err := client.GetGroupInfo(ctx, jid)
			if err != nil {
				return ""
			}
			return info.Name
		},
		download: client.Download,
		state:    "offline",
	}
	if device.ID == nil {
		r.state = "unpaired"
	}
	// Reconnect after network problems, never after WhatsApp ended the session.
	client.EnableAutoReconnect = true
	client.AutoReconnectHook = func(error) bool { return r.Status().State != "action_required" }
	client.AddEventHandler(r.handleEvent)
	return r, nil
}

// Connect connects an existing session. With onQR it pairs a new device instead,
// passing each QR code to onQR; without it an unpaired runtime stays unpaired.
func (r *WhatsAppRuntime) Connect(ctx context.Context, onQR func(code string)) error {
	if r.client.Store.ID != nil {
		r.setState("connecting")
		return r.client.Connect()
	}
	if onQR == nil {
		return ErrNotPaired
	}
	codes, err := r.client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := r.client.Connect(); err != nil {
		return err
	}
	for item := range codes {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			onQR(item.Code)
		case whatsmeow.QRChannelSuccess.Event:
			return nil
		default:
			r.client.Disconnect()
			return fmt.Errorf("pairing did not complete (%s)", item.Event)
		}
	}
	return errors.New("pairing did not complete")
}

func (r *WhatsAppRuntime) Status() RuntimeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RuntimeStatus{State: r.state, Connected: r.state == "online"}
}

func (r *WhatsAppRuntime) setState(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != "action_required" || state == "stopped" {
		r.state = state
	}
}

func (r *WhatsAppRuntime) Close() error {
	r.setState("stopped")
	r.client.Disconnect()
	return r.container.Close()
}

func (r *WhatsAppRuntime) Read(ctx context.Context, tool string, request json.RawMessage) (any, error) {
	return r.store.Read(ctx, tool, request)
}

func (r *WhatsAppRuntime) handleEvent(evt any) {
	switch e := evt.(type) {
	case *events.Connected:
		r.setState("online")
	case *events.Disconnected:
		r.setState("offline")
	case *events.LoggedOut:
		r.endSession("WhatsApp logged this device out; pair it again with whatsapp-mcp setup")
	case *events.StreamReplaced:
		r.endSession("another client took over this WhatsApp session")
	case *events.TemporaryBan:
		r.endSession(fmt.Sprintf("WhatsApp temporarily banned this account (expires in %s)", e.Expire))
	case *events.ClientOutdated:
		r.endSession("WhatsApp rejected this client version; update whatsapp-mcp")
	case *events.Message:
		ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
		defer cancel()
		r.storeMessage(ctx, e, "")
	case *events.HistorySync:
		r.storeHistory(e)
	}
}

// endSession stops reconnecting after WhatsApp ended the session. Session data is
// left in place: the user decides whether to pair again.
func (r *WhatsAppRuntime) endSession(reason string) {
	r.mu.Lock()
	r.state = "action_required"
	r.mu.Unlock()
	r.logger.Warn("WhatsApp session needs attention: " + reason)
	go r.client.Disconnect()
}

func (r *WhatsAppRuntime) storeHistory(e *events.HistorySync) {
	for _, conversation := range e.Data.GetConversations() {
		chat, err := types.ParseJID(conversation.GetID())
		if err != nil {
			continue
		}
		name := firstNonEmpty(conversation.GetDisplayName(), conversation.GetName())
		for _, entry := range conversation.GetMessages() {
			message, err := r.client.ParseWebMessage(chat, entry.GetMessage())
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
			r.storeMessage(ctx, message, name)
			cancel()
		}
	}
}

// storeMessage records a message under its canonical chat. Content is never logged.
func (r *WhatsAppRuntime) storeMessage(ctx context.Context, e *events.Message, nameHint string) {
	content := messageText(e.Message)
	media := mediaFields(e.Message)
	if content == "" && media.MediaType == "" {
		return
	}
	chat := r.canonical(ctx, e.Info.Chat)
	sender := r.canonical(ctx, e.Info.Sender).User
	if e.Info.IsFromMe {
		sender = firstNonEmpty(r.ownUser(), sender)
	}
	name := firstNonEmpty(nameHint, r.chatName(ctx, chat, e.Info.PushName))
	if err := r.store.StoreChat(ctx, chat.String(), name, e.Info.Timestamp); err != nil {
		r.logger.Warn("could not store a chat")
		return
	}
	media.ID, media.ChatJID, media.Sender, media.Content = e.Info.ID, chat.String(), sender, content
	media.Timestamp, media.IsFromMe = e.Info.Timestamp, e.Info.IsFromMe
	if err := r.store.StoreMessage(ctx, media); err != nil {
		r.logger.Warn("could not store a message")
	}
}

// canonical maps a LID to its phone-number JID when WhatsApp has shared the mapping,
// so one contact does not split into two chats.
func (r *WhatsAppRuntime) canonical(ctx context.Context, jid types.JID) types.JID {
	jid = jid.ToNonAD()
	if jid.Server == types.HiddenUserServer {
		if pn, err := r.resolvePN(ctx, jid); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return jid
}

func (r *WhatsAppRuntime) chatName(ctx context.Context, chat types.JID, pushName string) string {
	if name, err := r.store.chatName(ctx, chat.String()); err == nil && name != "" {
		return name
	}
	if chat.Server == types.GroupServer {
		return firstNonEmpty(r.groupName(ctx, chat), "Group "+chat.User)
	}
	return firstNonEmpty(r.contactName(ctx, chat), pushName, chat.User)
}

func messageText(m *waE2E.Message) string {
	return firstNonEmpty(m.GetConversation(), m.GetExtendedTextMessage().GetText(), m.GetImageMessage().GetCaption(),
		m.GetVideoMessage().GetCaption(), m.GetDocumentMessage().GetCaption())
}

// mediaFields extracts what is needed to download an attachment later.
func mediaFields(m *waE2E.Message) StoredMessage {
	type downloadable interface {
		GetURL() string
		GetDirectPath() string
		GetMediaKey() []byte
		GetFileSHA256() []byte
		GetFileEncSHA256() []byte
		GetFileLength() uint64
	}
	var kind, name string
	var media downloadable
	switch {
	case m.GetImageMessage() != nil:
		kind, name, media = "image", "image.jpg", m.GetImageMessage()
	case m.GetVideoMessage() != nil:
		kind, name, media = "video", "video.mp4", m.GetVideoMessage()
	case m.GetAudioMessage() != nil:
		kind, name, media = "audio", "audio.ogg", m.GetAudioMessage()
	case m.GetDocumentMessage() != nil:
		kind, name, media = "document", firstNonEmpty(m.GetDocumentMessage().GetFileName(), "document"), m.GetDocumentMessage()
	default:
		return StoredMessage{}
	}
	return StoredMessage{
		MediaType: kind, Filename: name, URL: media.GetURL(), DirectPath: media.GetDirectPath(),
		MediaKey: media.GetMediaKey(), FileSHA256: media.GetFileSHA256(), FileEncSHA256: media.GetFileEncSHA256(), FileLength: media.GetFileLength(),
	}
}

// Send delivers a draft the approval manager has consumed. Errors other than
// ErrClosed and ErrSendFailed leave it uncertain whether the message arrived.
func (r *WhatsAppRuntime) Send(ctx context.Context, draft FrozenDraft) (SendReceipt, error) {
	if !r.connected() {
		return SendReceipt{}, ErrClosed
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sendTimeout)
		defer cancel()
	}
	to, err := types.ParseJID(draft.Payload.RecipientJID)
	if err != nil {
		return SendReceipt{}, ErrSendFailed
	}
	message, record, err := r.buildMessage(ctx, draft)
	if err != nil {
		return SendReceipt{}, ErrSendFailed
	}
	resp, err := r.transport.SendMessage(ctx, to, message)
	if err != nil {
		return SendReceipt{}, fmt.Errorf("send: %w", err)
	}
	r.recordOutgoing(ctx, to, resp, record)
	return SendReceipt{Status: "sent", MessageID: resp.ID}, nil
}

func (r *WhatsAppRuntime) buildMessage(ctx context.Context, draft FrozenDraft) (*waE2E.Message, StoredMessage, error) {
	p := draft.Payload
	record := StoredMessage{Content: p.Message}
	if p.Kind == "text" {
		return &waE2E.Message{Conversation: proto.String(p.Message)}, record, nil
	}
	a := p.Attachment
	mediaType, kind := mediaTypeFor(a.MIME)
	up, err := r.transport.Upload(ctx, draft.Media, mediaType)
	if err != nil {
		return nil, record, err
	}
	record.MediaType, record.Filename, record.URL, record.DirectPath = kind, a.Name, up.URL, up.DirectPath
	record.MediaKey, record.FileSHA256, record.FileEncSHA256, record.FileLength = up.MediaKey, up.FileSHA256, up.FileEncSHA256, up.FileLength
	var caption *string
	if p.Message != "" {
		caption = proto.String(p.Message)
	}
	switch mediaType {
	case whatsmeow.MediaImage:
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: caption, Mimetype: proto.String(a.MIME), URL: &up.URL, DirectPath: &up.DirectPath,
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength}}, record, nil
	case whatsmeow.MediaVideo:
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: caption, Mimetype: proto.String(a.MIME), URL: &up.URL, DirectPath: &up.DirectPath,
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength}}, record, nil
	case whatsmeow.MediaAudio:
		seconds, _ := AnalyzeOpus(draft.Media) // validated when the draft was frozen
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String(a.MIME), URL: &up.URL, DirectPath: &up.DirectPath,
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
			Seconds: proto.Uint32(seconds), PTT: proto.Bool(p.Kind == "voice")}}, record, nil
	default:
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: caption, Mimetype: proto.String(a.MIME), FileName: proto.String(a.Name),
			Title: proto.String(a.Name), URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: &up.FileLength}}, record, nil
	}
}

func mediaTypeFor(mime string) (whatsmeow.MediaType, string) {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return whatsmeow.MediaImage, "image"
	case strings.HasPrefix(mime, "video/"):
		return whatsmeow.MediaVideo, "video"
	case strings.HasPrefix(mime, "audio/"):
		return whatsmeow.MediaAudio, "audio"
	}
	return whatsmeow.MediaDocument, "document"
}

// recordOutgoing keeps sent messages in the history; the phone does not echo them back to this device.
func (r *WhatsAppRuntime) recordOutgoing(ctx context.Context, to types.JID, resp whatsmeow.SendResponse, record StoredMessage) {
	sentAt := resp.Timestamp
	if sentAt.IsZero() {
		sentAt = time.Now()
	}
	name := r.chatName(ctx, to, "")
	record.ID, record.ChatJID, record.Sender, record.Timestamp, record.IsFromMe = resp.ID, to.String(), r.ownUser(), sentAt, true
	if err := r.store.StoreChat(ctx, to.String(), name, sentAt); err != nil {
		r.logger.Warn("could not record a sent message")
		return
	}
	if err := r.store.StoreMessage(ctx, record); err != nil {
		r.logger.Warn("could not record a sent message")
	}
}

// storedMedia lets whatsmeow download an attachment recorded in the message store.
type storedMedia struct {
	info      MediaInfo
	path      string
	mediaType whatsmeow.MediaType
}

func (m storedMedia) GetDirectPath() string             { return m.path }
func (m storedMedia) GetMediaKey() []byte               { return m.info.MediaKey }
func (m storedMedia) GetFileSHA256() []byte             { return m.info.FileSHA256 }
func (m storedMedia) GetFileEncSHA256() []byte          { return m.info.FileEncSHA256 }
func (m storedMedia) GetMediaType() whatsmeow.MediaType { return m.mediaType }

var storedMediaTypes = map[string]whatsmeow.MediaType{
	"image": whatsmeow.MediaImage, "video": whatsmeow.MediaVideo, "audio": whatsmeow.MediaAudio, "document": whatsmeow.MediaDocument,
}

// Download fetches a received attachment through WhatsApp's own media hosts and
// saves it under a generated name. Stored paths are validated before any request.
func (r *WhatsAppRuntime) Download(ctx context.Context, messageID, chatJID string) (DownloadedMedia, error) {
	info, err := r.store.GetMediaInfo(ctx, messageID, chatJID)
	if errors.Is(err, ErrNotFound) || (err == nil && info.MediaType == "") {
		return DownloadedMedia{}, ErrMediaNotFound
	}
	if err != nil {
		return DownloadedMedia{}, err
	}
	path := info.DirectPath
	if path == "" {
		if path, err = legacyDirectPath(info.URL); err != nil {
			return DownloadedMedia{}, err
		}
	}
	if err := validateDirectPath(path); err != nil {
		return DownloadedMedia{}, err
	}
	mediaType, known := storedMediaTypes[info.MediaType]
	if info.FileLength > maxMediaBytes {
		return DownloadedMedia{}, fmt.Errorf("%w: attachments are limited to 16 MiB", ErrUnsupportedMedia)
	}
	if !known || len(info.MediaKey) != 32 || len(info.FileSHA256) != 32 || len(info.FileEncSHA256) != 32 {
		return DownloadedMedia{}, fmt.Errorf("%w: this message's media cannot be downloaded", ErrMediaNotFound)
	}
	if !r.connected() {
		return DownloadedMedia{}, ErrClosed
	}
	data, err := r.download(ctx, storedMedia{info: info, path: path, mediaType: mediaType})
	if err != nil {
		return DownloadedMedia{}, fmt.Errorf("%w: download failed", ErrMediaNotFound)
	}
	return SaveDownloadedMedia(r.cfg.DataDir, info.Filename, data)
}

var directPathChars = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/?-]*$`)

// validateDirectPath accepts only a path on WhatsApp's media hosts, never a URL or another host.
func validateDirectPath(path string) error {
	if len(path) > maxDirectPathLen || strings.HasPrefix(path, "//") || !directPathChars.MatchString(path) {
		return fmt.Errorf("%w: invalid media path", ErrInvalidRequest)
	}
	return nil
}

// legacyDirectPath converts a media URL stored by the upstream bridge, if it points at WhatsApp.
func legacyDirectPath(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !strings.HasSuffix(u.Hostname(), ".whatsapp.net") {
		return "", fmt.Errorf("%w: invalid media URL", ErrInvalidRequest)
	}
	path := u.EscapedPath()
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path, nil
}

// cappedTransport rejects media responses larger than limit, whatever length they declare.
type cappedTransport struct {
	base  http.RoundTripper
	limit int64
}

var errMediaTooLarge = fmt.Errorf("%w: attachments are limited to 16 MiB", ErrUnsupportedMedia)

func (t cappedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.limit {
		resp.Body.Close()
		return nil, errMediaTooLarge
	}
	resp.Body = &cappedBody{ReadCloser: resp.Body, remaining: t.limit}
	return resp, nil
}

type cappedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return n, errMediaTooLarge
	}
	return n, err
}

func newMediaHTTPClient() *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	// Encrypted media is the plaintext plus up to 16 bytes of padding and a 10-byte MAC.
	return &http.Client{Timeout: 2 * time.Minute, Transport: cappedTransport{base: base, limit: maxMediaBytes + 32}}
}

// redactingLogger passes the WhatsApp library's warnings and errors to slog with
// phone numbers, JIDs and token-like strings removed. Info and debug output is dropped.
type redactingLogger struct {
	logger *slog.Logger
	module string
}

var sensitiveText = regexp.MustCompile(`[0-9]{6,}|[A-Za-z0-9+/_=-]{24,}`)

func newRedactingLogger(logger *slog.Logger) waLog.Logger {
	return redactingLogger{logger: logger, module: "whatsmeow"}
}

func (l redactingLogger) Warnf(msg string, args ...any) {
	l.logger.Warn(sensitiveText.ReplaceAllString(fmt.Sprintf(msg, args...), "[redacted]"), "module", l.module)
}

func (l redactingLogger) Errorf(msg string, args ...any) {
	l.logger.Error(sensitiveText.ReplaceAllString(fmt.Sprintf(msg, args...), "[redacted]"), "module", l.module)
}

func (redactingLogger) Infof(string, ...any)  {}
func (redactingLogger) Debugf(string, ...any) {}

func (l redactingLogger) Sub(module string) waLog.Logger {
	return redactingLogger{logger: l.logger, module: l.module + "/" + module}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
