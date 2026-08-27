package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/google/uuid"
	"layeh.com/gopus"

	"github.com/stephencshelton/discord-dnd-bot/internal/audio"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
)

// voiceManager owns active voice recordings, one per guild. Opus packets are
// tagged by SSRC (per speaker); each stream is decoded and time-aligned into a
// PER-USER PCM track so transcription can attribute speech to the right player.
//
// With disgo the SSRC->user resolution is handled inside the voice connection,
// so each received frame already carries the speaking user's ID.
type voiceManager struct {
	g   *Gateway
	mu  sync.Mutex
	rec map[string]*recording // keyed by guildID
}

func newVoiceManager(g *Gateway) *voiceManager {
	return &voiceManager{g: g, rec: make(map[string]*recording)}
}

type recording struct {
	conn      voice.Conn
	sessionID string
	guildID   string
	channelID string
	started   time.Time

	// g lets capture goroutines persist participants as speakers are identified;
	// read-only from the recording's perspective.
	g *Gateway

	// chunkPrefix is the object-storage prefix for this session's PCM checkpoints.
	// Per-user chunks live under chunkPrefix/<userID>/chunk-NNNNNN.pcm so each
	// speaker's audio can be transcribed separately and merged with speaker labels.
	chunkPrefix string
	// startSeq is the chunk number already present in storage per user at resume
	// (0 for a fresh session); a resumed track's numbering continues after it.
	startSeq int
	// stop signals the checkpoint loop to exit; done is closed when it has.
	stopCh chan struct{}
	doneCh chan struct{}

	mu       sync.Mutex
	decoders map[uint32]*gopus.Decoder // per-SSRC Opus decoder
	ssrcUser map[uint32]string         // SSRC -> speaking userID (for routing frames)
	seen     map[string]bool           // userIDs already persisted this session
	capped   bool
	// tracks holds one sliding-window PCM buffer per speaking user, keyed by
	// userID string. Each track only allocates frames for slots that user
	// actually spoke, so total RAM scales with concurrent speech, not with the
	// number of participants or session length. After each checkpoint the flushed
	// leading frames of every track are dropped and its frameBase advances, so
	// memory stays bounded to ~one checkpoint interval (preserves the earlier
	// OOM fix, now per track).
	tracks map[string]*userTrack
	// totalFramesAll is the absolute count of frames ever produced across ALL
	// tracks; used for the whole-session MaxSessionMinutes cap.
	totalFramesAll int
	// anchor maps each SSRC to its first packet's alignment: baseTS is that
	// packet's RTP timestamp, baseFrame the ABSOLUTE timeline index (from
	// wall-clock arrival). Later slots = baseFrame + (pkt.Timestamp-baseTS)/FrameSize.
	// Kept per-SSRC (an SSRC belongs to one user) so each speaker's RTP clock is
	// aligned to the shared session timeline independently.
	anchor map[uint32]streamAnchor
}

// userTrack is one speaker's slice of the session timeline. frames[i] is the
// mixed stereo PCM for absolute timeline index frameBase+i (only this user's
// audio; a user's own rare overlapping packets are summed). chunkSeq is the next
// per-user chunk number.
type userTrack struct {
	frames      [][]int16
	frameBase   int
	totalFrames int
	chunkSeq    int
}

// streamAnchor ties one speaker's RTP clock to the shared session timeline.
type streamAnchor struct {
	baseTS    uint32 // RTP timestamp of the stream's first observed packet
	baseFrame int    // timeline frame index that first packet was placed at
}

// checkpointInterval is how often the live recorder flushes newly-mixed PCM to
// object storage, so a pod crash loses at most this much unsaved tail (plus the
// downtime window itself). Chosen to balance data-loss window vs. S3 PUT churn.
const checkpointInterval = 30 * time.Second

// start joins the voice channel and begins capturing a NEW session. chunkSeq
// begins at 1. sessionID's chunk prefix is recorded so a crash can be recovered.
func (m *voiceManager) start(guildID, channelID, sessionID string) error {
	return m.join(guildID, channelID, sessionID, 0)
}

