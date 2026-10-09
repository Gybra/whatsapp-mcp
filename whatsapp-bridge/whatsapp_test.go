package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var (
	adaPN  = types.NewJID("12025550123", types.DefaultUserServer)
	adaLID = types.NewJID("123456789012345", types.HiddenUserServer)
	team   = types.NewJID("120363025555555555", types.GroupServer)
)

type fakeTransport struct {
	uploads  [][]byte
	types    []whatsmeow.MediaType
	sent     []*waE2E.Message
	to       []types.JID
	deadline bool
	upErr    error
	sendErr  error
}

func (f *fakeTransport) Upload(ctx context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.uploads, f.types = append(f.uploads, data), append(f.types, mediaType)
	_, f.deadline = ctx.Deadline()
	return whatsmeow.UploadResponse{URL: "https://mmg.whatsapp.net/v/up.enc", DirectPath: "/v/up.enc?ccb=1", MediaKey: bytes.Repeat([]byte{1}, 32), FileSHA256: bytes.Repeat([]byte{2}, 32), FileEncSHA256: bytes.Repeat([]byte{3}, 32), FileLength: uint64(len(data))}, f.upErr
}

func (f *fakeTransport) SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.sent, f.to = append(f.sent, message), append(f.to, to)
	_, f.deadline = ctx.Deadline()
	if f.sendErr != nil {
		return whatsmeow.SendResponse{}, f.sendErr
	}
	return whatsmeow.SendResponse{ID: types.MessageID(fmt.Sprintf("3EB0SENT%d", len(f.sent))), Timestamp: t0}, nil
}

// newTestRuntime builds a real runtime with a fresh, unpaired session database. It never connects.
func newTestRuntime(t *testing.T, logs io.Writer) (*WhatsAppRuntime, *fakeTransport) {
	t.Helper()
	dir := privateDataDir(t)
	store, err := OpenMessageStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if logs == nil {
		logs = io.Discard
	}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runtime, err := NewWhatsAppRuntime(RuntimeConfig{DataDir: dir, Logger: logger}, store)
	if err != nil {
		t.Fatalf("NewWhatsAppRuntime: %v", err)
	}
	t.Cleanup(func() { runtime.Close(); store.Close() })
	transport := &fakeTransport{}
	runtime.transport = transport
	runtime.connected = func() bool { return true }
	runtime.ownUser = func() string { return "19995550100" }
	runtime.resolvePN = func(_ context.Context, jid types.JID) (types.JID, error) {
		if jid == adaLID {
			return adaPN, nil
		}
		return types.EmptyJID, nil
	}
	runtime.contactName = func(_ context.Context, jid types.JID) string {
		if jid == adaPN {
			return "Ada Lovelace"
		}
		return ""
	}
	runtime.groupName = func(context.Context, types.JID) string { return "Team" }
	return runtime, transport
}

func TestSensitiveLogs(t *testing.T) {
	var out bytes.Buffer
	logger := newRedactingLogger(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	logger.Sub("Client").Warnf("Failed to decrypt from %s: key %s", "12025550123@s.whatsapp.net", "Zm9vYmFyYmF6cXV4cXV1eHF1dXhxdXV4eA==")
	logger.Errorf("send to %s failed", "120363025555555555@g.us")
	logger.Infof("message %s", "secret plan")
	logger.Debugf("debug %s", "secret plan")
	for _, leak := range []string{"12025550123", "120363025555555555", "Zm9vYmFyYmF6cXV4", "secret plan"} {
		if strings.Contains(out.String(), leak) {
			t.Fatalf("log leaked %q:\n%s", leak, out.String())
		}
	}
	if !strings.Contains(out.String(), "Failed to decrypt") || !strings.Contains(out.String(), "Client") {
		t.Fatalf("diagnostics lost:\n%s", out.String())
	}

	out.Reset()
	runtime, _ := newTestRuntime(t, &out)
	runtime.handleEvent(textEvent("L1", adaLID, adaLID, "secret plan for 12025550123", t0))
	if strings.Contains(out.String(), "secret plan") || strings.Contains(out.String(), "12025550123") {
		t.Fatalf("event handling logged personal data:\n%s", out.String())
	}
}

func textEvent(id string, chat, sender types.JID, text string, at time.Time) *events.Message {
	return &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: chat.Server == types.GroupServer}, ID: id, Timestamp: at, PushName: "Pushed"},
		Message: &waE2E.Message{Conversation: proto.String(text)},
	}
}

