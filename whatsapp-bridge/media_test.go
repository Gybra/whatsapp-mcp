package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var samples = map[string][]byte{
	"photo.jpg":  append([]byte{0xff, 0xd8, 0xff, 0xe0}, "jpeg body"...),
	"photo.png":  append([]byte("\x89PNG\r\n\x1a\n"), "png body"...),
	"anim.gif":   []byte("GIF89a gif body"),
	"pic.webp":   []byte("RIFF\x10\x00\x00\x00WEBPVP8 body"),
	"clip.mp4":   []byte("\x00\x00\x00\x18ftypisom mp4 body"),
	"report.pdf": []byte("%PDF-1.7 report"),
	"notes.txt":  []byte("Release notes ✓\n"),
	"voice.ogg":  opusSample(48000, 312),
}

func outboxFile(t *testing.T, dataDir, name string, data []byte) {
	t.Helper()
	outbox := filepath.Join(dataDir, "outbox")
	if err := EnsurePrivateDir(outbox); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outbox, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileInput(name string) SendInput {
	return SendInput{Kind: "file", Recipient: "12025550123", MediaID: name}
}

func TestMediaSupportedTypes(t *testing.T) {
	dir := privateDataDir(t)
	wantMIME := map[string]string{"photo.jpg": "image/jpeg", "photo.png": "image/png", "anim.gif": "image/gif", "pic.webp": "image/webp", "clip.mp4": "video/mp4", "report.pdf": "application/pdf", "notes.txt": "text/plain", "voice.ogg": "audio/ogg; codecs=opus"}
	for name, data := range samples {
		outboxFile(t, dir, name, data)
		draft, err := FreezeSend(context.Background(), dir, fileInput(name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		a := draft.Payload.Attachment
		if a.MIME != wantMIME[name] || a.ID != name || a.Size != int64(len(data)) || a.SHA256 != sha256Hex(data) || !bytes.Equal(draft.Media, data) {
			t.Errorf("%s: attachment %+v", name, a)
		}
		if draft.Payload.RecipientJID != "12025550123@s.whatsapp.net" {
			t.Errorf("%s: recipient %q", name, draft.Payload.RecipientJID)
		}
		if err := validateDraft(draft); err != nil {
			t.Errorf("%s: frozen draft is not consistent: %v", name, err)
		}
	}
	text, err := FreezeSend(context.Background(), dir, SendInput{Kind: "text", Recipient: "120363025555555555@g.us", Message: "hi"})
	if err != nil || text.Payload.RecipientJID != "120363025555555555@g.us" || text.Payload.Attachment != nil {
		t.Fatalf("text draft: %+v, %v", text, err)
	}
}

func TestMediaTraversal(t *testing.T) {
	dir := privateDataDir(t)
	os.WriteFile(filepath.Join(dir, "secret.pdf"), []byte("%PDF-1.7 outside the outbox"), 0o600)
	outboxFile(t, dir, "ok.pdf", samples["report.pdf"])
	for _, id := range []string{"../secret.pdf", "sub/ok.pdf", `..\secret.pdf`, "..", ".", "", "/etc/hosts", filepath.Join(dir, "secret.pdf"), ".hidden.pdf", "ok.pdf\x00.txt"} {
		if _, err := FreezeSend(context.Background(), dir, fileInput(id)); !errors.Is(err, ErrInvalidRequest) && !errors.Is(err, ErrMediaNotFound) {
			t.Errorf("%q: got %v", id, err)
		}
	}
	if _, err := FreezeSend(context.Background(), dir, fileInput("missing.pdf")); !errors.Is(err, ErrMediaNotFound) {
		t.Errorf("missing file: %v", err)
	}
}

func TestMediaSymlink(t *testing.T) {
	dir := privateDataDir(t)
	outbox := filepath.Join(dir, "outbox")
	outboxFile(t, dir, "real.pdf", samples["report.pdf"])
	os.WriteFile(filepath.Join(dir, "outside.pdf"), samples["report.pdf"], 0o600)
	if err := os.Symlink(filepath.Join(dir, "outside.pdf"), filepath.Join(outbox, "out.pdf")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	os.Symlink("real.pdf", filepath.Join(outbox, "in.pdf"))
	for _, id := range []string{"out.pdf", "in.pdf"} {
		if _, err := FreezeSend(context.Background(), dir, fileInput(id)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: symlink not rejected as a non-regular file: %v", id, err)
		}
	}
}

func TestMediaSize(t *testing.T) {
	dir := privateDataDir(t)
	exact := append([]byte("%PDF-1.7 "), make([]byte, maxMediaBytes-9)...)
	outboxFile(t, dir, "exact.pdf", exact)
	outboxFile(t, dir, "big.pdf", append(exact, 0))
	if _, err := FreezeSend(context.Background(), dir, fileInput("exact.pdf")); err != nil {
		t.Errorf("16 MiB file: %v", err)
	}
	if _, err := FreezeSend(context.Background(), dir, fileInput("big.pdf")); !errors.Is(err, ErrUnsupportedMedia) {
		t.Errorf("file over 16 MiB: %v", err)
	}
}

func TestMediaTypeMismatch(t *testing.T) {
	dir := privateDataDir(t)
	cases := map[string][]byte{
		"fake.pdf":   samples["photo.png"],
		"fake.png":   samples["report.pdf"],
		"tool.exe":   []byte("MZ\x90\x00"),
		"page.html":  []byte("<html>"),
		"binary.txt": []byte("text\x00with NUL"),
		"latin.txt":  []byte("caf\xe9"),
		"vorbis.ogg": append([]byte("OggS"), make([]byte, 60)...),
	}
	for name, data := range cases {
		outboxFile(t, dir, name, data)
		if _, err := FreezeSend(context.Background(), dir, fileInput(name)); !errors.Is(err, ErrUnsupportedMedia) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	outboxFile(t, dir, "doc.pdf", samples["report.pdf"])
	if _, err := FreezeSend(context.Background(), dir, SendInput{Kind: "voice", Recipient: "12025550123", MediaID: "doc.pdf"}); !errors.Is(err, ErrUnsupportedMedia) {
		t.Errorf("PDF as a voice message: %v", err)
	}
	outboxFile(t, dir, "voice.ogg", samples["voice.ogg"])
	if _, err := FreezeSend(context.Background(), dir, SendInput{Kind: "voice", Recipient: "12025550123", MediaID: "voice.ogg", Message: "caption"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("voice message with a caption: %v", err)
	}
}

func TestObviousSecretRejection(t *testing.T) {
	dir := privateDataDir(t)
	for _, name := range []string{".env", ".env.local", "id_rsa", "id_ed25519", "AUTH.JSON", "credentials.json", ".npmrc", ".pypirc"} {
		if err := RejectObviousSecrets(name, []byte("anything")); !errors.Is(err, ErrSecretRejected) {
			t.Errorf("%s: %v", name, err)
		}
	}
	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n"
	outboxFile(t, dir, "key.txt", []byte(pem))
	_, err := FreezeSend(context.Background(), dir, fileInput("key.txt"))
	if !errors.Is(err, ErrSecretRejected) || strings.Contains(err.Error(), "b3BlbnNzaC1rZXk") {
		t.Errorf("PEM key file: %v", err)
	}
	if _, err := FreezeSend(context.Background(), dir, SendInput{Kind: "text", Recipient: "12025550123", Message: "here you go\n" + pem}); !errors.Is(err, ErrSecretRejected) {
		t.Errorf("PEM key in a text message: %v", err)
	}
	outboxFile(t, dir, "public.txt", []byte("-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEA\n-----END PUBLIC KEY-----\n"))
	if _, err := FreezeSend(context.Background(), dir, fileInput("public.txt")); err != nil {
		t.Errorf("public key rejected: %v", err)
	}
}

func TestFrozenAttachment(t *testing.T) {
	dir := privateDataDir(t)
	original := samples["report.pdf"]
	outboxFile(t, dir, "report.pdf", original)
	draft, err := FreezeSend(context.Background(), dir, fileInput("report.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	h := newApprovalHarness(t)
	view := h.prepare(t, draft)

	path := filepath.Join(dir, "outbox", "report.pdf")
	os.WriteFile(path, []byte("%PDF-1.7 swapped after review"), 0o600) // rewritten during approval
	os.Remove(path)
	os.Symlink(filepath.Join(dir, "api-token"), path) // or replaced by a link

	sent, err := h.manager.Consume(draft.Input, signView(t, h.key, view))
	if err != nil || !bytes.Equal(sent.Media, original) || sent.Payload.Attachment.SHA256 != sha256Hex(original) {
		t.Fatalf("the sender would not get the reviewed bytes: %v", err)
	}
}

func TestVoiceFreezeConvertsCommonAudio(t *testing.T) {
	dir := privateDataDir(t)
	outboxFile(t, dir, "memo.mp3", []byte("ID3\x04\x00 mp3 data"))
	withFakeFFmpeg(t, func(ctx context.Context, executable string, args []string) error {
		return os.WriteFile(args[len(args)-1], opusSample(48000*2, 312), 0o600)
	})
	draft, err := FreezeSend(context.Background(), dir, SendInput{Kind: "voice", Recipient: "12025550123", MediaID: "memo.mp3"})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Payload.Attachment.MIME != "audio/ogg; codecs=opus" || draft.Payload.Attachment.Name != "memo.mp3" || validateDraft(draft) != nil {
		t.Fatalf("converted voice draft: %+v", draft.Payload.Attachment)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".voice-") {
			t.Errorf("temporary conversion files left behind: %s", e.Name())
		}
	}
	outboxFile(t, dir, "song.mp3", []byte("not really mp3"))
	if _, err := FreezeSend(context.Background(), dir, SendInput{Kind: "file", Recipient: "12025550123", MediaID: "song.mp3"}); !errors.Is(err, ErrUnsupportedMedia) {
		t.Errorf("mp3 as a file: %v", err)
	}
}

func TestDownloadCollision(t *testing.T) {
	dir := privateDataDir(t)
	first, err := SaveDownloadedMedia(dir, "../../photo.jpg", []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := SaveDownloadedMedia(dir, "../../photo.jpg", []byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	downloads := filepath.Join(dir, "downloads")
	for _, d := range []DownloadedMedia{first, second} {
		if filepath.Dir(d.Path) != downloads || !strings.HasSuffix(d.Path, ".jpg") || d.Name != "../../photo.jpg" {
			t.Errorf("download stored at %s (name %q)", d.Path, d.Name)
		}
		assertPrivate(t, d.Path, false)
	}
	if first.Path == second.Path || string(mustRead(t, first.Path)) != "one" || string(mustRead(t, second.Path)) != "two" {
		t.Fatal("a download overwrote another")
	}
	weird, err := SaveDownloadedMedia(dir, "no-extension\x00.exe/", []byte("three"))
	if err != nil || filepath.Ext(weird.Path) != "" || filepath.Dir(weird.Path) != downloads {
		t.Fatalf("odd name: %+v %v", weird, err)
	}
	if _, err := SaveDownloadedMedia(dir, "big.bin", make([]byte, maxMediaBytes+1)); err == nil {
		t.Fatal("oversized download stored")
	}
}
