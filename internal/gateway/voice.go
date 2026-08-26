package gateway

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
	"layeh.com/gopus"

	"github.com/stephencshelton/discord-dnd-bot/internal/audio"
)

// voiceManager owns active voice recordings, one per guild. Opus packets are
// tagged by SSRC (per speaker); each stream is decoded and time-aligned into
// one mixed PCM buffer so transcription gets a single file.
type voiceManager struct {
	g   *Gateway
	mu  sync.Mutex
	rec map[string]*recording // keyed by guildID
}

func newVoiceManager(g *Gateway) *voiceManager {
	return &voiceManager{g: g, rec: make(map[string]*recording)}
}

type recording struct {
	vc        *discordgo.VoiceConnection
	sessionID string
	guildID   string
	started   time.Time
	done      chan struct{}

	// g lets capture goroutines persist participants as speakers are identified;
	// read-only from the recording's perspective.
	g *Gateway

	mu       sync.Mutex
	decoders map[uint32]*gopus.Decoder // per-SSRC Opus decoder
	// ssrcUser maps an SSRC to the Discord user speaking on it, learned from
	// VoiceSpeakingUpdate events, so participants are known factually rather
	// than guessed from the transcript.
	ssrcUser map[uint32]string
	seen     map[string]bool // userIDs already persisted this session
	// frames holds mixed stereo PCM, one 20ms slot per shared timeline index.
	// Packets are placed by RTP timestamp (see anchor) so concurrent speakers
	// stay aligned and silence gaps leave empty slots instead of collapsing,
	// which previously caused drift on long sessions.
	frames [][]int16
	// anchor maps each SSRC to its first packet's alignment: baseTS is that
	// packet's RTP timestamp, baseFrame the timeline index (from wall-clock
	// arrival). Later slots = baseFrame + (pkt.Timestamp-baseTS)/FrameSize.
	anchor map[uint32]streamAnchor
}

// streamAnchor ties one speaker's RTP clock to the shared session timeline.
type streamAnchor struct {
	baseTS    uint32 // RTP timestamp of the stream's first observed packet
	baseFrame int    // timeline frame index that first packet was placed at
}

// start joins the voice channel and begins capturing.
func (m *voiceManager) start(guildID, channelID, sessionID string) error {
	m.mu.Lock()
	if _, ok := m.rec[guildID]; ok {
		m.mu.Unlock()
		return fmt.Errorf("already recording in guild %s", guildID)
	}
	m.mu.Unlock()

	vc, err := m.g.sess.ChannelVoiceJoin(guildID, channelID, true /*mute*/, false /*deaf: we must hear*/)
	if err != nil {
		return err
	}
	r := &recording{
		vc:        vc,
		sessionID: sessionID,
		guildID:   guildID,
		started:   time.Now(),
		done:      make(chan struct{}),
		g:         m.g,
		decoders:  make(map[uint32]*gopus.Decoder),
		ssrcUser:  make(map[uint32]string),
		seen:      make(map[string]bool),
		anchor:    make(map[uint32]streamAnchor),
	}
	m.mu.Lock()
	m.rec[guildID] = r
	m.mu.Unlock()

	// Learn SSRC -> user mappings via discordgo's voice-speaking events.
	vc.AddHandler(r.onSpeakingUpdate)

	go r.capture()
	return nil
}

// onSpeakingUpdate records the SSRC->user mapping Discord provides when a
// member starts speaking, persisting the participant immediately (best effort)
// so an abruptly-ended session still knows who was present.
func (r *recording) onSpeakingUpdate(_ *discordgo.VoiceConnection, vs *discordgo.VoiceSpeakingUpdate) {
	if vs == nil || vs.UserID == "" {
		return
	}
	ssrc := uint32(vs.SSRC)
	r.mu.Lock()
	r.ssrcUser[ssrc] = vs.UserID
	alreadySeen := r.seen[vs.UserID]
	r.seen[vs.UserID] = true
	sessionID := r.sessionID
	guildID := r.guildID
	r.mu.Unlock()

	if alreadySeen {
		return
	}
	// Resolve a display name and persist without blocking the voice event loop.
	go r.persistParticipant(sessionID, guildID, vs.UserID)
}