func readJSON(t *testing.T, runtime *WhatsAppRuntime, tool, request string) string {
	t.Helper()
	result, err := runtime.Read(context.Background(), tool, json.RawMessage(request))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	data, _ := json.Marshal(result)
	return string(data)
}

func TestEventsAreStored(t *testing.T) {
	runtime, _ := newTestRuntime(t, nil)
	// A LID chat whose phone number is known is stored under the phone number.
	runtime.handleEvent(textEvent("L1", adaLID, adaLID, "hello from a LID", t0))
	image := &events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: adaPN, Sender: adaPN}, ID: "I1", Timestamp: t0.Add(time.Minute)},
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption: proto.String("look at this"), Mimetype: proto.String("image/jpeg"),
			URL: proto.String("https://mmg.whatsapp.net/v/t62/x.enc"), DirectPath: proto.String("/v/t62/x.enc?ccb=1"),
			MediaKey: bytes.Repeat([]byte{1}, 32), FileSHA256: bytes.Repeat([]byte{2}, 32), FileEncSHA256: bytes.Repeat([]byte{3}, 32), FileLength: proto.Uint64(100),
		}},
	}
	runtime.handleEvent(image)
	runtime.handleEvent(textEvent("G1", team, adaPN, "group news", t0.Add(2*time.Minute)))
	runtime.handleEvent(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: adaPN, Sender: adaPN}, ID: "R1", Timestamp: t0}, Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}})

	chat := readJSON(t, runtime, "get_chat", `{"chat_jid":"12025550123@s.whatsapp.net","include_last_message":false}`)
	if !strings.Contains(chat, `"name":"Ada Lovelace"`) {
		t.Fatalf("chat: %s", chat)
	}
	messages := readJSON(t, runtime, "list_messages", `{"chat_jid":"12025550123@s.whatsapp.net","include_context":false}`)
	for _, want := range []string{`"id":"L1"`, `"content":"hello from a LID"`, `"id":"I1"`, `"content":"look at this"`, `"media_type":"image"`} {
		if !strings.Contains(messages, want) {
			t.Errorf("messages lack %s: %s", want, messages)
		}
	}
	if strings.Contains(messages, `"R1"`) {
		t.Error("a reaction without text or media was stored as a message")
	}
	if info, err := runtime.store.GetMediaInfo(context.Background(), "I1", adaPN.String()); err != nil || info.DirectPath != "/v/t62/x.enc?ccb=1" || info.FileLength != 100 {
		t.Fatalf("media info: %+v %v", info, err)
	}
	if group := readJSON(t, runtime, "get_chat", `{"chat_jid":"120363025555555555@g.us"}`); !strings.Contains(group, `"name":"Team"`) || !strings.Contains(group, `"sender":"12025550123"`) {
		t.Fatalf("group: %s", group)
	}

	history := &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{
		ID:   proto.String("12025550199@s.whatsapp.net"),
		Name: proto.String("Bob from history"),
		Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String("12025550199@s.whatsapp.net"), FromMe: proto.Bool(false), ID: proto.String("H1")},
			MessageTimestamp: proto.Uint64(uint64(t0.Unix())),
			Message:          &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{Conversation: proto.String("old ephemeral text")}}},
		}}},
	}}}}
	runtime.handleEvent(history)
	if bob := readJSON(t, runtime, "get_chat", `{"chat_jid":"12025550199@s.whatsapp.net"}`); !strings.Contains(bob, `"name":"Bob from history"`) || !strings.Contains(bob, `"content":"old ephemeral text"`) {
		t.Fatalf("history sync: %s", bob)
	}
}

