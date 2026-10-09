package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow"
	"rsc.io/qr"
)

const qrQuietZone = 2

// LinkEvent is one step of linking this computer as a WhatsApp device. With
// --json each event is printed as one line, for the Pi extension to display.
type LinkEvent struct {
	Event     string   `json:"event"`                // qr, notice, confirm_code, linked
	Modules   []string `json:"modules,omitempty"`    // QR rows, "1" = dark module, quiet zone included
	ExpiresIn int      `json:"expires_in,omitempty"` // seconds until the next code
	Code      string   `json:"code,omitempty"`       // passkey confirmation code to compare with the phone
	Message   string   `json:"message,omitempty"`
}

// qrModules encodes text as a QR code, one string per row.
func qrModules(text string) ([]string, error) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return nil, err
	}
	size := code.Size + 2*qrQuietZone
	rows := make([]string, size)
	for y := range size {
		var row strings.Builder
		for x := range size {
			cx, cy := x-qrQuietZone, y-qrQuietZone
			if cx >= 0 && cy >= 0 && cx < code.Size && cy < code.Size && code.Black(cx, cy) {
				row.WriteByte('1')
			} else {
				row.WriteByte('0')
			}
		}
		rows[y] = row.String()
	}
	return rows, nil
}

// renderQR draws two module rows per text line with half blocks, in explicit dark-on-light
// colours so the code scans whatever the terminal theme.
func renderQR(modules []string) []string {
	var lines []string
	for y := 0; y < len(modules); y += 2 {
		var line strings.Builder
		line.WriteString("\x1b[30;47m")
		for x := range len(modules[y]) {
			top := modules[y][x] == '1'
			bottom := y+1 < len(modules) && modules[y+1][x] == '1'
			switch {
			case top && bottom:
				line.WriteString("█")
			case top:
				line.WriteString("▀")
			case bottom:
				line.WriteString("▄")
			default:
				line.WriteString(" ")
			}
		}
		line.WriteString("\x1b[0m")
		lines = append(lines, line.String())
	}
	return lines
}

var (
	errLinkExpired = errors.New("the QR code expired before it was scanned; start linking again")
	errLinkPasskey = errors.New("this WhatsApp account requires a passkey to link new devices, which whatsapp-mcp cannot provide")
)

// runLink follows WhatsApp's QR pairing until the phone links this device.
// confirm and sendConfirm handle the passkey code check some accounts require;
// without them that check fails rather than being accepted automatically.
func runLink(ctx context.Context, codes <-chan whatsmeow.QRChannelItem, out func(LinkEvent),
	confirm func(code string) (bool, error), sendConfirm func(context.Context) error) error {
	for {
		var item whatsmeow.QRChannelItem
		var ok bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item, ok = <-codes:
		}
		if !ok {
			return errLinkExpired
		}
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			modules, err := qrModules(item.Code)
			if err != nil {
				return err
			}
			out(LinkEvent{Event: "qr", Modules: modules, ExpiresIn: int(item.Timeout.Seconds())})
		case whatsmeow.QRChannelSuccess.Event:
			out(LinkEvent{Event: "linked", Message: "This computer is now linked to WhatsApp."})
			return nil
		case whatsmeow.QRChannelScannedWithoutMultidevice.Event:
			out(LinkEvent{Event: "notice", Message: "The phone needs multi-device support enabled. Enable it, then scan the code again."})
		case whatsmeow.QRChannelEventPasskeyResponse:
			if confirm == nil || sendConfirm == nil || item.PasskeyConfirmation == nil {
				return errors.New("WhatsApp asked to confirm a code, which this setup cannot do; run whatsapp-mcp setup in a terminal")
			}
			out(LinkEvent{Event: "confirm_code", Code: item.PasskeyConfirmation.Code, Message: "Check that your phone shows the same code."})
			accepted, err := confirm(item.PasskeyConfirmation.Code)
			if err != nil {
				return err
			}
			if !accepted {
				return errCancelled
			}
			if err := sendConfirm(ctx); err != nil {
				return fmt.Errorf("confirming the code failed: %w", err)
			}
		case whatsmeow.QRChannelEventPasskeyRequest:
			return errLinkPasskey
		case whatsmeow.QRChannelTimeout.Event:
			return errLinkExpired
		case whatsmeow.QRChannelClientOutdated.Event:
			return errors.New("WhatsApp rejected this client version; update whatsapp-mcp")
		case whatsmeow.QRChannelEventError:
			return fmt.Errorf("linking failed: %v", item.Error)
		default:
			return fmt.Errorf("linking failed (%s)", item.Event)
		}
	}
}