// join is the shared connect+record path for both a fresh start and a resume.
// startSeq is the chunk number already present in storage (0 for a new session);
// new checkpoints continue after it so a resumed session's audio is contiguous.
func (m *voiceManager) join(guildID, channelID, sessionID string, startSeq int) error {
	m.mu.Lock()
	if _, ok := m.rec[guildID]; ok {
		m.mu.Unlock()
		return fmt.Errorf("already recording in guild %s", guildID)
	}
	m.mu.Unlock()

	gid, err := snowflake.Parse(guildID)
	if err != nil {
		return fmt.Errorf("invalid guild id %q: %w", guildID, err)
	}
	cid, err := snowflake.Parse(channelID)
	if err != nil {
		return fmt.Errorf("invalid channel id %q: %w", channelID, err)
	}

	r := &recording{
		sessionID:   sessionID,
		guildID:     guildID,
		channelID:   channelID,
		started:     time.Now(),
		g:           m.g,
		chunkPrefix: chunkPrefixFor(guildID, sessionID),
		startSeq:    startSeq,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
		decoders:    make(map[uint32]*gopus.Decoder),
		ssrcUser:    make(map[uint32]string),
		seen:        make(map[string]bool),
		tracks:      make(map[string]*userTrack),
		anchor:      make(map[uint32]streamAnchor),
	}

	// Connect to the voice gateway, retrying on timeout. Discord sometimes
	// withholds VOICE_SERVER_UPDATE/VOICE_STATE_UPDATE in response to the first
	// join op-4 (leaving conn.Open waiting on both events, disgo #580) — often
	// after a reconnect or a prior half-open attempt. Each failed attempt's
	// conn.Close sends a leave op-4, which reliably prods Discord to deliver the
	// events, so a subsequent attempt with a fresh conn succeeds. We recreate the
	// conn per attempt (RemoveConn+CreateConn) so no stale state carries over.
	const (
		maxAttempts    = 3
		perAttemptOpen = 12 * time.Second
	)
	var conn voice.Conn
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		m.g.client.VoiceManager.RemoveConn(gid)
		conn = m.g.client.VoiceManager.CreateConn(gid)
		r.conn = conn

		ctx, cancel := context.WithTimeout(context.Background(), perAttemptOpen)
		openStart := time.Now()
		err := conn.Open(ctx, cid, true /*selfMute*/, false /*selfDeaf*/)
		cancel()
		if err == nil {
			m.g.log.Info("voice conn opened",
				"guild", guildID, "channel", channelID, "session", sessionID,
				"attempt", attempt, "elapsed_ms", time.Since(openStart).Milliseconds())
			lastErr = nil
			break
		}
		lastErr = err

		var gwStatus voice.Status = -1
		var ssrc uint32
		if gw := conn.Gateway(); gw != nil {
			gwStatus = gw.Status()
			ssrc = gw.SSRC()
		}
		m.g.log.Warn("voice conn open attempt failed",
			"guild", guildID, "channel", channelID, "session", sessionID,
			"attempt", attempt, "max_attempts", maxAttempts,
			"elapsed_ms", time.Since(openStart).Milliseconds(),
			"voice_gateway_status", int(gwStatus), "ssrc", ssrc, "err", err)

		// Tear down the half-open conn. Close sends a leave op-4, which nudges
		// Discord to send the voice events for the next attempt.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn.Close(closeCtx)
		closeCancel()
		m.g.client.VoiceManager.RemoveConn(gid)
		if attempt < maxAttempts {
			time.Sleep(1500 * time.Millisecond)
		}
	}
	if lastErr != nil {
		m.g.log.Error("voice conn open failed after retries",
			"guild", guildID, "channel", channelID, "session", sessionID,
			"attempts", maxAttempts, "err", lastErr)
		return lastErr
	}

	// Attach our receiver AFTER Open: SetOpusFrameReceiver immediately starts a
	// goroutine that calls conn.UDP().ReadPacket(); the UDP socket is only dialed
	// during Open, so attaching before Open races and dereferences a nil conn
	// (SIGSEGV in disgo's udpConnImpl.ReadPacket). By now the socket is ready.
	conn.SetOpusFrameReceiver(r)

	// Record where checkpoints live so a crashed session can be reassembled.
	if err := m.g.store.SetSessionChunkPrefix(context.Background(), mustSessionUUID(sessionID), r.chunkPrefix); err != nil {
		m.g.log.Warn("record chunk prefix", "session", sessionID, "err", err)
	}

	// Periodically flush accumulated PCM to storage for crash recovery.
	go r.checkpointLoop()

	m.mu.Lock()
	m.rec[guildID] = r
	m.mu.Unlock()
	m.g.log.Info("voice recording started",
		"guild", guildID, "channel", channelID, "session", sessionID,
		"start_seq", startSeq)
	return nil
}