func TestAdapterSendRequiresFrozenDraft(t *testing.T) {
	runtime, transport := newTestRuntime(t, nil)
	dir := runtime.cfg.DataDir
	for name, data := range samples {
		outboxFile(t, dir, name, data)
	}
	ctx := context.Background()
	freeze := func(in SendInput) FrozenDraft {
		draft, err := FreezeSend(ctx, dir, in)
		if err != nil {
			t.Fatalf("freeze %+v: %v", in, err)
		}
		return draft
	}

	receipt, err := runtime.Send(ctx, freeze(SendInput{Kind: "text", Recipient: "12025550123", Message: "hi Ada"}))
	if err != nil || receipt.MessageID != "3EB0SENT1" || transport.sent[0].GetConversation() != "hi Ada" || transport.to[0] != adaPN || !transport.deadline {
		t.Fatalf("text send: %+v %v (deadline %v)", receipt, err, transport.deadline)
	}
	if len(transport.uploads) != 0 {
		t.Fatal("a text message uploaded media")
	}

	for _, c := range []struct {
		in    SendInput
		kind  whatsmeow.MediaType
		check func(*waE2E.Message) bool
	}{
		{SendInput{Kind: "file", Recipient: "12025550123", MediaID: "report.pdf"}, whatsmeow.MediaDocument, func(m *waE2E.Message) bool {
			d := m.GetDocumentMessage()
			return d.GetFileName() == "report.pdf" && d.GetMimetype() == "application/pdf" && d.GetDirectPath() == "/v/up.enc?ccb=1" && len(d.GetMediaKey()) == 32
		}},
		{SendInput{Kind: "file", Recipient: "12025550123", MediaID: "photo.png"}, whatsmeow.MediaImage, func(m *waE2E.Message) bool { return m.GetImageMessage().GetMimetype() == "image/png" }},
		{SendInput{Kind: "file", Recipient: "12025550123", MediaID: "clip.mp4"}, whatsmeow.MediaVideo, func(m *waE2E.Message) bool { return m.GetVideoMessage().GetMimetype() == "video/mp4" }},
		{SendInput{Kind: "voice", Recipient: "12025550123", MediaID: "voice.ogg"}, whatsmeow.MediaAudio, func(m *waE2E.Message) bool {
			a := m.GetAudioMessage()
			return a.GetPTT() && a.GetSeconds() == 1 && a.GetMimetype() == opusMIME
		}},
	} {
		draft := freeze(c.in)
		before := len(transport.uploads)
		if _, err := runtime.Send(ctx, draft); err != nil {
			t.Fatalf("%s: %v", c.in.MediaID, err)
		}
		if len(transport.uploads) != before+1 || !bytes.Equal(transport.uploads[before], draft.Media) || transport.types[before] != c.kind || !c.check(transport.sent[len(transport.sent)-1]) {
			t.Errorf("%s: upload/message mismatch: %v", c.in.MediaID, transport.sent[len(transport.sent)-1])
		}
	}
	if sent := readJSON(t, runtime, "list_messages", `{"chat_jid":"12025550123@s.whatsapp.net","include_context":false}`); !strings.Contains(sent, `"content":"hi Ada"`) || !strings.Contains(sent, `"is_from_me":true`) {
		t.Fatalf("outgoing message not recorded: %s", sent)
	}

	runtime.connected = func() bool { return false }
	calls := len(transport.sent)
	if _, err := runtime.Send(ctx, freeze(SendInput{Kind: "text", Recipient: "12025550123", Message: "offline"})); !errors.Is(err, ErrClosed) || len(transport.sent) != calls {
		t.Fatalf("offline send: %v", err)
	}
	runtime.connected = func() bool { return true }
	transport.upErr = errors.New("upload refused")
	if _, err := runtime.Send(ctx, freeze(SendInput{Kind: "file", Recipient: "12025550123", MediaID: "report.pdf"})); !errors.Is(err, ErrSendFailed) || len(transport.sent) != calls {
		t.Fatalf("failed upload: %v", err)
	}
	transport.upErr, transport.sendErr = nil, errors.New("connection lost")
	if _, err := runtime.Send(ctx, freeze(SendInput{Kind: "text", Recipient: "12025550123", Message: "maybe"})); err == nil || errors.Is(err, ErrSendFailed) || errors.Is(err, ErrClosed) {
		t.Fatalf("an interrupted send must stay uncertain: %v", err)
	}
}

