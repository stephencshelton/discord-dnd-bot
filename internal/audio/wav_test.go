package audio

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWriteWAVHeaderAndData(t *testing.T) {
	pcm := []int16{0, 1, -1, 32767, -32768, 100}
	var buf bytes.Buffer
	if err := WriteWAV(&buf, pcm, SampleRate, Channels); err != nil {
		t.Fatalf("WriteWAV: %v", err)
	}

	out := buf.Bytes()
	// 44-byte canonical PCM WAV header + data.
	dataBytes := len(pcm) * 2
	if got, want := len(out), 44+dataBytes; got != want {
		t.Fatalf("total length = %d, want %d", got, want)
	}

	if string(out[0:4]) != "RIFF" {
		t.Errorf("missing RIFF marker: %q", out[0:4])
	}
	if got := binary.LittleEndian.Uint32(out[4:8]); got != uint32(36+dataBytes) {
		t.Errorf("RIFF chunk size = %d, want %d", got, 36+dataBytes)
	}
	if string(out[8:16]) != "WAVEfmt " {
		t.Errorf("missing WAVEfmt marker: %q", out[8:16])
	}
	if got := binary.LittleEndian.Uint16(out[20:22]); got != 1 {
		t.Errorf("audio format = %d, want 1 (PCM)", got)
	}
	if got := binary.LittleEndian.Uint16(out[22:24]); got != uint16(Channels) {
		t.Errorf("channels = %d, want %d", got, Channels)
	}
	if got := binary.LittleEndian.Uint32(out[24:28]); got != uint32(SampleRate) {
		t.Errorf("sample rate = %d, want %d", got, SampleRate)
	}
	byteRate := SampleRate * Channels * 2
	if got := binary.LittleEndian.Uint32(out[28:32]); got != uint32(byteRate) {
		t.Errorf("byte rate = %d, want %d", got, byteRate)
	}
	if got := binary.LittleEndian.Uint16(out[32:34]); got != uint16(Channels*2) {
		t.Errorf("block align = %d, want %d", got, Channels*2)
	}
	if got := binary.LittleEndian.Uint16(out[34:36]); got != 16 {
		t.Errorf("bits per sample = %d, want 16", got)
	}
	if string(out[36:40]) != "data" {
		t.Errorf("missing data marker: %q", out[36:40])
	}
	if got := binary.LittleEndian.Uint32(out[40:44]); got != uint32(dataBytes) {
		t.Errorf("data chunk size = %d, want %d", got, dataBytes)
	}

	// Samples must round-trip little-endian.
	for i, want := range pcm {
		off := 44 + i*2
		got := int16(binary.LittleEndian.Uint16(out[off : off+2]))
		if got != want {
			t.Errorf("sample %d = %d, want %d", i, got, want)
		}
	}
}

func TestWriteWAVEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteWAV(&buf, nil, SampleRate, Channels); err != nil {
		t.Fatalf("WriteWAV(nil): %v", err)
	}
	if got := buf.Len(); got != 44 {
		t.Fatalf("empty WAV length = %d, want 44 (header only)", got)
	}
	if got := binary.LittleEndian.Uint32(buf.Bytes()[40:44]); got != 0 {
		t.Errorf("data chunk size = %d, want 0", got)
	}
}

func TestTrimSilence(t *testing.T) {
	const frame = 4 // samples per frame for the test

	loud := []int16{5000, -5000, 6000, -6000}
	quiet := []int16{0, 1, -1, 2}

	t.Run("drops silent frames keeps loud ones", func(t *testing.T) {
		in := append(append(append([]int16{}, quiet...), loud...), quiet...)
		got := TrimSilence(in, frame, 350)
		if len(got) != len(loud) {
			t.Fatalf("len = %d, want %d", len(got), len(loud))
		}
		for i := range loud {
			if got[i] != loud[i] {
				t.Errorf("sample %d = %d, want %d (loud audio must be untouched)", i, got[i], loud[i])
			}
		}
	})

	t.Run("threshold zero is a no-op", func(t *testing.T) {
		in := append(append([]int16{}, quiet...), loud...)
		got := TrimSilence(in, frame, 0)
		if len(got) != len(in) {
			t.Fatalf("len = %d, want %d (disabled trim must not modify)", len(got), len(in))
		}
	})

	t.Run("preserves trailing partial frame", func(t *testing.T) {
		in := append(append([]int16{}, loud...), 1, 2) // 4 + 2 partial
		got := TrimSilence(in, frame, 350)
		if len(got) != 6 {
			t.Fatalf("len = %d, want 6 (partial frame preserved)", len(got))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if got := TrimSilence(nil, frame, 350); len(got) != 0 {
			t.Fatalf("len = %d, want 0", len(got))
		}
	})
}

func TestSegmentPCM(t *testing.T) {
	seq := func(n int) []int16 {
		s := make([]int16, n)
		for i := range s {
			s[i] = int16(i)
		}
		return s
	}

	t.Run("shorter than one segment returns whole input", func(t *testing.T) {
		segs := SegmentPCM(seq(10), 100, 5)
		if len(segs) != 1 || len(segs[0]) != 10 {
			t.Fatalf("segments = %d (first len %d), want 1 whole segment", len(segs), len(segs[0]))
		}
	})

	t.Run("segmentSamples<=0 disables segmenting", func(t *testing.T) {
		segs := SegmentPCM(seq(1000), 0, 0)
		if len(segs) != 1 || len(segs[0]) != 1000 {
			t.Fatalf("segments = %d, want 1 whole segment when disabled", len(segs))
		}
	})

	t.Run("splits with overlap and covers all samples", func(t *testing.T) {
		segs := SegmentPCM(seq(25), 10, 3) // step 7: [0:10] [7:17] [14:24] [21:25]
		if len(segs) != 4 {
			t.Fatalf("segments = %d, want 4", len(segs))
		}
		if segs[0][0] != 0 || segs[0][len(segs[0])-1] != 9 {
			t.Errorf("seg0 = [%d..%d], want [0..9]", segs[0][0], segs[0][len(segs[0])-1])
		}
		if segs[1][0] != 7 {
			t.Errorf("seg1 start = %d, want 7 (overlap)", segs[1][0])
		}
		last := segs[len(segs)-1]
		if last[len(last)-1] != 24 {
			t.Errorf("last sample = %d, want 24", last[len(last)-1])
		}
	})

	t.Run("overlap >= segment is clamped and still terminates", func(t *testing.T) {
		segs := SegmentPCM(seq(30), 10, 100) // clamped to 9
		if len(segs) == 0 {
			t.Fatal("expected progress, got no segments")
		}
		last := segs[len(segs)-1]
		if last[len(last)-1] != 29 {
			t.Errorf("last sample = %d, want 29", last[len(last)-1])
		}
	})
}

// failWriter fails after allowing n successful writes, to exercise error paths.
type failWriter struct {
	remaining int
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.remaining <= 0 {
		return 0, errShortWrite
	}
	f.remaining--
	return len(p), nil
}

var errShortWrite = &writeErr{"short write"}

type writeErr struct{ msg string }

func (e *writeErr) Error() string { return e.msg }

func TestWriteWAVPropagatesWriterError(t *testing.T) {
	// Fail immediately on the very first write (RIFF marker).
	if err := WriteWAV(&failWriter{remaining: 0}, []int16{1, 2}, SampleRate, Channels); err == nil {
		t.Fatal("expected error from failing writer, got nil")
	}
}
