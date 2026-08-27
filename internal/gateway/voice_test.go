package gateway

import (
	"testing"

	"github.com/stephencshelton/discord-dnd-bot/internal/audio"
	"github.com/stephencshelton/discord-dnd-bot/internal/config"
)

// newAnchoredRecording builds a recording with a single pre-anchored stream so
// the pure timestamp→frame math can be exercised without a live voice conn.
func newAnchoredRecording(ssrc uint32, baseTS uint32, baseFrame int) *recording {
	r := &recording{anchor: map[uint32]streamAnchor{}}
	r.anchor[ssrc] = streamAnchor{baseTS: baseTS, baseFrame: baseFrame}
	return r
}

func TestFrameIndexAdvancesByTimestamp(t *testing.T) {
	const ssrc = uint32(42)
	r := newAnchoredRecording(ssrc, 1000, 0)

	// Consecutive 20ms frames advance the RTP timestamp by FrameSize each and
	// must map to consecutive timeline slots.
	for i := 0; i < 100; i++ {
		ts := uint32(1000 + i*audio.FrameSize)
		if got := r.frameIndexFor(ssrc, ts); got != i {
			t.Fatalf("frameIndexFor(ts=%d) = %d, want %d", ts, got, i)
		}
	}
}

func TestFrameIndexPreservesSilenceGaps(t *testing.T) {
	const ssrc = uint32(7)
	r := newAnchoredRecording(ssrc, 0, 0)

	// A speaker pauses for 5 seconds (250 frames) then resumes. Discord omits
	// packets during silence, so the next packet's RTP timestamp jumps by
	// 250*FrameSize. The old running-counter approach placed it at the next
	// slot (drift); timestamp alignment must place it 250 frames later.
	if got := r.frameIndexFor(ssrc, 0); got != 0 {
		t.Fatalf("first frame = %d, want 0", got)
	}
	gap := uint32(250 * audio.FrameSize)
	if got := r.frameIndexFor(ssrc, gap); got != 250 {
		t.Fatalf("post-gap frame = %d, want 250 (no drift)", got)
	}
}

func TestFrameIndexStreamsStayAligned(t *testing.T) {
	// Two speakers with different RTP bases but anchored to the same wall-clock
	// frame must land on the same timeline slot for simultaneous speech.
	const a, b = uint32(1), uint32(2)
	r := &recording{anchor: map[uint32]streamAnchor{
		a: {baseTS: 500000, baseFrame: 30},
		b: {baseTS: 9, baseFrame: 30},
	}}
	// 10 frames after each stream's base — both should be at frame 40.
	if got := r.frameIndexFor(a, 500000+10*audio.FrameSize); got != 40 {
		t.Fatalf("stream a = %d, want 40", got)
	}
	if got := r.frameIndexFor(b, 9+10*audio.FrameSize); got != 40 {
		t.Fatalf("stream b = %d, want 40", got)
	}
}

func TestFrameIndexHandlesTimestampWraparound(t *testing.T) {
	const ssrc = uint32(3)
	// Anchor near the top of the uint32 range so the next frames wrap past 0.
	base := uint32(0xFFFFFFFF - audio.FrameSize + 1) // one frame before wrap
	r := newAnchoredRecording(ssrc, base, 1000)

	if got := r.frameIndexFor(ssrc, base); got != 1000 {
		t.Fatalf("base frame = %d, want 1000", got)
	}
	// Advancing one frame wraps the uint32 timestamp to a small value; unsigned
	// subtraction must still yield a delta of exactly one frame.
	if got := r.frameIndexFor(ssrc, base+audio.FrameSize); got != 1001 {
		t.Fatalf("post-wrap frame = %d, want 1001", got)
	}
}

// newTrackRecording builds a recording ready to accept mixFrame calls, with an
// empty per-user track map and a Gateway carrying a config (mixFrame reads the
// session-length cap from it).
func newTrackRecording() *recording {
	return &recording{
		tracks: map[string]*userTrack{},
		g:      &Gateway{cfg: &config.Config{}},
	}
}

func TestMixFrameSeparatesUsers(t *testing.T) {
	r := newTrackRecording()
	full := make([]int16, audio.FrameSize*audio.Channels)
	for i := range full {
		full[i] = 100
	}

	// Two users speak at overlapping absolute timeline slots. Each must land in
	// its own track and NOT be summed together (that was the old bug — one mixed
	// buffer meant Whisper couldn't tell speakers apart).
	r.mixFrame("userA", 0, full)
	r.mixFrame("userB", 0, full)
	r.mixFrame("userA", 1, full)

	if len(r.tracks) != 2 {
		t.Fatalf("tracks = %d, want 2 (one per user)", len(r.tracks))
	}
	ta, tb := r.tracks["userA"], r.tracks["userB"]
	if ta == nil || tb == nil {
		t.Fatal("expected both userA and userB tracks")
	}
	if len(ta.frames) != 2 {
		t.Fatalf("userA frames = %d, want 2", len(ta.frames))
	}
	if len(tb.frames) != 1 {
		t.Fatalf("userB frames = %d, want 1", len(tb.frames))
	}
	// Each user's slot 0 holds its own single contribution (100), not the sum of
	// both users (which would be 200 in the old mixed buffer).
	if ta.frames[0][0] != 100 {
		t.Errorf("userA slot0 = %d, want 100 (not summed with userB)", ta.frames[0][0])
	}
	if tb.frames[0][0] != 100 {
		t.Errorf("userB slot0 = %d, want 100", tb.frames[0][0])
	}
}

func TestMixFrameStartSeqOffsetsChunkNumbering(t *testing.T) {
	r := newTrackRecording()
	r.startSeq = 5 // resumed session already has chunks 1..5 in storage
	frame := make([]int16, audio.FrameSize*audio.Channels)

	r.mixFrame("userA", 0, frame)
	if got := r.tracks["userA"].chunkSeq; got != 6 {
		t.Fatalf("resumed track chunkSeq = %d, want 6 (startSeq+1)", got)
	}
}
