package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	apiAddress       = "127.0.0.1:8080"
	maxRequestBytes  = 64 * 1024
	maxResponseBytes = 4 * 1024 * 1024
	sendTimeout      = 60 * time.Second
)

var (
	ErrUnauthorized     = errors.New("unauthorized")
	ErrApprovalRequired = errors.New("approval_required")
	ErrSendUncertain    = errors.New("send_uncertain")
	ErrNotFound         = errors.New("not_found")
	errInternal         = errors.New("internal_error")
)

// RuntimeStatus is the WhatsApp connection state, without account identifiers.
type RuntimeStatus struct {
	State     string `json:"state"`
	Connected bool   `json:"connected"`
}

type APIStatus struct {
	State             string `json:"state"`
	Connected         bool   `json:"connected"`
	ApproverID        string `json:"approver_id"`
	ApproverExpiresAt int64  `json:"approver_expires_at"`
}

type APIConfig struct {
	Token          []byte
	RevokeApprover func() error
	Status         func() RuntimeStatus
}

// ReadService answers the read-only tools. Unknown tools and arguments fail with ErrInvalidRequest.
type ReadService interface {
	Read(ctx context.Context, tool string, request json.RawMessage) (any, error)
	Download(ctx context.Context, messageID, chatJID string) (AttachmentView, error)
}

type SendRequest struct {
	Input    SendInput      `json:"input"`
	Approval *ApprovalProof `json:"approval"`
}

var (
	phoneRecipient = regexp.MustCompile(`^[1-9][0-9]{5,14}$`)
	jidRecipient   = regexp.MustCompile(`^(?:[1-9][0-9]{5,14}@s\.whatsapp\.net|[0-9]{6,20}(?:-[0-9]{6,12})?@g\.us|[0-9]{6,20}@lid)$`)
	readToolName   = regexp.MustCompile(`^[a-z_]{1,64}$`)
)

type api struct {
	tokenHex  []byte
	cfg       APIConfig
	approvals *ApprovalManager
	freeze    func(context.Context, SendInput) (FrozenDraft, error)
	sender    Sender
	reads     ReadService
}

// NewAPI returns the bridge's local HTTP API. Every route requires the bearer
// token, and the only route that sends requires a consumed approval.
func NewAPI(cfg APIConfig, approvals *ApprovalManager, freeze func(context.Context, SendInput) (FrozenDraft, error), sender Sender, reads ReadService) http.Handler {
	a := &api{cfg: cfg, approvals: approvals, freeze: freeze, sender: sender, reads: reads}
	if len(cfg.Token) == 32 {
		a.tokenHex = []byte(hex.EncodeToString(cfg.Token))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("POST /api/drafts", a.createDraft)
	mux.HandleFunc("GET /api/drafts/{id}/preview", a.preview)
	mux.HandleFunc("DELETE /api/drafts/{id}", a.cancelDraft)
	mux.HandleFunc("POST /api/send", a.send)
	mux.HandleFunc("DELETE /api/approver", a.revokeApprover)
	mux.HandleFunc("POST /api/read/{tool}", a.read)
	mux.HandleFunc("POST /api/download", a.download)
	return a.guard(mux)
}

// guard rejects browser and DNS-rebinding requests, then authenticates, before any routing or decoding.
func (a *api) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h := r.Header
		if r.Host != apiAddress || h.Get("Origin") != "" || h.Get("Sec-Fetch-Site") != "" || h.Get("Forwarded") != "" || h.Get("X-Forwarded-For") != "" {
			writeCode(w, http.StatusForbidden, ErrUnauthorized.Error(), "")
			return
		}
		scheme, token, _ := strings.Cut(h.Get("Authorization"), " ")
		if a.tokenHex == nil || scheme != "Bearer" || subtle.ConstantTimeCompare([]byte(token), a.tokenHex) != 1 {
			writeCode(w, http.StatusUnauthorized, ErrUnauthorized.Error(), "")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		next.ServeHTTP(w, r)
	})
}

