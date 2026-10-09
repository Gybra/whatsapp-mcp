package main

import (
	"image"
	"image/color"
	"testing"

	"github.com/makiuchi-d/gozxing"
	gozxingqr "github.com/makiuchi-d/gozxing/qrcode"
)

// TestQRDecodes checks with an independent decoder that the code shown to the phone holds the exact pairing string.
func TestQRDecodes(t *testing.T) {
	const pairing = "2@Xq8sFzQ0rT3mVn5LkP9wYc1bHg6eJd4,Uo7iA2sD5fG8hJ1kL4zX7cV0bN3mQ6wE9rT2yU5iO8p=,aS1dF4gH7jK0lZ3xC6vB9nM2qW5eR8tY1uI4oP7aS0=,zX2cV5bN8mQ1wE4rT7yU0iO3pA6sD9fG2hJ5kL8zX1="
	modules, err := qrModules(pairing)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 4
	size := len(modules) * scale
	img := image.NewGray(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			c := color.Gray{Y: 255}
			if modules[y/scale][x/scale] == '1' {
				c.Y = 0
			}
			img.SetGray(x, y, c)
		}
	}
	bitmap, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		t.Fatal(err)
	}
	result, err := gozxingqr.NewQRCodeReader().Decode(bitmap, nil)
	if err != nil {
		t.Fatalf("an independent decoder cannot read the QR code: %v", err)
	}
	if result.GetText() != pairing {
		t.Fatalf("decoded %q", result.GetText())
	}
}
