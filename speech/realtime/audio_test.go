package realtime

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os/exec"
	"testing"
)

func TestAudioBuffer_BoundsDecodedAudioThroughIOCopy(t *testing.T) {
	output := &audioBuffer{limit: 10}
	if _, err := io.Copy(output, bytes.NewReader(make([]byte, 12))); err == nil || !output.overflow {
		t.Fatal("oversized decoded audio was not rejected")
	}
	if len(output.Bytes()) > 10 {
		t.Fatal("buffer exceeded its limit")
	}
}

func TestDecodeAudio_NormalizesWAVAndRejectsInvalidInput(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	binary.Write(&wav, binary.LittleEndian, uint32(36+16000))
	wav.WriteString("WAVEfmt ")
	for _, value := range []any{uint32(16), uint16(1), uint16(1), uint32(8000), uint32(16000), uint16(2), uint16(16)} {
		binary.Write(&wav, binary.LittleEndian, value)
	}
	wav.WriteString("data")
	binary.Write(&wav, binary.LittleEndian, uint32(16000))
	wav.Write(make([]byte, 16000))
	pcm, err := decodeAudio(context.Background(), wav.Bytes())
	if err != nil || len(pcm) != 48000 {
		t.Fatalf("one-second 8kHz WAV decoded to %d bytes, %v; want 48,000 PCM bytes", len(pcm), err)
	}
	if _, err := decodeAudio(context.Background(), []byte("not audio")); err == nil {
		t.Fatal("invalid recording was accepted")
	}
}