func (a *api) status(w http.ResponseWriter, r *http.Request) {
	runtime := RuntimeStatus{State: "offline"}
	if a.cfg.Status != nil {
		runtime = a.cfg.Status()
	}
	status := APIStatus{State: runtime.State, Connected: runtime.Connected}
	if id, expiresAt, err := a.approvals.TrustedApprover(); err == nil {
		status.ApproverID, status.ApproverExpiresAt = id, expiresAt
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *api) createDraft(w http.ResponseWriter, r *http.Request) {
	var input SendInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := validateSendInput(input); err != nil {
		writeError(w, err)
		return
	}
	draft, err := a.freeze(r.Context(), input)
	if err != nil {
		writeError(w, err)
		return
	}
	view, err := a.approvals.Prepare(draft)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (a *api) preview(w http.ResponseWriter, r *http.Request) {
	media, err := a.approvals.ReadFrozenPreview(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	w.Write(media)
}

func (a *api) cancelDraft(w http.ResponseWriter, r *http.Request) {
	a.approvals.Cancel(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// send is the only route that reaches the Sender, and only with a consumed approval.
func (a *api) send(w http.ResponseWriter, r *http.Request) {
	var req SendRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validateSendInput(req.Input); err != nil {
		writeError(w, err)
		return
	}
	if req.Approval == nil {
		writeError(w, ErrApprovalRequired)
		return
	}
	draft, err := a.approvals.Consume(req.Input, *req.Approval)
	if err != nil {
		writeError(w, err)
		return
	}
	// A disconnecting client must not abort a send halfway; the approval is spent either way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), sendTimeout)
	defer cancel()
	receipt, err := a.sender.Send(ctx, draft)
	if err != nil {
		if !errors.Is(err, ErrClosed) {
			err = ErrSendUncertain
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

// revokeApprover only removes trust; enrolling an approver is not possible through the API.
func (a *api) revokeApprover(w http.ResponseWriter, r *http.Request) {
	a.approvals.Revoke()
	if a.cfg.RevokeApprover != nil {
		if err := a.cfg.RevokeApprover(); err != nil {
			writeError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) read(w http.ResponseWriter, r *http.Request) {
	tool := r.PathValue("tool")
	var request json.RawMessage
	if !decodeJSON(w, r, &request) {
		return
	}
	if !readToolName.MatchString(tool) || !bytes.HasPrefix(bytes.TrimSpace(request), []byte("{")) {
		writeError(w, ErrInvalidRequest)
		return
	}
	if a.reads == nil {
		writeError(w, ErrClosed)
		return
	}
	result, err := a.reads.Read(r.Context(), tool, request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *api) download(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MessageID string `json:"message_id"`
		ChatJID   string `json:"chat_jid"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.MessageID == "" || len(req.MessageID) > 128 || !jidRecipient.MatchString(req.ChatJID) {
		writeError(w, ErrInvalidRequest)
		return
	}
	if a.reads == nil {
		writeError(w, ErrClosed)
		return
	}
	attachment, err := a.reads.Download(r.Context(), req.MessageID, req.ChatJID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attachment)
}

func validateSendInput(in SendInput) error {
	recipientOK := phoneRecipient.MatchString(in.Recipient) || jidRecipient.MatchString(in.Recipient)
	if !recipientOK || len(in.Message) > maxMessageBytes || !utf8.ValidString(in.Message) {
		return ErrInvalidRequest
	}
	switch in.Kind {
	case "text":
		if in.Message == "" || in.MediaID != "" {
			return ErrInvalidRequest
		}
	case "file", "voice":
		if in.MediaID == "" || len(in.MediaID) > 128 || strings.ContainsAny(in.MediaID, `/\`) || in.MediaID == "." || in.MediaID == ".." {
			return ErrInvalidRequest
		}
	default:
		return ErrInvalidRequest
	}
	return nil
}

// decodeJSON reads exactly one JSON value with no unknown fields; on failure it writes the error.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeCode(w, http.StatusUnsupportedMediaType, ErrInvalidRequest.Error(), "")
		return false
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(dst)
	if err == nil {
		if err = decoder.Decode(&struct{}{}); err == io.EOF {
			return true
		}
	}
	status := http.StatusBadRequest
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		status = http.StatusRequestEntityTooLarge
	}
	writeCode(w, status, ErrInvalidRequest.Error(), "")
	return false
}

var errorStatus = []struct {
	err    error
	status int
}{
	{ErrInvalidRequest, http.StatusBadRequest},
	{ErrNotFound, http.StatusNotFound},
	{ErrNotPaired, http.StatusConflict},
	{ErrApprovalRequired, http.StatusForbidden},
	{ErrApprovalInvalid, http.StatusForbidden},
	{ErrApprovalExpired, http.StatusForbidden},
	{ErrApprovalUsed, http.StatusForbidden},
	{ErrBusy, http.StatusTooManyRequests},
	{ErrClosed, http.StatusServiceUnavailable},
	{ErrSendUncertain, http.StatusBadGateway},
}

// writeError replies with a fixed error code; underlying error details never reach the client.
func writeError(w http.ResponseWriter, err error) {
	for _, known := range errorStatus {
		if errors.Is(err, known.err) {
			writeCode(w, known.status, known.err.Error(), "")
			return
		}
	}
	writeCode(w, http.StatusInternalServerError, errInternal.Error(), "")
}

func writeCode(w http.ResponseWriter, status int, code, message string) {
	body := map[string]string{"error": code}
	if message != "" {
		body["message"] = message
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":"internal_error"}`)
	} else if len(data) > maxResponseBytes {
		status, data = http.StatusRequestEntityTooLarge, []byte(`{"error":"invalid_request","message":"result too large; narrow the query"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}

// ListenLoopback listens on the fixed API address. It never binds a non-loopback interface.
func ListenLoopback() (net.Listener, error) {
	return listenLoopback(apiAddress)
}

func listenLoopback(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || ip.To4() == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("refusing to listen on %q: the API is loopback-only", address)
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s (is another bridge already running?): %w", address, err)
	}
	return listener, nil
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      180 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// RunHTTP serves until ctx is cancelled, then shuts down gracefully.
func RunHTTP(ctx context.Context, listener net.Listener, handler http.Handler) error {
	server := newHTTPServer(handler)
	stop := context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	})
	defer stop()
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
