package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type fakeSender struct {
	calls  atomic.Int32
	err    error
	last   FrozenDraft
	ctxErr error
	budget time.Duration
	mu     sync.Mutex
}

func (s *fakeSender) Send(ctx context.Context, draft FrozenDraft) (SendReceipt, error) {
	s.calls.Add(1)
	s.mu.Lock()
	s.last, s.ctxErr = draft, ctx.Err()
	if deadline, ok := ctx.Deadline(); ok {
		s.budget = time.Until(deadline)
	}
	s.mu.Unlock()
	if s.err != nil {
		return SendReceipt{}, s.err
	}
	return SendReceipt{Status: "sent", MessageID: "3EB0TEST"}, nil
}

type fakeReads struct {
	tool    string
	request string
	result  any
	err     error
}

func (r *fakeReads) Read(ctx context.Context, tool string, request json.RawMessage) (any, error) {
	r.tool, r.request = tool, string(request)
	return r.result, r.err
}

func (r *fakeReads) Download(ctx context.Context, messageID, chatJID string) (AttachmentView, error) {
	return AttachmentView{ID: "dl-1", Name: "photo.jpg", MIME: "image/jpeg", Size: 3, SHA256: strings.Repeat("a", 64)}, r.err
}

func fakeFreeze(_ context.Context, in SendInput) (FrozenDraft, error) {
	jid := in.Recipient
	if !strings.Contains(jid, "@") {
		jid += "@s.whatsapp.net"
	}
	draft := FrozenDraft{Input: in, Payload: ApprovalPayload{RecipientJID: jid, Message: in.Message, Kind: in.Kind}}
	if in.Kind != "text" {
		draft.Media = []byte("frozen:" + in.MediaID)
		draft.Payload.Attachment = &AttachmentView{ID: in.MediaID, Name: in.MediaID, MIME: "application/pdf", Size: int64(len(draft.Media)), SHA256: sha256Hex(draft.Media)}
	}
	return draft, nil
}

type apiTest struct {
	*approvalHarness
	handler http.Handler
	token   string
	sender  *fakeSender
	reads   *fakeReads
	freeze  func(context.Context, SendInput) (FrozenDraft, error)
}

func newAPITest(t *testing.T) *apiTest {
	t.Helper()
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i + 1)
	}
	a := &apiTest{approvalHarness: newApprovalHarness(t), token: hex.EncodeToString(token), sender: &fakeSender{}, reads: &fakeReads{result: map[string]any{"ok": true}}, freeze: fakeFreeze}
	cfg := APIConfig{Token: token, RevokeApprover: func() error { a.leaseOK = false; return nil }}
	a.handler = NewAPI(cfg, a.manager, func(ctx context.Context, in SendInput) (FrozenDraft, error) { return a.freeze(ctx, in) }, a.sender, a.reads)
	return a
}

func (a *apiTest) request(method, path, body string, edit ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, change := range edit {
		change(req)
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error
}

func (a *apiTest) createDraft(t *testing.T, input string) DraftView {
	t.Helper()
	rec := a.request("POST", "/api/drafts", input)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft: %d %s", rec.Code, rec.Body)
	}
	var view DraftView
	json.Unmarshal(rec.Body.Bytes(), &view)
	return view
}

func sendBody(input string, proof *ApprovalProof) string {
	body := map[string]any{"input": json.RawMessage(input)}
	if proof != nil {
		body["approval"] = proof
	}
	data, _ := json.Marshal(body)
	return string(data)
}

const textInput = `{"kind":"text","recipient":"12025550123","message":"hello","media_id":""}`

var allRoutes = [][2]string{
	{"GET", "/api/status"}, {"POST", "/api/drafts"}, {"GET", "/api/drafts/x/preview"}, {"DELETE", "/api/drafts/x"},
	{"POST", "/api/send"}, {"DELETE", "/api/approver"}, {"POST", "/api/read/list_chats"}, {"POST", "/api/download"},
	{"GET", "/api/unknown"}, {"GET", "/"},
}

func TestAPIRequiresBearer(t *testing.T) {
	a := newAPITest(t)
	wrong := strings.Repeat("0", 64)
	headers := map[string]string{"missing": "", "wrong token": "Bearer " + wrong, "basic scheme": "Basic " + a.token, "upper-case hex": "Bearer " + strings.ToUpper(a.token)}
	for _, route := range allRoutes {
		for name, header := range headers {
			rec := a.request(route[0], route[1], "{}", func(r *http.Request) { r.Header.Set("Authorization", header) })
			if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "unauthorized" {
				t.Errorf("%s %s with %s auth: %d %s", route[0], route[1], name, rec.Code, rec.Body)
			}
		}
	}
	if a.sender.calls.Load() != 0 {
		t.Fatal("unauthenticated request reached the sender")
	}
}