func TestOfflineSyncSignalled(t *testing.T) {
	runtime, _ := newTestRuntime(t, nil)
	select {
	case <-runtime.synced:
		t.Fatal("synced before any sync")
	default:
	}
	runtime.handleEvent(&events.OfflineSyncCompleted{Count: 3})
	runtime.handleEvent(&events.OfflineSyncCompleted{Count: 0}) // a second event must not panic
	select {
	case <-runtime.synced:
	default:
		t.Fatal("offline sync completion not signalled")
	}
}

func TestTerminalDisconnectPreservesSession(t *testing.T) {
	for name, event := range map[string]any{
		"logged out":      &events.LoggedOut{},
		"stream replaced": &events.StreamReplaced{},
		"temporary ban":   &events.TemporaryBan{Expire: time.Hour},
		"client outdated": &events.ClientOutdated{},
	} {
		runtime, _ := newTestRuntime(t, nil)
		session := filepath.Join(runtime.cfg.DataDir, "whatsapp.db")
		if runtime.Status().State != "unpaired" {
			t.Fatalf("fresh runtime: %+v", runtime.Status())
		}
		runtime.handleEvent(&events.Connected{})
		if runtime.Status() != (RuntimeStatus{State: "online", Connected: true}) {
			t.Fatalf("after connect: %+v", runtime.Status())
		}
		runtime.handleEvent(event)
		runtime.handleEvent(&events.Disconnected{})
		if st := runtime.Status(); st.State != "action_required" || st.Connected {
			t.Errorf("%s: status %+v", name, st)
		}
		if runtime.client.AutoReconnectHook == nil || runtime.client.AutoReconnectHook(errors.New("retry")) {
			t.Errorf("%s: automatic reconnects continue", name)
		}
		if _, err := os.Stat(session); err != nil {
			t.Errorf("%s: session database removed: %v", name, err)
		}
	}
	runtime, _ := newTestRuntime(t, nil)
	runtime.handleEvent(&events.Connected{})
	runtime.handleEvent(&events.Disconnected{})
	if st := runtime.Status(); st.State != "offline" || !runtime.client.AutoReconnectHook(errors.New("retry")) {
		t.Fatalf("an ordinary disconnect must keep reconnecting: %+v", st)
	}
}

type staticRoundTripper struct{ response *http.Response }

func (s staticRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return s.response, nil }

func TestDownloadTransportLimits(t *testing.T) {
	limit := int64(maxMediaBytes + 32)
	get := func(length int64, body int) error {
		transport := cappedTransport{base: staticRoundTripper{&http.Response{StatusCode: 200, ContentLength: length, Body: io.NopCloser(bytes.NewReader(make([]byte, body)))}}, limit: limit}
		resp, err := transport.RoundTrip(&http.Request{})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		return err
	}
	if err := get(1000, 1000); err != nil {
		t.Fatalf("small download: %v", err)
	}
	if err := get(limit+1, 0); err == nil {
		t.Error("declared oversized download accepted")
	}
	if err := get(10, int(limit)+1); err == nil {
		t.Error("streamed oversized download accepted despite a small declared length")
	}
	if err := get(-1, int(limit)); err != nil {
		t.Errorf("download at the limit: %v", err)
	}
}

