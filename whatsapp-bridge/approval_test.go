package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var updateFixture = flag.Bool("update", false, "rewrite testdata/approval-v1.json")

// RFC 8032 section 7.1 test 1 secret key: public, never a real approver key.
const testSeedHex = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"

type approvalHarness struct {
	manager *ApprovalManager
	now     time.Time
	key     ed25519.PrivateKey
	lease   *ApproverLease
	leaseOK bool
	mu      sync.Mutex
}

func newApprovalHarness(t *testing.T) *approvalHarness {
	t.Helper()
	seed, _ := hex.DecodeString(testSeedHex)
	h := &approvalHarness{now: time.Unix(1_800_000_000, 0), key: ed25519.NewKeyFromSeed(seed), leaseOK: true}
	h.lease = &ApproverLease{PublicKeyB64: publicKeyB64(h.key), ExpiresAt: h.now.Unix() + 3600}
	h.manager = NewApprovalManager(h.clock, rand.Reader, h.lookup)
	return h
}

func (h *approvalHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *approvalHarness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

func (h *approvalHarness) lookup() (ApproverLease, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.leaseOK {
		return ApproverLease{}, errors.New("no approver")
	}
	return *h.lease, nil
}

func (h *approvalHarness) prepare(t *testing.T, draft FrozenDraft) DraftView {
	t.Helper()
	view, err := h.manager.Prepare(draft)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return view
}

func publicKeyB64(key ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
}

func signView(t *testing.T, key ed25519.PrivateKey, view DraftView) ApprovalProof {
	t.Helper()
	challenge, err := base64.StdEncoding.DecodeString(view.ChallengeB64)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	return ApprovalProof{DraftID: view.DraftID, SignatureB64: base64.StdEncoding.EncodeToString(ed25519.Sign(key, challenge))}
}

func textDraft(message string) FrozenDraft {
	return FrozenDraft{
		Input:   SendInput{Kind: "text", Recipient: "12025550123", Message: message},
		Payload: ApprovalPayload{RecipientJID: "12025550123@s.whatsapp.net", Message: message, Kind: "text"},
	}
}

func fileDraft(caption string, data []byte) FrozenDraft {
	sum := sha256.Sum256(data)
	return FrozenDraft{
		Input: SendInput{Kind: "file", Recipient: "12025550123", Message: caption, MediaID: "report.pdf"},
		Payload: ApprovalPayload{
			RecipientJID: "12025550123@s.whatsapp.net", Message: caption, Kind: "file",
			Attachment: &AttachmentView{ID: "report.pdf", Name: "report.pdf", MIME: "application/pdf", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])},
		},
		Media: data,
	}
}

func TestApprovalRejectsUnsigned(t *testing.T) {
	h := newApprovalHarness(t)
	draft := textDraft("hello")
	view := h.prepare(t, draft)

	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	forged := signView(t, otherKey, view)
	bad := []ApprovalProof{
		{DraftID: view.DraftID},
		{DraftID: view.DraftID, SignatureB64: "not base64!"},
		{DraftID: view.DraftID, SignatureB64: base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))},
		forged,
		{DraftID: "unknown", SignatureB64: signView(t, h.key, view).SignatureB64},
	}
	for i, proof := range bad {
		if _, err := h.manager.Consume(draft.Input, proof); !errors.Is(err, ErrApprovalInvalid) {
			t.Fatalf("bad proof %d: got %v, want ErrApprovalInvalid", i, err)
		}
	}
	// Invalid attempts must not burn the genuine approval.
	if _, err := h.manager.Consume(draft.Input, signView(t, h.key, view)); err != nil {
		t.Fatalf("genuine proof after forgeries: %v", err)
	}
}

