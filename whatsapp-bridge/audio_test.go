package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func oggPage(headerType byte, granule uint64, seq uint32, body []byte) []byte {
	var segments []byte
	n := len(body)
	for n >= 255 {
		segments = append(segments, 255)
		n -= 255
	}
	segments = append(segments, byte(n))
	header := make([]byte, 27)
	copy(header, "OggS")
	header[5] = headerType
	binary.LittleEndian.PutUint64(header[6:], granule)
	binary.LittleEndian.PutUint32(header[14:], 1)
	binary.LittleEndian.PutUint32(header[18:], seq)
	header[26] = byte(len(segments))
	return slices.Concat(header, segments, body)
}

// opusSample builds a structurally valid Ogg Opus stream lasting the given number of 48 kHz samples.
func opusSample(samples uint64, preSkip uint16) []byte {
	head := []byte("OpusHead")
	head = append(head, 1, 1)
	head = binary.LittleEndian.AppendUint16(head, preSkip)
	head = binary.LittleEndian.AppendUint32(head, 16000) // declared input rate: not the granule rate
	head = append(head, 0, 0, 0)
	tags := append([]byte("OpusTags"), 0, 0, 0, 0, 0, 0, 0, 0)
	return slices.Concat(oggPage(2, 0, 0, head), oggPage(0, 0, 1, tags), oggPage(4, uint64(preSkip)+samples, 2, []byte{0xfc, 0xff, 0xfe}))
}

func TestOpusDuration(t *testing.T) {
	for _, c := range []struct {
		samples uint64
		want    uint32
	}{{48000 * 3, 3}, {48000*3 + 1, 4}, {100, 1}} {
		got, err := AnalyzeOpus(opusSample(c.samples, 312))
		if err != nil || got != c.want {
			t.Errorf("%d samples: got %d, %v; want %d", c.samples, got, err, c.want)
		}
	}
}

func TestOpusTruncated(t *testing.T) {
	valid := opusSample(48000, 312)
	cases := map[string][]byte{
		"empty":            nil,
		"no magic":         append([]byte("RIFF"), valid[4:]...),
		"cut header":       valid[:20],
		"cut segments":     valid[:27],
		"cut body":         valid[:len(valid)-1],
		"trailing garbage": append(slices.Clone(valid), 'x'),
		"not opus":         slices.Concat(oggPage(2, 0, 0, []byte("Vorbis head bytes...")), oggPage(4, 48000, 1, []byte{1})),
		"headers only":     opusSample(0, 312)[:len(opusSample(0, 312))-(27+1+3)],
		"granule before pre-skip": slices.Concat(
			opusSample(0, 312)[:len(opusSample(0, 312))-(27+1+3)],
			oggPage(4, 100, 2, []byte{1, 2, 3})),
	}
	for name, data := range cases {
		if _, err := AnalyzeOpus(data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func FuzzAnalyzeOpus(f *testing.F) {
	f.Add(opusSample(48000, 312))
	f.Add(oggPage(2, 0, 0, []byte("OpusHead")))
	f.Add([]byte("OggS\x00\x02\xff\xff\xff\xff\xff\xff\xff\xff\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xff"))
	f.Fuzz(func(t *testing.T, data []byte) {
		AnalyzeOpus(data) // must never panic or loop
	})
}

func withFakeFFmpeg(t *testing.T, run func(ctx context.Context, executable string, args []string) error) {
	t.Helper()
	oldLook, oldRun := lookFFmpeg, runFFmpeg
	lookFFmpeg = func(string) (string, error) { return "/usr/local/bin/ffmpeg", nil }
	runFFmpeg = run
	t.Cleanup(func() { lookFFmpeg, runFFmpeg = oldLook, oldRun })
}

func TestVoiceConversion(t *testing.T) {
	dir := privateDataDir(t)
	source := filepath.Join(dir, "input.mp3")
	os.WriteFile(source, []byte("ID3fake mp3"), 0o600)

	var gotArgs []string
	var deadline bool
	withFakeFFmpeg(t, func(ctx context.Context, executable string, args []string) error {
		gotArgs = args
		_, deadline = ctx.Deadline()
		return os.WriteFile(args[len(args)-1], opusSample(48000, 312), 0o600)
	})
	out, err := ConvertVoice(context.Background(), source, "mp3", dir)
	if err != nil {
		t.Fatalf("ConvertVoice: %v", err)
	}
	if _, err := AnalyzeOpus(mustRead(t, out)); err != nil || filepath.Dir(out) != dir {
		t.Fatalf("output %s not a valid Opus file in the private dir: %v", out, err)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"-protocol_whitelist file", "-f mp3 -i " + source, "-c:a libopus", "-map_metadata -1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ffmpeg args %q lack %q", joined, want)
		}
	}
	if !deadline {
		t.Error("conversion has no time limit")
	}

	withFakeFFmpeg(t, func(ctx context.Context, executable string, args []string) error {
		return os.WriteFile(args[len(args)-1], []byte("not opus"), 0o600)
	})
	if _, err := ConvertVoice(context.Background(), source, "mp3", dir); err == nil {
		t.Error("invalid converter output accepted")
	}
	lookFFmpeg = func(string) (string, error) { return "", exec.ErrNotFound }
	if _, err := ConvertVoice(context.Background(), source, "mp3", dir); !errors.Is(err, ErrUnsupportedMedia) || !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("missing ffmpeg: %v", err)
	}
}

func TestVoiceConversionWithFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := privateDataDir(t)
	source := filepath.Join(dir, "tone.wav")
	os.WriteFile(source, silentWAV(8000, 4000), 0o600) // half a second
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := ConvertVoice(ctx, source, "wav", dir)
	if err != nil {
		t.Fatalf("ConvertVoice: %v", err)
	}
	if seconds, err := AnalyzeOpus(mustRead(t, out)); err != nil || seconds != 1 {
		t.Fatalf("converted voice: %d s, %v", seconds, err)
	}
}

func silentWAV(rate, samples uint32) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+samples*2))
	b.WriteString("WAVEfmt ")
	for _, field := range []any{uint32(16), uint16(1), uint16(1), rate, rate * 2, uint16(2), uint16(16)} {
		binary.Write(&b, binary.LittleEndian, field)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, samples*2)
	b.Write(make([]byte, samples*2))
	return b.Bytes()
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