func TestMediaDirectPath(t *testing.T) {
	for _, good := range []string{"/v/t62.7118-24/123_n.enc?ccb=11-4&oh=01&oe=AB&_nc_sid=5e03e0", "/o1/v/t24/f2/m233/x.enc"} {
		if err := validateDirectPath(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{"", "v/x", "//attacker.example/x", "https://attacker.example/x", "/x\r\nHost: attacker", "/x y", `/x\y`, "/x#frag", "/" + strings.Repeat("a", 2048)} {
		if err := validateDirectPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if path, err := legacyDirectPath("https://mmg.whatsapp.net/v/t62/x.enc?ccb=1&oh=2"); err != nil || path != "/v/t62/x.enc?ccb=1&oh=2" {
		t.Errorf("legacy URL: %q %v", path, err)
	}
	for _, bad := range []string{"http://mmg.whatsapp.net/v/x", "https://attacker.example/v/x", "https://mmg.whatsapp.net.attacker.example/v/x", "https://user@mmg.whatsapp.net/v/x", "https://127.0.0.1/v/x", "https://mmg.whatsapp.net:444/v/x"} {
		if _, err := legacyDirectPath(bad); err == nil {
			t.Errorf("legacy %q accepted", bad)
		}
	}

	runtime, _ := newTestRuntime(t, nil)
	downloads := 0
	runtime.download = func(context.Context, whatsmeow.DownloadableMessage) ([]byte, error) {
		downloads++
		return []byte("\xff\xd8\xff downloaded"), nil
	}
	ctx := context.Background()
	store := func(id, directPath, url string, length uint64) {
		runtime.store.StoreChat(ctx, adaPN.String(), "Ada", t0)
		runtime.store.StoreMessage(ctx, StoredMessage{ID: id, ChatJID: adaPN.String(), Sender: "12025550123", MediaType: "image", Filename: "photo.jpg", Timestamp: t0,
			URL: url, DirectPath: directPath, MediaKey: bytes.Repeat([]byte{1}, 32), FileSHA256: bytes.Repeat([]byte{2}, 32), FileEncSHA256: bytes.Repeat([]byte{3}, 32), FileLength: length})
	}
	store("evil", "//attacker.example/steal", "", 10)
	store("legacy-evil", "", "https://attacker.example/v/x", 10)
	store("huge", "/v/t62/huge.enc", "", maxMediaBytes+1)
	store("ok", "/v/t62/ok.enc?ccb=1", "", 10)
	for _, id := range []string{"evil", "legacy-evil", "huge"} {
		if _, err := runtime.Download(ctx, id, adaPN.String()); err == nil {
			t.Errorf("%s: downloaded", id)
		}
	}
	if downloads != 0 {
		t.Fatal("an unsafe media reference reached the network")
	}
	media, err := runtime.Download(ctx, "ok", adaPN.String())
	if err != nil || downloads != 1 || filepath.Dir(media.Path) != filepath.Join(runtime.cfg.DataDir, "downloads") || media.Name != "photo.jpg" {
		t.Fatalf("download: %+v %v", media, err)
	}
	if _, err := runtime.Download(ctx, "missing", adaPN.String()); !errors.Is(err, ErrMediaNotFound) {
		t.Fatalf("missing media: %v", err)
	}
}

func TestUnpairedRuntimeLookups(t *testing.T) {
	dir := privateDataDir(t)
	store, err := OpenMessageStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime, err := NewWhatsAppRuntime(RuntimeConfig{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, store)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx := context.Background()
	if pn, err := runtime.resolvePN(ctx, adaLID); err != nil || !pn.IsEmpty() {
		t.Fatalf("resolvePN on an unpaired device: %v %v", pn, err)
	}
	if name := runtime.contactName(ctx, adaPN); name != "" {
		t.Fatalf("contactName on an unpaired device: %q", name)
	}
	if err := runtime.Connect(ctx); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("connecting without a session must not start pairing: %v", err)
	}
	if !runtime.client.EnableAutoReconnect || !runtime.client.InitialAutoReconnect {
		t.Fatal("the bridge must keep retrying when the network is down at startup")
	}
}