// persistParticipant resolves a display name (guild nick > global name > ID) and
// upserts the participant row. Errors are logged but never fatal.
func (r *recording) persistParticipant(sessionID, guildID, userID string) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return
	}
	name := userID
	if m, err := r.g.sess.State.Member(guildID, userID); err == nil && m != nil {
		switch {
		case m.Nick != "":
			name = m.Nick
		case m.User != nil && m.User.GlobalName != "":
			name = m.User.GlobalName
		case m.User != nil && m.User.Username != "":
			name = m.User.Username
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.g.store.RecordParticipant(ctx, sid, userID, name); err != nil {
		r.g.log.Warn("record participant", "err", err, "session", sessionID, "user", userID)
	}
}

// capture consumes Opus packets until the connection's receive channel closes.
func (r *recording) capture() {
	defer close(r.done)
	// Cap retained frames to bound memory (20ms/frame -> minutes*60*50). 0 disables.
	maxFrames := 0
	if m := r.g.cfg.Audio.MaxSessionMinutes; m > 0 {
		maxFrames = m * 60 * 50
	}
	capped := false
	for pkt := range r.vc.OpusRecv {
		r.mu.Lock()
		if maxFrames > 0 && len(r.frames) >= maxFrames {
			r.mu.Unlock()
			if !capped {
				capped = true
				r.g.log.Warn("recording hit max length; further audio dropped",
					"guild", r.guildID, "session", r.sessionID, "maxMinutes", r.g.cfg.Audio.MaxSessionMinutes)
			}
			continue
		}
		dec, ok := r.decoders[pkt.SSRC]
		if !ok {
			d, err := gopus.NewDecoder(audio.SampleRate, audio.Channels)
			if err != nil {
				r.mu.Unlock()
				continue
			}
			dec = d
			r.decoders[pkt.SSRC] = dec
			// Anchor this speaker's RTP clock to the timeline via wall-clock
			// arrival, so later frames are placed by RTP-delta and pauses leave
			// empty slots instead of collapsing and drifting.
			r.anchor[pkt.SSRC] = streamAnchor{
				baseTS:    pkt.Timestamp,
				baseFrame: r.wallClockFrame(),
			}
		}
		pcm, err := dec.Decode(pkt.Opus, audio.FrameSize, false)
		if err != nil {
			r.mu.Unlock()
			continue
		}
		r.mixFrame(r.frameIndexFor(pkt.SSRC, pkt.Timestamp), pcm)
		r.mu.Unlock()
	}
}

// wallClockFrame returns the timeline frame index for now (20ms frames elapsed
// since start), used to anchor a new speaker's stream. Caller holds r.mu.
func (r *recording) wallClockFrame() int {
	const frame = 20 * time.Millisecond
	idx := int(time.Since(r.started) / frame)
	if idx < 0 {
		idx = 0
	}
	return idx
}

// frameIndexFor maps a packet's RTP timestamp to a timeline frame index
// relative to the stream's anchor. Unsigned subtraction handles 32-bit RTP
// timestamp wraparound. Caller holds r.mu.
func (r *recording) frameIndexFor(ssrc uint32, ts uint32) int {
	a := r.anchor[ssrc]
	deltaSamples := ts - a.baseTS // wraps correctly as uint32
	idx := a.baseFrame + int(deltaSamples/audio.FrameSize)
	if idx < 0 {
		idx = 0
	}
	return idx
}

// mixFrame sums a decoded stereo frame into the timeline at idx, extending the
// buffer as needed. A corrupt/huge RTP-derived idx is clamped (dropped) rather
// than allowed to force a giant allocation. Caller holds r.mu.
func (r *recording) mixFrame(idx int, pcm []int16) {
	if idx < 0 {
		return
	}
	// A legitimate gap is silence; more than ~1h ahead of the tail indicates a
	// bogus timestamp — drop it rather than allocate.
	const maxGapFrames = 180000
	if idx > len(r.frames)+maxGapFrames {
		return
	}
	// Respect the overall session cap.
	if m := r.g.cfg.Audio.MaxSessionMinutes; m > 0 && idx >= m*60*50 {
		return
	}
	for len(r.frames) <= idx {
		r.frames = append(r.frames, make([]int16, audio.FrameSize*audio.Channels))
	}
	dst := r.frames[idx]
	n := len(pcm)
	if n > len(dst) {
		n = len(dst)
	}
	for i := 0; i < n; i++ {
		dst[i] = clip16(int32(dst[i]) + int32(pcm[i]))
	}
}

func clip16(v int32) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// stop leaves the channel, encodes the mixed PCM to WAV, uploads it to object
// storage, and returns the object key plus recorded duration.
func (m *voiceManager) stop(ctx context.Context, guildID string) (string, time.Duration, error) {
	m.mu.Lock()
	r, ok := m.rec[guildID]
	if ok {
		delete(m.rec, guildID)
	}
	m.mu.Unlock()
	if !ok {
		return "", 0, fmt.Errorf("no recording for guild %s", guildID)
	}

	duration := time.Since(r.started)
	// Disconnect; this closes OpusRecv and lets capture() drain + exit.
	_ = r.vc.Disconnect()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
	}

	// Flatten frames into one interleaved PCM slice, in timeline order.
	r.mu.Lock()
	pcm := make([]int16, 0, len(r.frames)*audio.FrameSize*audio.Channels)
	for _, f := range r.frames {
		pcm = append(pcm, f...)
	}
	r.mu.Unlock()

	// Optionally drop near-silent frames to cut billed transcription minutes.
	if m.g.cfg.Audio.SilenceTrim {
		pcm = audio.TrimSilence(pcm, audio.FrameSize*audio.Channels, m.g.cfg.Audio.SilenceRMSThreshold)
	}

	var buf bytes.Buffer
	if err := audio.WriteWAV(&buf, pcm, audio.SampleRate, audio.Channels); err != nil {
		return "", duration, err
	}
	key := fmt.Sprintf("sessions/%s/%s.wav", guildID, r.sessionID)
	if _, err := m.g.storage.Put(ctx, key, "audio/wav", &buf); err != nil {
		return "", duration, err
	}
	return key, duration, nil
}

// stopAll disconnects every active recording on shutdown, waits briefly for
// capture to exit, and marks in-flight sessions failed so the DB row isn't
// stuck 'recording' (which blocks the one-active-session unique index).
// Captured audio is not uploaded — a dropped recording is preferable to a
// stuck session.
func (m *voiceManager) stopAll() {
	m.mu.Lock()
	recs := make([]*recording, 0, len(m.rec))
	for _, r := range m.rec {
		recs = append(recs, r)
	}
	m.rec = make(map[string]*recording)
	m.mu.Unlock()

	for _, r := range recs {
		_ = r.vc.Disconnect()
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
		}
		if sid, err := uuid.Parse(r.sessionID); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := m.g.store.SetSessionResult(ctx, sid, "", "", "failed"); err != nil {
				m.g.log.Warn("mark session failed on shutdown", "session", r.sessionID, "err", err)
			}
			cancel()
		}
	}
}
