package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	opusMIME               = "audio/ogg; codecs=opus"
	opusGranuleRate        = 48000 // Opus granule positions always count 48 kHz samples
	maxVoiceSeconds        = 24 * 60 * 60
	voiceConversionTimeout = 60 * time.Second
)

var errNotOpus = fmt.Errorf("%w: not a valid Ogg Opus file", ErrUnsupportedMedia)

// AnalyzeOpus checks that data is a complete Ogg Opus stream and returns its
// duration in whole seconds, rounded up.
func AnalyzeOpus(data []byte) (uint32, error) {
	var preSkip, lastGranule uint64
	for offset, page := 0, 0; offset < len(data); page++ {
		if len(data)-offset < 27 || string(data[offset:offset+4]) != "OggS" || data[offset+4] != 0 {
			return 0, errNotOpus
		}
		granule := binary.LittleEndian.Uint64(data[offset+6:])
		bodyStart := offset + 27 + int(data[offset+26])
		if bodyStart > len(data) {
			return 0, errNotOpus
		}
		bodyEnd := bodyStart
		for _, segment := range data[offset+27 : bodyStart] {
			bodyEnd += int(segment)
		}
		if bodyEnd > len(data) {
			return 0, errNotOpus
		}
		body := data[bodyStart:bodyEnd]
		if page == 0 {
			if len(body) < 19 || string(body[:8]) != "OpusHead" {
				return 0, errNotOpus
			}
			preSkip = uint64(binary.LittleEndian.Uint16(body[10:12]))
		} else if granule != math.MaxUint64 { // MaxUint64: no packet ends on this page
			lastGranule = granule
		}
		offset = bodyEnd
	}
	if lastGranule <= preSkip {
		return 0, errNotOpus
	}
	seconds := (lastGranule - preSkip + opusGranuleRate - 1) / opusGranuleRate
	if seconds > maxVoiceSeconds {
		return 0, errNotOpus
	}
	return uint32(seconds), nil
}

// Replaced in tests; the real converter never sees a shell.
var (
	lookFFmpeg = exec.LookPath
	runFFmpeg  = func(ctx context.Context, executable string, args []string) error {
		return exec.CommandContext(ctx, executable, args...).Run() // output may name files: never logged
	}
)

// ConvertVoice converts source, whose container format was already detected, to
// Ogg Opus inside dir. The explicit input format and the file-only protocol list
// keep ffmpeg from following playlists or URLs to other files.
func ConvertVoice(ctx context.Context, source, format, dir string) (string, error) {
	executable, err := lookFFmpeg("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("%w: install ffmpeg to send %s audio as a voice message, or send an Ogg Opus file", ErrUnsupportedMedia, format)
	}
	out := filepath.Join(dir, "voice.ogg")
	ctx, cancel := context.WithTimeout(ctx, voiceConversionTimeout)
	defer cancel()
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file",
		"-f", format, "-i", source,
		"-vn", "-map_metadata", "-1", "-ac", "1", "-c:a", "libopus", "-b:a", "32k", "-application", "voip",
		"-f", "ogg", "-n", out,
	}
	if err := runFFmpeg(ctx, executable, args); err != nil {
		return "", fmt.Errorf("%w: audio conversion failed", ErrUnsupportedMedia)
	}
	info, err := os.Lstat(out)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxMediaBytes {
		return "", fmt.Errorf("%w: audio conversion failed", ErrUnsupportedMedia)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return "", err
	}
	if _, err := AnalyzeOpus(data); err != nil {
		return "", err
	}
	return out, nil
}