// has reports whether an in-memory recording exists for the guild.
func (m *voiceManager) has(guildID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.rec[guildID]
	return ok
}

// chunkPrefixFor returns the object-storage prefix for a session's PCM chunks.
func chunkPrefixFor(guildID, sessionID string) string {
	return fmt.Sprintf("sessions/%s/%s/chunks", guildID, sessionID)
}

// userChunkPrefix returns the per-user sub-prefix under a session's chunk prefix.
// Each speaker's PCM lives under its own userID directory so the worker can
// transcribe tracks separately and merge them with speaker labels.
func userChunkPrefix(prefix, userID string) string {
	return fmt.Sprintf("%s/%s", prefix, userID)
}

// chunkKey returns the storage key for chunk number seq under a (per-user)
// prefix, zero-padded so lexical order matches chronological order when the
// worker lists and stitches them.
func chunkKey(prefix string, seq int) string {
	return fmt.Sprintf("%s/chunk-%06d.pcm", prefix, seq)
}

// maxChunkSeq returns the highest chunk sequence number embedded in a set of
// chunk keys (chunk-NNNNNN.pcm), or 0 if none. Used at resume to continue
// numbering after existing per-user chunks.
func maxChunkSeq(keys []string) int {
	max := 0
	for _, k := range keys {
		base := k
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		var seq int
		if _, err := fmt.Sscanf(base, "chunk-%06d.pcm", &seq); err == nil && seq > max {
			max = seq
		}
	}
	return max
}

// mustSessionUUID parses a session ID string, returning the zero UUID on error
// (callers only use it for best-effort DB writes that tolerate a bad ID).
func mustSessionUUID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}
	}
	return id
}

