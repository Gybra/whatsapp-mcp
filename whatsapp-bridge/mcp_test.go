package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMain lets the test binary act as the MCP stdio server for TestStdioClean.
func TestMain(m *testing.M) {
	if url := os.Getenv("WHATSAPP_MCP_TEST_BRIDGE"); url != "" {
		token, _ := hex.DecodeString(os.Getenv("WHATSAPP_MCP_TEST_TOKEN"))
		client := NewBridgeClient(token)
		client.baseURL = url
		if err := RunMCP(context.Background(), client); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type storeReads struct {
	*MessageStore
	downloads atomic.Int32
}

func (s *storeReads) Download(context.Context, string, string) (DownloadedMedia, error) {
	s.downloads.Add(1)
	return DownloadedMedia{AttachmentView{ID: "d1", Name: "photo.jpg", MIME: "image/jpeg", Size: 3, SHA256: strings.Repeat("a", 64)}, "/data/downloads/d1.jpg"}, nil
}

type mcpTest struct {
	*apiTest
	session *mcp.ClientSession
	server  *httptest.Server
}

// newMCPTest wires a real MCP client and server to the real API, backed by a seeded store and a fake sender.
func newMCPTest(t *testing.T) *mcpTest {
	t.Helper()
	a := newAPITest(t)
	reads := &storeReads{MessageStore: seededStore(t)}
	a.handler = NewAPI(APIConfig{Token: decodeHex(t, a.token)}, a.manager, fakeFreeze, a.sender, reads)
	server := httptest.NewServer(a.handler)
	t.Cleanup(server.Close)
	client := NewBridgeClient(decodeHex(t, a.token))
	client.baseURL = server.URL

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := NewMCPServer(client).Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return &mcpTest{apiTest: a, session: session, server: server}
}

func decodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (m *mcpTest) call(t *testing.T, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	result, err := m.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return &mcp.CallToolResult{IsError: true}, err.Error()
	}
	var text strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return result, text.String()
}

var allTools = []string{"download_media", "get_chat", "get_contact_chats", "get_direct_chat_by_contact", "get_last_interaction", "get_message_context",
	"list_chats", "list_messages", "search_contacts", "send_audio_message", "send_file", "send_message"}

func TestMCPToolsDiscovery(t *testing.T) {
	m := newMCPTest(t)
	tools, err := m.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]*mcp.Tool{}
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		byName[tool.Name] = tool
	}
	slices.Sort(names)
	if !slices.Equal(names, allTools) {
		t.Fatalf("tools: %v", names)
	}
	for _, name := range allTools {
		tool := byName[name]
		readOnly := !strings.HasPrefix(name, "send_") && name != "download_media"
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != readOnly {
			t.Errorf("%s: read-only hint should be %v", name, readOnly)
		}
		if tool.Description == "" {
			t.Errorf("%s: no description", name)
		}
	}
	schema := func(name string) string { data, _ := json.Marshal(byName[name].InputSchema); return string(data) }
	for _, name := range []string{"send_file", "send_audio_message"} {
		if s := schema(name); !strings.Contains(s, `"media_id"`) || strings.Contains(s, "media_path") || !strings.Contains(s, `"required":["recipient","media_id"]`) {
			t.Errorf("%s schema: %s", name, s)
		}
	}
	if s := schema("send_message"); !strings.Contains(s, `"required":["recipient","message"]`) || !strings.Contains(s, `"approval"`) {
		t.Errorf("send_message schema: %s", s)
	}
}

