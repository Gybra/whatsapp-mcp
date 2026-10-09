package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

// Approval protocol v1 (docs/approval-protocol-v1.md): the bridge sends only an
// operation that an explicitly enrolled approver signed after reviewing its
// exact frozen content. Grants live in memory, expire and are single use.
const (
	approvalDomain   = "whatsapp-mcp/approval/v1"
	approvalLifetime = 120 * time.Second
	maxPendingDrafts = 32
	maxMessageBytes  = 32 * 1024
	maxMediaBytes    = 16 * 1024 * 1024
)

// Error values double as the API error codes returned to clients.
var (
	ErrInvalidRequest  = errors.New("invalid_request")
	ErrNotPaired       = errors.New("not_paired")
	ErrApprovalInvalid = errors.New("approval_invalid")
	ErrApprovalExpired = errors.New("approval_expired")
	ErrApprovalUsed    = errors.New("approval_used")
	ErrBusy            = errors.New("busy")
	ErrClosed          = errors.New("offline")
)

// SendInput is the outgoing operation exactly as a client submitted it.
type SendInput struct {
	Kind      string `json:"kind"`
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaID   string `json:"media_id"`
}

type AttachmentView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	MIME   string `json:"mime"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ApprovalPayload is what the approver reviews. Field order defines the signed bytes.
type ApprovalPayload struct {
	Version      int             `json:"version"`
	DraftID      string          `json:"draft_id"`
	RecipientJID string          `json:"recipient_jid"`
	Message      string          `json:"message"`
	Kind         string          `json:"kind"`
	Attachment   *AttachmentView `json:"attachment"`
	ExpiresAt    int64           `json:"expires_at"`
}

// FrozenDraft holds the validated operation and the exact bytes that will be sent.
type FrozenDraft struct {
	Input   SendInput
	Payload ApprovalPayload
	Media   []byte
}

type DraftView struct {
	DraftID      string `json:"draft_id"`
	PayloadB64   string `json:"payload_b64"`
	ChallengeB64 string `json:"challenge_b64"`
	ApproverID   string `json:"approver_id"`
	ExpiresAt    int64  `json:"expires_at"`
}

type ApprovalProof struct {
	DraftID      string `json:"draft_id"`
	SignatureB64 string `json:"signature_b64"`
}

// ApproverLease is the public half of the approver enrolled through the trusted console.
// It has no expiry: the private half lives only in the approver's process, and the
// approver revokes the lease when it exits. A newer pairing replaces it.
type ApproverLease struct {
	PublicKeyB64 string `json:"public_key_b64"`
}

type ApproverLookup func() (ApproverLease, error)

type SendReceipt struct {
	Status    string `json:"status"`
	MessageID string `json:"message_id"`
}

// Sender delivers a consumed draft. It is the only way the bridge sends user-visible messages.
type Sender interface {
	Send(ctx context.Context, draft FrozenDraft) (SendReceipt, error)
}

type grant struct {
	draft     FrozenDraft
	publicKey ed25519.PublicKey
	challenge []byte
	expiresAt time.Time
	used      bool
}

type ApprovalManager struct {
	mu     sync.Mutex
	now    func() time.Time
	random io.Reader
	lookup ApproverLookup
	grants map[string]*grant
	closed bool
}

func NewApprovalManager(now func() time.Time, random io.Reader, lookup ApproverLookup) *ApprovalManager {
	return &ApprovalManager{now: now, random: random, lookup: lookup, grants: map[string]*grant{}}
}

// Prepare freezes a validated draft for review and returns what the approver signs.
func (m *ApprovalManager) Prepare(draft FrozenDraft) (DraftView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return DraftView{}, ErrClosed
	}
	if err := validateDraft(draft); err != nil {
		return DraftView{}, err
	}
	now := m.now()
	publicKey, err := m.trustedKey()
	if err != nil {
		return DraftView{}, err
	}
	m.prune(now)
	if m.pendingCount() >= maxPendingDrafts {
		return DraftView{}, ErrBusy
	}

	random := make([]byte, 16+32)
	if _, err := io.ReadFull(m.random, random); err != nil {
		return DraftView{}, fmt.Errorf("approval randomness: %w", err)
	}
	frozen := cloneDraft(draft)
	frozen.Payload.Version = 1
	frozen.Payload.DraftID = hex.EncodeToString(random[:16])
	frozen.Payload.ExpiresAt = now.Add(approvalLifetime).Unix()
	payload, err := json.Marshal(frozen.Payload)
	if err != nil {
		return DraftView{}, fmt.Errorf("approval payload: %w", err)
	}
	challenge := approvalChallenge(random[16:], payload, publicKey)

	m.grants[frozen.Payload.DraftID] = &grant{
		draft:     frozen,
		publicKey: publicKey,
		challenge: challenge,
		expiresAt: time.Unix(frozen.Payload.ExpiresAt, 0),
	}
	return DraftView{
		DraftID:      frozen.Payload.DraftID,
		PayloadB64:   base64.StdEncoding.EncodeToString(payload),
		ChallengeB64: base64.StdEncoding.EncodeToString(challenge),
		ApproverID:   approverID(publicKey),
		ExpiresAt:    frozen.Payload.ExpiresAt,
	}, nil
}

// Consume verifies a proof for exactly this operation and spends it. A valid
// signature is spent even when the operation does not match, so a proof can
// never be replayed; the caller owns the returned frozen draft.
func (m *ApprovalManager) Consume(input SendInput, proof ApprovalProof) (FrozenDraft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return FrozenDraft{}, ErrClosed
	}
	g := m.grants[proof.DraftID]
	if g == nil {
		return FrozenDraft{}, ErrApprovalInvalid
	}
	if g.used {
		return FrozenDraft{}, ErrApprovalUsed
	}
	now := m.now()
	if !now.Before(g.expiresAt) {
		delete(m.grants, proof.DraftID)
		return FrozenDraft{}, ErrApprovalExpired
	}
	publicKey, err := m.trustedKey()
	if err != nil {
		return FrozenDraft{}, err
	}
	signature, ok := decodeCanonicalB64(proof.SignatureB64)
	if !publicKey.Equal(g.publicKey) || !ok || len(signature) != ed25519.SignatureSize ||
		!ed25519.Verify(g.publicKey, g.challenge, signature) {
		return FrozenDraft{}, ErrApprovalInvalid
	}

	draft := g.draft
	g.used, g.draft = true, FrozenDraft{} // keep a tombstone, not the bytes
	if input != draft.Input {
		return FrozenDraft{}, ErrApprovalInvalid
	}
	return draft, nil
}

// TrustedApprover reports the enrolled approver's fingerprint, never key material.
func (m *ApprovalManager) TrustedApprover() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	publicKey, err := m.trustedKey()
	if err != nil {
		return "", err
	}
	return approverID(publicKey), nil
}

// ReadFrozenPreview returns a copy of the exact attachment bytes of a pending draft, for review.
func (m *ApprovalManager) ReadFrozenPreview(draftID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.grants[draftID]
	if g == nil || g.used || g.draft.Payload.Attachment == nil || !m.now().Before(g.expiresAt) {
		return nil, ErrNotFound
	}
	return append([]byte(nil), g.draft.Media...), nil
}

func (m *ApprovalManager) Cancel(draftID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.grants, draftID)
}

// Revoke invalidates every outstanding draft, for example when the approver leaves.
func (m *ApprovalManager) Revoke() {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.grants)
}

func (m *ApprovalManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	clear(m.grants)
}

func (m *ApprovalManager) trustedKey() (ed25519.PublicKey, error) {
	lease, err := m.lookup()
	if err != nil {
		return nil, ErrNotPaired
	}
	key, ok := decodeCanonicalB64(lease.PublicKeyB64)
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, ErrNotPaired
	}
	return ed25519.PublicKey(key), nil
}

func (m *ApprovalManager) prune(now time.Time) {
	for id, g := range m.grants {
		if !now.Before(g.expiresAt) {
			delete(m.grants, id)
		}
	}
}

func (m *ApprovalManager) pendingCount() int {
	count := 0
	for _, g := range m.grants {
		if !g.used {
			count++
		}
	}
	return count
}

func validateDraft(d FrozenDraft) error {
	in, p := d.Input, d.Payload
	invalid := in.Kind != p.Kind || in.Message != p.Message || in.Recipient == "" || p.RecipientJID == "" ||
		len(in.Message) > maxMessageBytes || !utf8.ValidString(in.Message) || len(d.Media) > maxMediaBytes
	if invalid {
		return ErrInvalidRequest
	}
	switch in.Kind {
	case "text":
		if in.Message == "" || in.MediaID != "" || p.Attachment != nil || len(d.Media) != 0 {
			return ErrInvalidRequest
		}
	case "file", "voice":
		a := p.Attachment
		if a == nil || in.MediaID == "" || a.ID != in.MediaID || a.Size != int64(len(d.Media)) ||
			a.SHA256 != sha256Hex(d.Media) {
			return ErrInvalidRequest
		}
	default:
		return ErrInvalidRequest
	}
	return nil
}

func cloneDraft(d FrozenDraft) FrozenDraft {
	d.Media = append([]byte(nil), d.Media...)
	if d.Payload.Attachment != nil {
		attachment := *d.Payload.Attachment
		d.Payload.Attachment = &attachment
	}
	return d
}

func approvalChallenge(nonce, payload []byte, publicKey ed25519.PublicKey) []byte {
	payloadSum := sha256.Sum256(payload)
	keySum := sha256.Sum256(publicKey)
	challenge := append([]byte(approvalDomain), 0)
	challenge = append(challenge, nonce...)
	challenge = append(challenge, payloadSum[:]...)
	return append(challenge, keySum[:]...)
}

func approverID(publicKey ed25519.PublicKey) string {
	return sha256Hex(publicKey)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// decodeCanonicalB64 accepts only standard padded base64 that re-encodes identically,
// rejecting the whitespace and alternate forms the stdlib decoder tolerates.
func decodeCanonicalB64(s string) ([]byte, bool) {
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil || s == "" || base64.StdEncoding.EncodeToString(data) != s {
		return nil, false
	}
	return data, true
}