func TestApprovalBindsOperation(t *testing.T) {
	changes := map[string]func(*SendInput){
		"recipient": func(in *SendInput) { in.Recipient = "12025550124" },
		"message":   func(in *SendInput) { in.Message = "caption!" },
		"kind":      func(in *SendInput) { in.Kind = "voice" },
		"media":     func(in *SendInput) { in.MediaID = "other.pdf" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			h := newApprovalHarness(t)
			draft := fileDraft("caption", []byte("%PDF-1.4 frozen"))
			view := h.prepare(t, draft)
			proof := signView(t, h.key, view)
			changed := draft.Input
			change(&changed)
			if _, err := h.manager.Consume(changed, proof); !errors.Is(err, ErrApprovalInvalid) {
				t.Fatalf("changed %s: got %v, want ErrApprovalInvalid", name, err)
			}
			if _, err := h.manager.Consume(draft.Input, proof); err == nil {
				t.Fatal("a proof presented with a tampered operation must not be reusable")
			}
		})
	}

	t.Run("frozen bytes are copied", func(t *testing.T) {
		h := newApprovalHarness(t)
		draft := fileDraft("caption", []byte("%PDF-1.4 frozen"))
		view := h.prepare(t, draft)
		draft.Media[0] = 'X'
		draft.Payload.Attachment.SHA256 = strings.Repeat("0", 64)
		got, err := h.manager.Consume(draft.Input, signView(t, h.key, view))
		if err != nil {
			t.Fatalf("Consume: %v", err)
		}
		if string(got.Media) != "%PDF-1.4 frozen" {
			t.Fatalf("sender would get caller-mutated bytes: %q", got.Media)
		}
		if got.Payload.Attachment.SHA256 == strings.Repeat("0", 64) {
			t.Fatal("attachment metadata was not copied")
		}
	})

	inconsistent := map[string]func(*FrozenDraft){
		"attachment hash": func(d *FrozenDraft) { d.Media = []byte("%PDF-1.4 swapped") },
		"attachment size": func(d *FrozenDraft) { d.Payload.Attachment.Size++ },
		"payload message": func(d *FrozenDraft) { d.Payload.Message = "other" },
		"payload kind":    func(d *FrozenDraft) { d.Payload.Kind = "voice" },
		"missing file":    func(d *FrozenDraft) { d.Payload.Attachment = nil },
		"unknown kind":    func(d *FrozenDraft) { d.Input.Kind, d.Payload.Kind = "poll", "poll" },
		"empty recipient": func(d *FrozenDraft) { d.Payload.RecipientJID = "" },
	}
	for name, corrupt := range inconsistent {
		t.Run("rejects "+name, func(t *testing.T) {
			h := newApprovalHarness(t)
			draft := fileDraft("caption", []byte("%PDF-1.4 frozen"))
			corrupt(&draft)
			if _, err := h.manager.Prepare(draft); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("got %v, want ErrInvalidRequest", err)
			}
		})
	}
	t.Run("rejects text with attachment", func(t *testing.T) {
		h := newApprovalHarness(t)
		draft := textDraft("hi")
		draft.Media = []byte("x")
		if _, err := h.manager.Prepare(draft); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("got %v, want ErrInvalidRequest", err)
		}
	})
}

func TestApprovalExpires(t *testing.T) {
	h := newApprovalHarness(t)
	start := h.clock().Unix()
	early, late := textDraft("early"), textDraft("late")
	earlyView, lateView := h.prepare(t, early), h.prepare(t, late)
	if earlyView.ExpiresAt != start+120 {
		t.Fatalf("expires_at = %d, want %d", earlyView.ExpiresAt, start+120)
	}
	h.advance(119 * time.Second)
	if _, err := h.manager.Consume(early.Input, signView(t, h.key, earlyView)); err != nil {
		t.Fatalf("at 119s: %v", err)
	}
	h.advance(time.Second)
	if _, err := h.manager.Consume(late.Input, signView(t, h.key, lateView)); !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("at 120s: got %v, want ErrApprovalExpired", err)
	}
}