func TestMCPReadTools(t *testing.T) {
	m := newMCPTest(t)
	cases := map[string]struct {
		args map[string]any
		want string
	}{
		"list_chats":                 {map[string]any{"limit": 2}, `"jid":"120363025555555555@g.us"`},
		"list_messages":              {map[string]any{"query": "dexoverse", "include_context": false}, `"id":"a3"`},
		"search_contacts":            {map[string]any{"query": "ada"}, `"phone_number":"12025550123"`},
		"get_chat":                   {map[string]any{"chat_jid": adaJID}, `"name":"Ada"`},
		"get_direct_chat_by_contact": {map[string]any{"sender_phone_number": "12025550123"}, `"jid":"12025550123@s.whatsapp.net"`},
		"get_contact_chats":          {map[string]any{"jid": adaJID}, `"chats":[`},
		"get_last_interaction":       {map[string]any{"jid": adaJID}, `"id":"g1"`},
		"get_message_context":        {map[string]any{"message_id": "a3", "chat_jid": adaJID, "before": 1, "after": 1}, `"id":"a2"`},
		"download_media":             {map[string]any{"message_id": "a4", "chat_jid": adaJID}, `"path":"/data/downloads/d1.jpg"`},
	}
	for name, c := range cases {
		result, text := m.call(t, name, c.args)
		structured, _ := json.Marshal(result.StructuredContent)
		if result.IsError || !strings.Contains(string(structured), c.want) {
			t.Errorf("%s: error=%v structured=%s text=%s", name, result.IsError, structured, text)
		}
	}
	if result, text := m.call(t, "get_message_context", map[string]any{"message_id": "dup"}); !result.IsError || !strings.Contains(text, "invalid") {
		t.Errorf("ambiguous message id: %v %s", result.IsError, text)
	}
	if result, _ := m.call(t, "list_messages", map[string]any{"limit": 500}); !result.IsError {
		t.Error("limit 500 accepted")
	}
	if result, _ := m.call(t, "list_chats", map[string]any{"sql": "DROP TABLE chats"}); !result.IsError {
		t.Error("unknown argument accepted")
	}
}

func TestMCPSendRequiresApproval(t *testing.T) {
	m := newMCPTest(t)
	input := map[string]any{"recipient": "12025550123", "message": "hello"}
	result, text := m.call(t, "send_message", input)
	if !result.IsError || !strings.Contains(text, "approval") || !strings.Contains(text, "Nothing was sent") {
		t.Fatalf("send without approval: %s", text)
	}
	forged := map[string]any{"recipient": "12025550123", "message": "hello", "approval": map[string]any{"draft_id": strings.Repeat("0", 32), "signature_b64": strings.Repeat("A", 86) + "=="}}
	if result, text := m.call(t, "send_message", forged); !result.IsError || !strings.Contains(text, "not approved") {
		t.Fatalf("forged approval: %s", text)
	}

	// The user approved "hello"; a later hook or the model changes the text.
	view := m.createDraft(t, textInput)
	proof := signView(t, m.key, view)
	approval := map[string]any{"draft_id": proof.DraftID, "signature_b64": proof.SignatureB64}
	if result, text := m.call(t, "send_message", map[string]any{"recipient": "12025550123", "message": "hello, changed", "approval": approval}); !result.IsError || !strings.Contains(text, "not approved") {
		t.Fatalf("mutated message: %s", text)
	}
	if m.sender.calls.Load() != 0 {
		t.Fatal("an unapproved message reached the sender")
	}
	proof = signView(t, m.key, m.createDraft(t, textInput))
	approval = map[string]any{"draft_id": proof.DraftID, "signature_b64": proof.SignatureB64}
	result, text = m.call(t, "send_message", map[string]any{"recipient": "12025550123", "message": "hello", "approval": approval})
	structured, _ := json.Marshal(result.StructuredContent)
	if result.IsError || !strings.Contains(string(structured), `"status":"sent"`) || m.sender.calls.Load() != 1 {
		t.Fatalf("approved send: %s %s", text, structured)
	}

	fileInput := `{"kind":"file","recipient":"12025550123","message":"","media_id":"report.pdf"}`
	proof = signView(t, m.key, m.createDraft(t, fileInput))
	fileArgs := map[string]any{"recipient": "12025550123", "media_id": "report.pdf", "approval": map[string]any{"draft_id": proof.DraftID, "signature_b64": proof.SignatureB64}}
	if result, text := m.call(t, "send_file", fileArgs); result.IsError || m.sender.last.Payload.Kind != "file" || string(m.sender.last.Media) != "frozen:report.pdf" {
		t.Fatalf("approved file send: %s (kind %q)", text, m.sender.last.Payload.Kind)
	}

	m.sender.err = ErrSendUncertain
	proof = signView(t, m.key, m.createDraft(t, textInput))
	if result, text := m.call(t, "send_message", map[string]any{"recipient": "12025550123", "message": "hello", "approval": map[string]any{"draft_id": proof.DraftID, "signature_b64": proof.SignatureB64}}); !result.IsError || !strings.Contains(text, "Do not send it again") {
		t.Fatalf("uncertain send: %s", text)
	}
}

func TestMCPSendFileSchema(t *testing.T) {
	m := newMCPTest(t)
	for _, args := range []map[string]any{
		{"recipient": "12025550123", "media_path": "/Users/ada/.ssh/id_ed25519"},
		{"recipient": "12025550123", "media_id": "a.pdf", "media_path": "/etc/hosts"},
	} {
		if result, _ := m.call(t, "send_file", args); !result.IsError {
			t.Errorf("send_file accepted %v", args)
		}
	}
	if m.sender.calls.Load() != 0 {
		t.Fatal("a raw path reached the sender")
	}
}