// checkpointLoop periodically flushes newly-mixed PCM to object storage until
// the recording stops, so a pod crash loses at most one interval of unsaved
// tail plus the downtime window. It also heartbeats the DB row so the reaper
// can tell a live session from an abandoned one.
func (r *recording) checkpointLoop() {
	defer close(r.doneCh)
	defer func() {
		if rec := recover(); rec != nil {
			metrics.PanicsRecovered.WithLabelValues("goroutine").Inc()
			r.g.log.Error("checkpoint loop panicked; recovered",
				"panic", fmt.Sprintf("%v", rec), "stack", string(debug.Stack()),
				"guild", r.guildID, "session", r.sessionID)
		}
	}()
	ticker := time.NewTicker(checkpointInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			if err := r.checkpoint(); err != nil {
				r.g.log.Warn("checkpoint failed", "guild", r.guildID, "session", r.sessionID, "err", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := r.g.store.TouchSessionHeartbeat(ctx, mustSessionUUID(r.sessionID)); err != nil {
				r.g.log.Warn("heartbeat failed", "session", r.sessionID, "err", err)
			}
			cancel()
		}
	}
}

// checkpoint uploads each user's frames accumulated since the last checkpoint
// as per-user raw-PCM chunks and advances the flushed watermark. A no-op when
// nothing new.
//
// It holds back the most recent frames (holdbackFrames) from each periodic
// flush so that late-arriving concurrent-speaker packets still land in the
// in-memory slot before it's frozen into a chunk. The final flush at stop time
// passes holdback=0 to drain everything.
func (r *recording) checkpoint() error { return r.checkpointUpTo(holdbackFrames) }

// holdbackFrames is ~1s of 20ms frames kept un-flushed on periodic checkpoints
// so concurrent-speaker mixing settles before a slot is frozen.
const holdbackFrames = 50

// checkpointUpTo flushes every user track, holding back the most recent
// `holdback` frames of each. Each track is uploaded to its own per-user chunk
// key; on success the flushed frames are dropped from that track and its
// frameBase advances (freeing RAM). Tracks are handled independently so one
// user's failed upload doesn't stall another's.
func (r *recording) checkpointUpTo(holdback int) error {
	// Snapshot the per-track flush work under the lock.
	type flushJob struct {
		userID     string
		seq        int
		flushCount int
		pcm        []int16
	}
	r.mu.Lock()
	jobs := make([]flushJob, 0, len(r.tracks))
	for uid, t := range r.tracks {
		flushCount := len(t.frames) - holdback
		if flushCount <= 0 {
			continue
		}
		pcm := make([]int16, 0, flushCount*audio.FrameSize*audio.Channels)
		for _, f := range t.frames[:flushCount] {
			pcm = append(pcm, f...)
		}
		jobs = append(jobs, flushJob{userID: uid, seq: t.chunkSeq, flushCount: flushCount, pcm: pcm})
	}
	prefix := r.chunkPrefix
	r.mu.Unlock()

	if len(jobs) == 0 {
		return nil
	}

	var firstErr error
	for _, j := range jobs {
		buf := new(bytes.Buffer)
		if err := binary.Write(buf, binary.LittleEndian, j.pcm); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("encode chunk (user %s): %w", j.userID, err)
			}
			continue
		}
		key := chunkKey(userChunkPrefix(prefix, j.userID), j.seq)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err := r.g.storage.Put(ctx, key, "application/octet-stream", buf)
		cancel()
		if err != nil {
			// Leave this track's frames in place so the same chunk is retried
			// next tick; don't advance its watermark.
			if firstErr == nil {
				firstErr = fmt.Errorf("upload chunk %d (user %s): %w", j.seq, j.userID, err)
			}
			continue
		}

		// Upload succeeded: drop the flushed frames from the front of this track
		// and advance its frameBase so their RAM is freed. Re-slice into a fresh
		// backing array so the old array can be GC'd.
		r.mu.Lock()
		if t := r.tracks[j.userID]; t != nil && t.chunkSeq == j.seq && len(t.frames) >= j.flushCount {
			remaining := t.frames[j.flushCount:]
			kept := make([][]int16, len(remaining))
			copy(kept, remaining)
			t.frames = kept
			t.frameBase += j.flushCount
			t.chunkSeq++
		}
		r.mu.Unlock()
		r.g.log.Info("checkpointed recording chunk",
			"guild", r.guildID, "session", r.sessionID, "user", j.userID, "seq", j.seq, "frames", j.flushCount)
	}
	return firstErr
}

// ReceiveOpusFrame is called by disgo for every received Opus frame with the
// resolved speaking user. It decodes and mixes the frame into the session
// timeline, and persists the participant the first time a user is seen.
// It satisfies voice.OpusFrameReceiver.
func (r *recording) ReceiveOpusFrame(userID snowflake.ID, pkt *voice.Packet) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			metrics.PanicsRecovered.WithLabelValues("goroutine").Inc()
			r.g.log.Error("voice frame handler panicked; recovered",
				"panic", fmt.Sprintf("%v", rec), "stack", string(debug.Stack()),
				"guild", r.guildID, "session", r.sessionID)
		}
	}()
	if pkt == nil || len(pkt.Opus) == 0 {
		return nil
	}

	// Persist the speaker the first time we see them (best effort, off-thread).
	uid := ""
	if userID != 0 {
		uid = userID.String()
		r.mu.Lock()
		firstSeen := !r.seen[uid]
		if firstSeen {
			r.seen[uid] = true
		}
		sessionID, guildID := r.sessionID, r.guildID
		r.mu.Unlock()
		if firstSeen {
			go r.persistParticipant(sessionID, guildID, uid)
		}
	}

	// Cap retained frames to bound memory (20ms/frame -> minutes*60*50), counted
	// across all per-user tracks so total RAM stays bounded regardless of party
	// size.
	maxFrames := 0
	if m := r.g.cfg.Audio.MaxSessionMinutes; m > 0 {
		maxFrames = m * 60 * 50
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if maxFrames > 0 && r.totalFramesAll >= maxFrames {
		if !r.capped {
			r.capped = true
			r.g.log.Warn("recording hit max length; further audio dropped",
				"guild", r.guildID, "session", r.sessionID, "maxMinutes", r.g.cfg.Audio.MaxSessionMinutes)
		}
		return nil
	}

	// Route this SSRC to its speaking user so frames land in the right track.
	// (disgo resolves SSRC->user; fall back to the SSRC itself if unknown so
	// audio is never silently dropped.)
	if uid == "" {
		if u, ok := r.ssrcUser[pkt.SSRC]; ok {
			uid = u
		} else {
			uid = fmt.Sprintf("ssrc-%d", pkt.SSRC)
		}
	}
	r.ssrcUser[pkt.SSRC] = uid

	dec, ok := r.decoders[pkt.SSRC]
	if !ok {
		d, derr := gopus.NewDecoder(audio.SampleRate, audio.Channels)
		if derr != nil {
			return nil
		}
		dec = d
		r.decoders[pkt.SSRC] = dec
		// Anchor this speaker's RTP clock to the timeline via wall-clock arrival,
		// so later frames are placed by RTP-delta and pauses leave empty slots
		// instead of collapsing and drifting.
		r.anchor[pkt.SSRC] = streamAnchor{
			baseTS:    pkt.Timestamp,
			baseFrame: r.wallClockFrame(),
		}
	}
	pcm, derr := dec.Decode(pkt.Opus, audio.FrameSize, false)
	if derr != nil {
		return nil
	}
	r.mixFrame(uid, r.frameIndexFor(pkt.SSRC, pkt.Timestamp), pcm)
	return nil
}