func TestApprovalConsumesOnce(t *testing.T) {
	h := newApprovalHarness(t)
	draft := textDraft("once")
	proof := signView(t, h.key, h.prepare(t, draft))
	if _, err := h.manager.Consume(draft.Input, proof); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	// Whatever the sender then reports, the approval is spent: a retry needs a new review.
	if _, err := h.manager.Consume(draft.Input, proof); !errors.Is(err, ErrApprovalUsed) {
		t.Fatalf("second consume: got %v, want ErrApprovalUsed", err)
	}
	// An identical message is a new operation and needs its own approval.
	again := h.prepare(t, draft)
	if _, err := h.manager.Consume(draft.Input, ApprovalProof{DraftID: again.DraftID, SignatureB64: proof.SignatureB64}); !errors.Is(err, ErrApprovalInvalid) {
		t.Fatalf("old signature on new draft: got %v, want ErrApprovalInvalid", err)
	}
}

func TestApprovalConcurrentConsume(t *testing.T) {
	h := newApprovalHarness(t)
	draft := textDraft("race")
	proof := signView(t, h.key, h.prepare(t, draft))
	var wg sync.WaitGroup
	var successes atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.manager.Consume(draft.Input, proof); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful consumes = %d, want 1", got)
	}
}

func TestApprovalRevocation(t *testing.T) {
	cases := map[string]func(h *approvalHarness, view DraftView){
		"cancel":        func(h *approvalHarness, view DraftView) { h.manager.Cancel(view.DraftID) },
		"revoke":        func(h *approvalHarness, _ DraftView) { h.manager.Revoke() },
		"close":         func(h *approvalHarness, _ DraftView) { h.manager.Close() },
		"lease removed": func(h *approvalHarness, _ DraftView) { h.leaseOK = false },
		"lease expired": func(h *approvalHarness, _ DraftView) { h.lease.ExpiresAt = h.now.Unix() },
		"lease replaced": func(h *approvalHarness, _ DraftView) {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			h.lease.PublicKeyB64 = publicKeyB64(other)
		},
	}
	for name, revoke := range cases {
		t.Run(name, func(t *testing.T) {
			h := newApprovalHarness(t)
			draft := textDraft("revoked")
			view := h.prepare(t, draft)
			proof := signView(t, h.key, view)
			revoke(h, view)
			if _, err := h.manager.Consume(draft.Input, proof); err == nil {
				t.Fatal("approval survived revocation")
			}
		})
	}

	t.Run("prepare requires a trusted approver", func(t *testing.T) {
		h := newApprovalHarness(t)
		h.leaseOK = false
		if _, err := h.manager.Prepare(textDraft("x")); !errors.Is(err, ErrNotPaired) {
			t.Fatalf("got %v, want ErrNotPaired", err)
		}
	})
	t.Run("closed manager", func(t *testing.T) {
		h := newApprovalHarness(t)
		h.manager.Close()
		if _, err := h.manager.Prepare(textDraft("x")); err == nil {
			t.Fatal("Prepare after Close succeeded")
		}
	})
	t.Run("trusted approver identity", func(t *testing.T) {
		h := newApprovalHarness(t)
		id, expires, err := h.manager.TrustedApprover()
		pub := sha256.Sum256(h.key.Public().(ed25519.PublicKey))
		if err != nil || id != hex.EncodeToString(pub[:]) || expires != h.lease.ExpiresAt {
			t.Fatalf("TrustedApprover = %q, %d, %v", id, expires, err)
		}
		for _, bad := range []string{"", "%%%", base64.StdEncoding.EncodeToString(make([]byte, 31))} {
			h.lease.PublicKeyB64 = bad
			if _, _, err := h.manager.TrustedApprover(); !errors.Is(err, ErrNotPaired) {
				t.Fatalf("malformed lease %q: got %v, want ErrNotPaired", bad, err)
			}
		}
	})
}