func TestClientNoProxyOrRedirect(t *testing.T) {
	var stolen atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			stolen.Add(1)
		}
	}))
	defer elsewhere.Close()
	var host string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		http.Redirect(w, r, elsewhere.URL+"/api/read/list_chats", http.StatusTemporaryRedirect)
	}))
	defer bridge.Close()
	t.Setenv("HTTP_PROXY", elsewhere.URL)
	t.Setenv("http_proxy", elsewhere.URL)
	client := NewBridgeClient(bytes.Repeat([]byte{1}, 32))
	client.baseURL = bridge.URL
	var out any
	if err := client.Read(context.Background(), "list_chats", map[string]any{}, &out); err == nil {
		t.Fatal("redirect followed")
	}
	if stolen.Load() != 0 {
		t.Fatal("the bearer token left the bridge connection")
	}
	if host != "127.0.0.1:8080" {
		t.Fatalf("Host header %q", host)
	}
}

func TestClientNoWriteRetry(t *testing.T) {
	var requests atomic.Int32
	listener, _ := net.Listen("tcp4", "127.0.0.1:0")
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			requests.Add(1)
			buf := make([]byte, 4096)
			conn.Read(buf)
			conn.Close() // the response is lost
		}
	}()
	defer listener.Close()
	client := NewBridgeClient(bytes.Repeat([]byte{1}, 32))
	client.baseURL = "http://" + listener.Addr().String()
	if _, err := client.Send(context.Background(), SendInput{Kind: "text", Recipient: "12025550123", Message: "hi"}, &ApprovalProof{DraftID: "x", SignatureB64: "y"}); !errors.Is(err, ErrSendUncertain) {
		t.Fatalf("lost response must be reported as uncertain, got %v", err)
	}
	listener.Close()
	if _, err := client.Send(context.Background(), SendInput{Kind: "text", Recipient: "12025550123", Message: "hi"}, &ApprovalProof{DraftID: "x", SignatureB64: "y"}); !errors.Is(err, ErrBridgeUnreachable) {
		t.Fatalf("a bridge that is not running must be reported as unreachable, got %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("send attempted %d times", requests.Load())
	}
}

func TestStdioClean(t *testing.T) {
	m := newMCPTest(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "WHATSAPP_MCP_TEST_BRIDGE="+m.server.URL, "WHATSAPP_MCP_TEST_TOKEN="+m.token)
	var stderr syncBuffer // written by the process's stderr copier while the test reads it
	cmd.Stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "stdio-test"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect over stdio: %v (stderr: %s)", err, stderr.String())
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != len(allTools) {
		t.Fatalf("list tools over stdio: %v", err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search_contacts", Arguments: map[string]any{"query": "ada"}})
	if err != nil || result.IsError {
		t.Fatalf("call over stdio: %v %+v (stderr: %s)", err, result, stderr.String())
	}
	if strings.Contains(stderr.String(), "Ada") || strings.Contains(stderr.String(), "12025550123") {
		t.Fatalf("stderr leaked personal data: %s", stderr.String())
	}
}

func TestMCPNullResults(t *testing.T) {
	m := newMCPTest(t)
	for name, args := range map[string]map[string]any{
		"get_chat":                   {"chat_jid": "19999999999@s.whatsapp.net"},
		"get_direct_chat_by_contact": {"sender_phone_number": "19999999999"},
		"get_last_interaction":       {"jid": "19999999999@s.whatsapp.net"},
	} {
		result, text := m.call(t, name, args)
		structured, _ := json.Marshal(result.StructuredContent)
		if result.IsError || (!strings.Contains(string(structured), `"chat":null`) && !strings.Contains(string(structured), `"message":null`)) {
			t.Errorf("%s with no match: error=%v %s %s", name, result.IsError, structured, text)
		}
	}
}

func TestClientResponseLimit(t *testing.T) {
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"contacts":"`))
		w.Write(bytes.Repeat([]byte("x"), maxResponseBytes))
		w.Write([]byte(`"}`))
	}))
	defer huge.Close()
	client := NewBridgeClient(bytes.Repeat([]byte{1}, 32))
	client.baseURL = huge.URL
	var out any
	if err := client.Read(context.Background(), "search_contacts", map[string]any{"query": "a"}, &out); err == nil {
		t.Fatal("oversized bridge response accepted")
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
