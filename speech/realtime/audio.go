package realtime

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

const maxPCMBytes = sampleRate * 2 * 60 * 10 // Ten minutes of mono PCM16.

func decodeAudio(ctx context.Context, audio []byte) ([]byte, error) {
	if len(audio) == 0 {
		return nil, fmt.Errorf("speech realtime: empty recording")
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error",
		"-protocol_whitelist", "pipe", "-i", "pipe:0", "-map", "0:a:0", "-vn",
		"-t", "601", "-ac", "1", "-ar", "24000", "-f", "s16le", "pipe:1")
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(audio)
	output := &audioBuffer{limit: maxPCMBytes}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("speech realtime: decode audio: %w", ctx.Err())
		}
		if output.overflow {
			return nil, fmt.Errorf("speech realtime: recording exceeds ten minutes")
		}
		return nil, fmt.Errorf("speech realtime: decode audio with ffmpeg: %w", err)
	}
	return output.Bytes(), nil
}

type audioBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *audioBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.overflow = true
		return 0, fmt.Errorf("decoded audio is too large")
	}
	return b.buffer.Write(p)
}

func (b *audioBuffer) Bytes() []byte { return b.buffer.Bytes() }