func TestApprovalLimits(t *testing.T) {
	h := newApprovalHarness(t)
	var first DraftView
	for i := range 32 {
		view := h.prepare(t, textDraft("pending"))
		if i == 0 {
			first = view
		}
	}
	if _, err := h.manager.Prepare(textDraft("pending")); !errors.Is(err, ErrBusy) {
		t.Fatalf("33rd draft: got %v, want ErrBusy", err)
	}
	h.manager.Cancel(first.DraftID)
	h.prepare(t, textDraft("after cancel"))

	h.manager.Revoke()
	tooLong := strings.Repeat("a", 32*1024+1)
	for name, draft := range map[string]FrozenDraft{
		"long message": textDraft(tooLong),
		"invalid utf8": textDraft("\xff"),
		"large media":  fileDraft("", make([]byte, 16*1024*1024+1)),
	} {
		if _, err := h.manager.Prepare(draft); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s: got %v, want ErrInvalidRequest", name, err)
		}
	}
	h.prepare(t, textDraft(strings.Repeat("a", 32*1024)))
}

type approvalFixture struct {
	Description  string    `json:"description"`
	Now          int64     `json:"now"`
	PublicKeyB64 string    `json:"public_key_b64"`
	ApproverID   string    `json:"approver_id"`
	Input        SendInput `json:"input"`
	MediaB64     string    `json:"media_b64"`
	DraftID      string    `json:"draft_id"`
	PayloadB64   string    `json:"payload_b64"`
	ChallengeB64 string    `json:"challenge_b64"`
	ExpiresAt    int64     `json:"expires_at"`
	SignatureB64 string    `json:"signature_b64"`
}

func TestProtocolVector(t *testing.T) {
	h := newApprovalHarness(t)
	sequential := make([]byte, 48)
	for i := range sequential {
		sequential[i] = byte(i)
	}
	h.manager = NewApprovalManager(h.clock, bytes.NewReader(sequential), h.lookup)
	media := []byte("fixture attachment\n")
	draft := fileDraft("Ciao Davide — test & <ok> ✓", media)
	view := h.prepare(t, draft)
	proof := signView(t, h.key, view)

	got := approvalFixture{
		Description:  "whatsapp-mcp approval v1 vector. RFC 8032 test-1 key; fictional recipient. Never a live key.",
		Now:          h.clock().Unix(),
		PublicKeyB64: h.lease.PublicKeyB64,
		ApproverID:   view.ApproverID,
		Input:        draft.Input,
		MediaB64:     base64.StdEncoding.EncodeToString(media),
		DraftID:      view.DraftID,
		PayloadB64:   view.PayloadB64,
		ChallengeB64: view.ChallengeB64,
		ExpiresAt:    view.ExpiresAt,
		SignatureB64: proof.SignatureB64,
	}
	encoded, _ := json.MarshalIndent(got, "", "  ")
	encoded = append(encoded, '\n')
	if *updateFixture {
		if err := os.WriteFile("testdata/approval-v1.json", encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/approval-v1.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("protocol output drifted from fixture:\n%s", encoded)
	}

	payload, _ := base64.StdEncoding.DecodeString(view.PayloadB64)
	if !bytes.HasPrefix(payload, []byte(`{"version":1,"draft_id":"`)) {
		t.Fatalf("payload field order changed: %s", payload)
	}
	challenge, _ := base64.StdEncoding.DecodeString(view.ChallengeB64)
	payloadSum := sha256.Sum256(payload)
	keySum := sha256.Sum256(h.key.Public().(ed25519.PublicKey))
	wantChallenge := append([]byte("whatsapp-mcp/approval/v1\x00"), sequential[16:48]...)
	wantChallenge = append(append(wantChallenge, payloadSum[:]...), keySum[:]...)
	if !bytes.Equal(challenge, wantChallenge) {
		t.Fatal("challenge framing differs from the documented construction")
	}

	sent, err := h.manager.Consume(draft.Input, proof)
	if err != nil || !bytes.Equal(sent.Media, media) {
		t.Fatalf("fixture proof did not authorize the frozen draft: %v", err)
	}
}