// CleanupUser is called by disgo when a user disconnects. We keep any audio
// already captured; nothing to release per-user. Satisfies OpusFrameReceiver.
func (r *recording) CleanupUser(_ snowflake.ID) {}

// Close is called by disgo when the receiver is torn down. The mixed buffer is
// owned by stop(); nothing to do here. Satisfies OpusFrameReceiver.
func (r *recording) Close() {}

// persistParticipant resolves a display name (guild nick > global name > ID) and
// upserts the participant row. Errors are logged but never fatal.
func (r *recording) persistParticipant(sessionID, guildID, userID string) {
	defer func() {
		if rec := recover(); rec != nil {
			metrics.PanicsRecovered.WithLabelValues("goroutine").Inc()
			r.g.log.Error("persistParticipant panicked; recovered",
				"panic", fmt.Sprintf("%v", rec), "stack", string(debug.Stack()),
				"session", sessionID, "user", userID)
		}
	}()
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return
	}
	name := r.g.displayName(guildID, userID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.g.store.RecordParticipant(ctx, sid, userID, name); err != nil {
		r.g.log.Warn("record participant", "err", err, "session", sessionID, "user", userID)
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

// mixFrame writes a decoded stereo frame into the given user's track at ABSOLUTE
// timeline index idx, extending that track's sliding window as needed. Frames
// whose absolute index is below the track's frameBase have already been
// checkpointed and freed, so a late packet for one is dropped. A corrupt/huge
// RTP-derived idx is clamped (dropped) rather than allowed to force a giant
// allocation. A user's own overlapping packets for the same slot are summed.
// Caller holds r.mu.
func (r *recording) mixFrame(userID string, idx int, pcm []int16) {
	if idx < 0 {
		return
	}
	// Respect the overall session cap (absolute).
	if m := r.g.cfg.Audio.MaxSessionMinutes; m > 0 && idx >= m*60*50 {
		return
	}
	t := r.tracks[userID]
	if t == nil {
		// New track. A resumed session already has startSeq chunks in storage for
		// (potentially) this user, so continue numbering after them.
		t = &userTrack{chunkSeq: r.startSeq + 1}
		r.tracks[userID] = t
	}
	// Already flushed & freed — can't retroactively mix. Rare (only within the
	// checkpoint holdback window); the frame is already durably uploaded.
	if idx < t.frameBase {
		return
	}
	local := idx - t.frameBase
	// A legitimate gap is silence; more than ~1h ahead of the window tail
	// indicates a bogus timestamp — drop it rather than allocate.
	const maxGapFrames = 180000
	if local > len(t.frames)+maxGapFrames {
		return
	}
	for len(t.frames) <= local {
		t.frames = append(t.frames, make([]int16, audio.FrameSize*audio.Channels))
	}
	// Track the per-track high-water mark and the cross-track total for the cap.
	if abs := t.frameBase + len(t.frames); abs > t.totalFrames {
		delta := abs - t.totalFrames
		t.totalFrames = abs
		r.totalFramesAll += delta
	}
	dst := t.frames[local]
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

// stop leaves the channel, flushes any un-checkpointed tail PCM as a final
// chunk, and returns the session's chunk prefix (which the worker reassembles
// into a single WAV) plus the recorded duration.
func (m *voiceManager) stop(ctx context.Context, guildID string) (string, time.Duration, error) {
	m.mu.Lock()
	r, ok := m.rec[guildID]
	if ok {
		delete(m.rec, guildID)
	}
	m.mu.Unlock()
	if !ok {
		m.g.log.Warn("stop called but no in-memory recording for guild",
			"guild", guildID)
		return "", 0, fmt.Errorf("no recording for guild %s", guildID)
	}

	duration := time.Since(r.started)

	// Stop the checkpoint loop and wait for it to exit so it can't race with our
	// final flush.
	close(r.stopCh)
	select {
	case <-r.doneCh:
	case <-time.After(5 * time.Second):
	}

	// Close the connection; disgo stops the receiver (Close/CleanupUser) and
	// tears down the DAVE session and UDP socket.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	r.conn.Close(closeCtx)
	closeCancel()

	// Flush the final tail (all remaining frames, including the held-back ones)
	// so the stored chunks cover the whole recording.
	if err := r.checkpointUpTo(0); err != nil {
		// Non-fatal: earlier chunks are still usable; log and continue.
		m.g.log.Warn("final checkpoint flush failed",
			"guild", guildID, "session", r.sessionID, "err", err)
	}
	_ = ctx // reassembly happens in the worker; nothing else to upload here.
	return r.chunkPrefix, duration, nil
}

// stopAll is called on shutdown (e.g. SIGTERM during a rollout). It stops the
// checkpoint loops and flushes each recording's tail so the audio captured so
// far is durably in storage, then closes the voice connections. Sessions are
// LEFT in 'recording' status (heartbeat frozen) so the next pod can resume them
// on startup; if none does, the reaper finalizes them from their chunks.
func (m *voiceManager) stopAll() {
	m.mu.Lock()
	recs := make([]*recording, 0, len(m.rec))
	for _, r := range m.rec {
		recs = append(recs, r)
	}
	m.rec = make(map[string]*recording)
	m.mu.Unlock()

	for _, r := range recs {
		close(r.stopCh)
		select {
		case <-r.doneCh:
		case <-time.After(3 * time.Second):
		}
		if err := r.checkpointUpTo(0); err != nil {
			m.g.log.Warn("flush recording on shutdown", "session", r.sessionID, "err", err)
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		r.conn.Close(closeCtx)
		closeCancel()
		m.g.log.Info("recording checkpointed for resume on shutdown",
			"guild", r.guildID, "session", r.sessionID)
	}
}

// resumeActive is called on startup. For every session still in 'recording'
// status (owned by a pod that died or was rolled), it rejoins the voice channel
// and continues recording into new chunks after the ones already in storage, so
// only the downtime window is lost. If rejoining isn't possible, the session is
// left for the reaper to finalize from its existing chunks.
func (m *voiceManager) resumeActive(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			metrics.PanicsRecovered.WithLabelValues("goroutine").Inc()
			m.g.log.Error("resumeActive panicked; recovered",
				"panic", fmt.Sprintf("%v", rec), "stack", string(debug.Stack()))
		}
	}()
	sessions, err := m.g.store.ListRecordingSessions(ctx)
	if err != nil {
		m.g.log.Error("list recording sessions for resume", "err", err)
		return
	}
	for _, sess := range sessions {
		if sess.VoiceChannelID == "" {
			m.g.log.Warn("cannot resume session with no voice channel; leaving for reaper",
				"session", sess.ID, "guild", sess.GuildID)
			continue
		}
		// Continue chunk numbering after whatever is already in storage. Chunks
		// are per-user (chunkPrefix/<userID>/chunk-NNN.pcm); use the highest chunk
		// number seen across all users so a resumed track never overwrites its
		// existing chunks (per-user numbering is independent, so the shared base
		// is safe and simply leaves gaps).
		prefix := sess.ChunkPrefix
		if prefix == "" {
			prefix = chunkPrefixFor(sess.GuildID, sess.ID.String())
		}
		lastSeq := 0
		if keys, lerr := m.g.storage.List(ctx, prefix); lerr == nil {
			lastSeq = maxChunkSeq(keys)
		}
		if err := m.join(sess.GuildID, sess.VoiceChannelID, sess.ID.String(), lastSeq); err != nil {
			m.g.log.Warn("failed to resume recording; leaving for reaper",
				"session", sess.ID, "guild", sess.GuildID, "channel", sess.VoiceChannelID, "err", err)
			continue
		}
		m.g.log.Info("resumed recording after restart",
			"session", sess.ID, "guild", sess.GuildID, "channel", sess.VoiceChannelID, "resumed_after_seq", lastSeq)
	}
}

// reapStale finalizes 'recording' sessions whose heartbeat has gone stale and
// which are not live on this pod. These are sessions whose owning pod died and
// which nothing resumed (e.g. the users left the voice channel, so resume
// couldn't rejoin). If any PCM chunks were checkpointed, it hands the session to
// the transcribe worker to reassemble what was captured before the crash;
// otherwise it marks the session failed so the guild isn't wedged.
func (m *voiceManager) reapStale(ctx context.Context, staleAfter time.Duration) {
	sessions, err := m.g.store.StaleRecordingSessions(ctx, staleAfter)
	if err != nil {
		m.g.log.Error("query stale sessions", "err", err)
		return
	}
	for _, sess := range sessions {
		// Skip anything we're actively recording (heartbeat should keep these
		// fresh, but guard against clock skew).
		if m.has(sess.GuildID) {
			continue
		}
		prefix := sess.ChunkPrefix
		if prefix == "" {
			prefix = chunkPrefixFor(sess.GuildID, sess.ID.String())
		}
		keys, lerr := m.g.storage.List(ctx, prefix)
		if lerr != nil {
			m.g.log.Warn("reaper: list chunks failed", "session", sess.ID, "err", lerr)
			continue
		}
		if len(keys) == 0 {
			// Nothing was captured; clear the wedged 'recording' row.
			m.g.log.Warn("reaper: stale session with no chunks; marking failed",
				"session", sess.ID, "guild", sess.GuildID)
			_ = m.g.store.SetSessionResult(ctx, sess.ID, "", "", "failed")
			continue
		}
		// Move to processing and enqueue transcription of the recovered chunks.
		if err := m.g.store.EndSession(ctx, sess.ID, 0); err != nil {
			m.g.log.Warn("reaper: end session", "session", sess.ID, "err", err)
			continue
		}
		if err := m.g.queue.Enqueue(ctx, queue.JobTranscribeSession, queue.TranscribeSessionPayload{
			SessionID: sess.ID.String(),
			GuildID:   sess.GuildID,
		}); err != nil {
			m.g.log.Warn("reaper: enqueue transcribe", "session", sess.ID, "err", err)
			continue
		}
		metrics.JobsEnqueued.WithLabelValues(string(queue.JobTranscribeSession)).Inc()
		m.g.log.Info("reaper: recovered crashed session from checkpoints",
			"session", sess.ID, "guild", sess.GuildID, "chunks", len(keys))
	}
}

// RunSessionReaper periodically finalizes abandoned 'recording' sessions (owning
// pod died and nothing resumed them). Runs in the gateway process alongside the
// reminder loop. staleAfter should comfortably exceed the checkpoint interval so
// a briefly-restarting pod that resumes isn't reaped out from under itself.
func (g *Gateway) RunSessionReaper(ctx context.Context, interval, staleAfter time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	g.log.Info("session reaper started", "interval", interval.String(), "staleAfter", staleAfter.String())
	for {
		select {
		case <-ctx.Done():
			g.log.Info("session reaper stopped")
			return
		case <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						metrics.PanicsRecovered.WithLabelValues("goroutine").Inc()
						g.log.Error("session reaper tick panicked; recovered",
							"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
					}
				}()
				rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				g.voice.reapStale(rctx, staleAfter)
			}()
		}
	}
}