func TestAPIRejectsBrowserAndRebinding(t *testing.T) {
	a := newAPITest(t)
	edits := map[string]func(*http.Request){
		"localhost host":   func(r *http.Request) { r.Host = "localhost:8080" },
		"rebinding host":   func(r *http.Request) { r.Host = "attacker.example:8080" },
		"other port":       func(r *http.Request) { r.Host = "127.0.0.1:9090" },
		"browser origin":   func(r *http.Request) { r.Header.Set("Origin", "http://attacker.example") },
		"null origin":      func(r *http.Request) { r.Header.Set("Origin", "null") },
		"fetch metadata":   func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"forwarded header": func(r *http.Request) { r.Header.Set("X-Forwarded-For", "10.0.0.5") },
	}
	for name, edit := range edits {
		rec := a.request("GET", "/api/status", "", edit)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, rec.Code)
		}
	}
}

func TestAPIMethodsAndStrictJSON(t *testing.T) {
	a := newAPITest(t)
	if rec := a.request("GET", "/api/send", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/send: %d", rec.Code)
	}
	if rec := a.request("POST", "/api/status", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/status: %d", rec.Code)
	}
	bad := map[string]string{
		"malformed":     `{"kind":`,
		"trailing data": textInput + `{}`,
		"unknown field": `{"kind":"text","recipient":"12025550123","message":"hi","media_id":"","approved":true}`,
		"array":         `[]`,
		"too large":     `{"kind":"text","recipient":"12025550123","message":"` + strings.Repeat("a", 64*1024) + `","media_id":""}`,
	}
	for name, body := range bad {
		rec := a.request("POST", "/api/drafts", body)
		if rec.Code/100 != 4 || errorCode(t, rec) != "invalid_request" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	huge := `{"padding":"` + strings.Repeat("a", 64*1024) + `"}`
	if rec := a.request("POST", "/api/read/list_chats", huge); rec.Code != http.StatusRequestEntityTooLarge || a.reads.tool != "" {
		t.Errorf("body over 64 KiB: %d, reached reads: %q", rec.Code, a.reads.tool)
	}
	if rec := a.request("POST", "/api/drafts", textInput, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain body: %d", rec.Code)
	}
	near := `{"kind":"text","recipient":"12025550123","message":"` + strings.Repeat("a", 32*1024) + `","media_id":""}`
	if rec := a.request("POST", "/api/drafts", near); rec.Code != http.StatusCreated {
		t.Errorf("32 KiB message within the 64 KiB body limit: %d %s", rec.Code, rec.Body)
	}
}

func TestAPIRecipientDomains(t *testing.T) {
	a := newAPITest(t)
	for _, ok := range []string{"12025550123", "12025550123@s.whatsapp.net", "120363025555555555@g.us", "123456789012345@lid"} {
		input := `{"kind":"text","recipient":"` + ok + `","message":"hi","media_id":""}`
		if rec := a.request("POST", "/api/drafts", input); rec.Code != http.StatusCreated {
			t.Errorf("%s rejected: %d %s", ok, rec.Code, rec.Body)
		}
	}
	for _, bad := range []string{"", "abc", "+12025550123", "status@broadcast", "12025550123@broadcast", "123456789@newsletter", "12025550123:4@s.whatsapp.net", "12025550123@s.whatsapp.net.evil", "12345"} {
		input := `{"kind":"text","recipient":"` + bad + `","message":"hi","media_id":""}`
		if rec := a.request("POST", "/api/drafts", input); rec.Code != http.StatusBadRequest {
			t.Errorf("%q accepted: %d", bad, rec.Code)
		}
	}
	for _, bad := range []string{`{"kind":"poll","recipient":"12025550123","message":"hi","media_id":""}`, `{"kind":"file","recipient":"12025550123","message":"","media_id":""}`, `{"kind":"text","recipient":"12025550123","message":"hi","media_id":"x.pdf"}`} {
		if rec := a.request("POST", "/api/drafts", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s accepted: %d", bad, rec.Code)
		}
	}
}

func TestAPISendRequiresApproval(t *testing.T) {
	a := newAPITest(t)
	view := a.createDraft(t, textInput)
	proof := signView(t, a.key, view)

	if rec := a.request("POST", "/api/send", sendBody(textInput, nil)); rec.Code != http.StatusForbidden || errorCode(t, rec) != "approval_required" {
		t.Errorf("no approval: %d %s", rec.Code, rec.Body)
	}
	forged := ApprovalProof{DraftID: view.DraftID, SignatureB64: strings.Repeat("A", 86) + "=="}
	if rec := a.request("POST", "/api/send", sendBody(textInput, &forged)); errorCode(t, rec) != "approval_invalid" {
		t.Errorf("forged proof: %d %s", rec.Code, rec.Body)
	}
	changed := strings.Replace(textInput, "hello", "hello!", 1)
	if rec := a.request("POST", "/api/send", sendBody(changed, &proof)); errorCode(t, rec) != "approval_invalid" {
		t.Errorf("changed message: %d %s", rec.Code, rec.Body)
	}
	if a.sender.calls.Load() != 0 {
		t.Fatal("an unapproved operation reached the sender")
	}

	fileInput := `{"kind":"file","recipient":"12025550123","message":"report","media_id":"report.pdf"}`
	fileProof := signView(t, a.key, a.createDraft(t, fileInput))
	rec := a.request("POST", "/api/send", sendBody(fileInput, &fileProof))
	if rec.Code != http.StatusOK || a.sender.calls.Load() != 1 || string(a.sender.last.Media) != "frozen:report.pdf" {
		t.Fatalf("approved send: %d %s, calls %d", rec.Code, rec.Body, a.sender.calls.Load())
	}
	if rec := a.request("POST", "/api/send", sendBody(fileInput, &fileProof)); errorCode(t, rec) != "approval_used" || a.sender.calls.Load() != 1 {
		t.Fatalf("replay: %d %s, calls %d", rec.Code, rec.Body, a.sender.calls.Load())
	}
}

func TestAPIConcurrentReplay(t *testing.T) {
	a := newAPITest(t)
	proof := signView(t, a.key, a.createDraft(t, textInput))
	var wg sync.WaitGroup
	var ok atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a.request("POST", "/api/send", sendBody(textInput, &proof)).Code == http.StatusOK {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || a.sender.calls.Load() != 1 {
		t.Fatalf("successes %d, sender calls %d; want 1 and 1", ok.Load(), a.sender.calls.Load())
	}
}

func TestSendResponseLost(t *testing.T) {
	a := newAPITest(t)
	a.sender.err = errors.New("connection reset after the message left")
	proof := signView(t, a.key, a.createDraft(t, textInput))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client has already gone away
	rec := a.request("POST", "/api/send", sendBody(textInput, &proof), func(r *http.Request) { *r = *r.WithContext(ctx) })
	if errorCode(t, rec) != "send_uncertain" || a.sender.calls.Load() != 1 {
		t.Fatalf("lost response: %d %s, calls %d", rec.Code, rec.Body, a.sender.calls.Load())
	}
	if a.sender.ctxErr != nil || a.sender.budget <= 0 || a.sender.budget > 60*time.Second {
		t.Fatalf("a client disconnect must not cancel a send in progress, and sends are bounded: ctx err %v, budget %v", a.sender.ctxErr, a.sender.budget)
	}
	a.sender.err = nil
	if rec := a.request("POST", "/api/send", sendBody(textInput, &proof)); errorCode(t, rec) != "approval_used" || a.sender.calls.Load() != 1 {
		t.Fatalf("retry after an uncertain send must need a new approval: %d %s", rec.Code, rec.Body)
	}
}

func TestNoEnrollmentAPI(t *testing.T) {
	a := newAPITest(t)
	for _, route := range [][2]string{{"POST", "/api/enroll"}, {"PUT", "/api/approver"}, {"POST", "/api/approver"}, {"POST", "/api/pair"}, {"PUT", "/api/approver/key"}} {
		body := `{"public_key_b64":"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="}`
		if rec := a.request(route[0], route[1], body); rec.Code/100 == 2 {
			t.Errorf("%s %s succeeded: %d", route[0], route[1], rec.Code)
		}
	}
	pending := a.createDraft(t, textInput)
	if rec := a.request("DELETE", "/api/approver", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	var status APIStatus
	json.Unmarshal(a.request("GET", "/api/status", "").Body.Bytes(), &status)
	if status.ApproverID != "" || status.ApproverExpiresAt != 0 {
		t.Fatalf("approver still trusted after revoke: %+v", status)
	}
	a.leaseOK = true // the same key is paired again
	proof := signView(t, a.key, pending)
	if rec := a.request("POST", "/api/send", sendBody(textInput, &proof)); rec.Code == http.StatusOK {
		t.Fatal("a draft survived revocation")
	}
}

func TestAPIStatus(t *testing.T) {
	a := newAPITest(t)
	var status APIStatus
	rec := a.request("GET", "/api/status", "")
	json.Unmarshal(rec.Body.Bytes(), &status)
	id, expires, _ := a.manager.TrustedApprover()
	if rec.Code != http.StatusOK || status.State != "offline" || status.Connected || status.ApproverID != id || status.ApproverExpiresAt != expires {
		t.Fatalf("status: %d %+v", rec.Code, status)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("API responses must not be cached")
	}
}

func TestAPIPreview(t *testing.T) {
	a := newAPITest(t)
	fileInput := `{"kind":"file","recipient":"12025550123","message":"","media_id":"a.pdf"}`
	view := a.createDraft(t, fileInput)
	rec := a.request("GET", "/api/drafts/"+view.DraftID+"/preview", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "frozen:a.pdf" || rec.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("preview: %d %q %s", rec.Code, rec.Body, rec.Header().Get("Content-Type"))
	}
	if rec := a.request("GET", "/api/drafts/"+a.createDraft(t, textInput).DraftID+"/preview", ""); rec.Code != http.StatusNotFound {
		t.Errorf("text draft preview: %d", rec.Code)
	}
	if rec := a.request("DELETE", "/api/drafts/"+view.DraftID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", rec.Code)
	}
	if rec := a.request("GET", "/api/drafts/"+view.DraftID+"/preview", ""); rec.Code != http.StatusNotFound {
		t.Errorf("cancelled draft preview: %d", rec.Code)
	}
}

func TestAPIReadAndDownload(t *testing.T) {
	a := newAPITest(t)
	rec := a.request("POST", "/api/read/list_chats", `{"limit":5}`)
	if rec.Code != http.StatusOK || a.reads.tool != "list_chats" || a.reads.request != `{"limit":5}` || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("read: %d %s (%s %s)", rec.Code, rec.Body, a.reads.tool, a.reads.request)
	}
	if rec := a.request("POST", "/api/read/list_chats", `[1]`); rec.Code != http.StatusBadRequest {
		t.Errorf("non-object read request: %d", rec.Code)
	}
	a.reads.err = ErrInvalidRequest
	if rec := a.request("POST", "/api/read/drop_table", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown read tool: %d", rec.Code)
	}
	a.reads.err = nil
	a.reads.result = strings.Repeat("x", 4*1024*1024)
	if rec := a.request("POST", "/api/read/list_messages", `{}`); rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "invalid_request" {
		t.Errorf("oversized result: %d %s", rec.Code, rec.Body.String()[:min(80, rec.Body.Len())])
	}
	if rec := a.request("POST", "/api/download", `{"message_id":"3EB0","chat_jid":"12025550123@s.whatsapp.net"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"photo.jpg"`) {
		t.Errorf("download: %d %s", rec.Code, rec.Body)
	}
	if rec := a.request("POST", "/api/download", `{"message_id":"","chat_jid":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty download request: %d", rec.Code)
	}
}

func TestAPIErrorsAreSafe(t *testing.T) {
	a := newAPITest(t)
	a.freeze = func(context.Context, SendInput) (FrozenDraft, error) {
		return FrozenDraft{}, errors.New("open /Users/ada/Secret Project/.env: permission denied")
	}
	rec := a.request("POST", "/api/drafts", textInput)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "/Users") || errorCode(t, rec) != "internal_error" {
		t.Fatalf("internal error leaked: %d %s", rec.Code, rec.Body)
	}
}

func TestListenerLoopback(t *testing.T) {
	l, err := listenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if addr := l.Addr().(*net.TCPAddr); !addr.IP.IsLoopback() {
		t.Fatalf("listening on %v", addr)
	}
	l.Close()
	for _, bad := range []string{"0.0.0.0:0", ":0", "[::]:0", "localhost:0"} {
		if l, err := listenLoopback(bad); err == nil {
			l.Close()
			t.Errorf("%s accepted", bad)
		}
	}
	l, err = ListenLoopback()
	if errors.Is(err, syscall.EADDRINUSE) {
		t.Skip("port 8080 is in use on this machine")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got := l.Addr().String(); got != "127.0.0.1:8080" {
		t.Fatalf("listening on %s", got)
	}
}

func TestAPIShutdown(t *testing.T) {
	server := newHTTPServer(http.NotFoundHandler())
	if server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 10*time.Second || server.WriteTimeout != 180*time.Second || server.IdleTimeout != 60*time.Second || server.MaxHeaderBytes != 16<<10 {
		t.Fatalf("server limits: %+v", server)
	}
	l, err := listenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunHTTP(ctx, l, http.NotFoundHandler()) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunHTTP: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunHTTP did not stop after cancellation")
	}
}

func TestAPIRequiresConfiguredToken(t *testing.T) {
	h := newApprovalHarness(t)
	handler := NewAPI(APIConfig{}, h.manager, fakeFreeze, &fakeSender{}, &fakeReads{})
	for _, header := range []string{"Bearer ", "Bearer", ""} {
		req := httptest.NewRequest("GET", "/api/status", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unconfigured token with %q: %d", header, rec.Code)
		}
	}
}
