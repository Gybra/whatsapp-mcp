package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

func TestQRModules(t *testing.T) {
	modules, err := qrModules("2@ABCDEFGHIJKLMNOPQRSTUVWXYZ,abcdefghijklmnopqrstuvwxyz0123456789+/=,0123456789,ABCDEFGHIJ")
	if err != nil {
		t.Fatal(err)
	}
	size := len(modules)
	if size < 21+2*qrQuietZone {
		t.Fatalf("too small: %d", size)
	}
	for i, row := range modules {
		if len(row) != size || strings.Trim(row, "01") != "" {
			t.Fatalf("row %d malformed: %q", i, row)
		}
	}
	if strings.Contains(modules[0], "1") || strings.Contains(modules[size-1], "1") {
		t.Fatal("no quiet zone")
	}
	// Every QR code has a finder pattern: a dark 7x7 square in the top-left corner.
	if modules[qrQuietZone][qrQuietZone:qrQuietZone+7] != "1111111" {
		t.Fatalf("finder pattern missing: %q", modules[qrQuietZone])
	}

	lines := renderQR(modules)
	if len(lines) != (size+1)/2 {
		t.Fatalf("%d rendered lines for %d rows", len(lines), size)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "\x1b[30;47m") || !strings.HasSuffix(line, "\x1b[0m") {
			t.Fatalf("line without explicit dark-on-light colours: %q", line)
		}
	}
}

func items(list ...whatsmeow.QRChannelItem) <-chan whatsmeow.QRChannelItem {
	ch := make(chan whatsmeow.QRChannelItem, len(list))
	for _, item := range list {
		ch <- item
	}
	close(ch)
	return ch
}

func code(c string) whatsmeow.QRChannelItem {
	return whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: c, Timeout: 20 * time.Second}
}

func TestLinkSuccess(t *testing.T) {
	var got []LinkEvent
	err := runLink(context.Background(), items(code("first"), code("second"), whatsmeow.QRChannelSuccess),
		func(e LinkEvent) { got = append(got, e) }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Event != "qr" || got[1].Event != "qr" || got[2].Event != "linked" || got[0].ExpiresIn != 20 {
		t.Fatalf("events: %+v", got)
	}
	if len(got[0].Modules) == 0 || strings.Join(got[0].Modules, "") == strings.Join(got[1].Modules, "") {
		t.Fatal("each new code must produce a new QR")
	}
	for _, e := range got {
		if strings.Contains(e.Message, "first") {
			t.Fatal("the raw pairing code must not be emitted")
		}
	}
}

func TestLinkFailures(t *testing.T) {
	cases := map[string]struct {
		item whatsmeow.QRChannelItem
		want string
	}{
		"expired":  {whatsmeow.QRChannelTimeout, "expired"},
		"outdated": {whatsmeow.QRChannelClientOutdated, "update"},
		"passkey":  {whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyRequest}, "passkey"},
		"error":    {whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: errors.New("pair rejected")}, "pair rejected"},
	}
	for name, c := range cases {
		err := runLink(context.Background(), items(code("a"), c.item), func(LinkEvent) {}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := runLink(context.Background(), items(code("a")), func(LinkEvent) {}, nil, nil); err == nil {
		t.Error("a channel closed without success must fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runLink(ctx, make(chan whatsmeow.QRChannelItem), func(LinkEvent) {}, nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestLinkScannedWithoutMultidevice(t *testing.T) {
	var got []LinkEvent
	err := runLink(context.Background(), items(code("a"), whatsmeow.QRChannelScannedWithoutMultidevice, whatsmeow.QRChannelSuccess),
		func(e LinkEvent) { got = append(got, e) }, nil, nil)
	if err != nil || got[1].Event != "notice" || got[2].Event != "linked" {
		t.Fatalf("scan without multi-device should be a notice, then continue: %v %+v", err, got)
	}
}

func TestLinkPasskeyConfirmation(t *testing.T) {
	confirmation := whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyResponse, PasskeyConfirmation: &events.PairPasskeyConfirmation{Code: "123456"}}
	for _, accept := range []bool{true, false} {
		var shown string
		sent := 0
		var got []LinkEvent
		err := runLink(context.Background(), items(code("a"), confirmation, whatsmeow.QRChannelSuccess),
			func(e LinkEvent) { got = append(got, e) },
			func(c string) (bool, error) { shown = c; return accept, nil },
			func(context.Context) error { sent++; return nil })
		if shown != "123456" {
			t.Fatalf("code not shown for confirmation: %q", shown)
		}
		if accept && (err != nil || sent != 1) {
			t.Fatalf("accepted: %v, sent %d", err, sent)
		}
		if !accept && (err == nil || sent != 0) {
			t.Fatalf("refused: %v, sent %d", err, sent)
		}
		if got[1].Event != "confirm_code" || got[1].Code != "123456" {
			t.Fatalf("events: %+v", got)
		}
	}
	if err := runLink(context.Background(), items(confirmation), func(LinkEvent) {}, nil, nil); err == nil {
		t.Fatal("a confirmation request without a way to confirm must fail, not auto-confirm")
	}
}
