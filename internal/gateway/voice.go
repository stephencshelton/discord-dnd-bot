package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"runtime/debug"
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
// tagged by SSRC (per speaker); each stream is decoded and time-aligned into
// one mixed PCM buffer so transcription gets a single file.
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

	// chunkPrefix is the object-storage prefix for this session's PCM checkpoints
	// (chunkPrefix/chunk-000001.pcm, ...). chunkSeq is the next chunk number.
	chunkPrefix string
	chunkSeq    int
	// stop signals the checkpoint loop to exit; done is closed when it has.
	stopCh chan struct{}
	doneCh chan struct{}

	mu       sync.Mutex
	decoders map[uint32]*gopus.Decoder // per-SSRC Opus decoder
	seen     map[string]bool           // userIDs already persisted this session
	capped   bool
	// frames holds mixed stereo PCM, one 20ms slot per shared timeline index.
	// Packets are placed by RTP timestamp (see anchor) so concurrent speakers
	// stay aligned and silence gaps leave empty slots instead of collapsing,
	// which previously caused drift on long sessions.
	frames [][]int16
	// flushed is the number of leading frames already checkpointed to storage;
	// each checkpoint uploads frames[flushed:] and advances flushed.
	flushed int
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

	// Clear any lingering/ghost voice session before joining. A prior attempt
	// (or a crashed pod) can leave Discord believing the bot is still connected
	// to a voice channel in this guild; when that happens the new join op is
	// accepted but Discord withholds VOICE_SERVER_UPDATE/VOICE_STATE_UPDATE until
	// something forces a reconciliation, so conn.Open hangs the full 30s and then
	// only gets the events once our timeout sends the leave. To avoid that, send
	// an explicit leave (channel_id=null) first, drop any stale disgo conn, and
	// give Discord a moment to settle before the real join.
	resetCtx, resetCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if verr := m.g.client.UpdateVoiceState(resetCtx, gid, nil, false, false); verr != nil {
		m.g.log.Debug("pre-join voice-state reset failed (continuing)",
			"guild", guildID, "err", verr)
	}
	resetCancel()
	m.g.client.VoiceManager.RemoveConn(gid)
	// Brief settle so Discord drops the old voice session before we re-join.
	time.Sleep(1 * time.Second)

	conn := m.g.client.VoiceManager.CreateConn(gid)

	r := &recording{
		conn:        conn,
		sessionID:   sessionID,
		guildID:     guildID,
		channelID:   channelID,
		started:     time.Now(),
		g:           m.g,
		chunkPrefix: chunkPrefixFor(guildID, sessionID),
		chunkSeq:    startSeq + 1,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
		decoders:    make(map[uint32]*gopus.Decoder),
		seen:        make(map[string]bool),
		anchor:      make(map[uint32]streamAnchor),
	}

	// Open the connection: self-mute (we never speak) but NOT self-deaf (we must
	// hear). disgo negotiates the DAVE/E2EE handshake as part of Open, and only
	// returns once the voice UDP + encryption (incl. DAVE) handshake completes.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	openStart := time.Now()
	if err := conn.Open(ctx, cid, true /*selfMute*/, false /*selfDeaf*/); err != nil {
		// Inspect the voice gateway state to localize the stall. If the gateway
		// reached Ready (SSRC assigned) but Open still timed out, the UDP media
		// path never completed the IP-discovery/SessionDescription round-trip —
		// almost always outbound voice UDP being blocked by cluster egress (NAT
		// gateway / security group only allowing TCP 443 for the websocket). If
		// the gateway never reached Ready, the voice websocket itself stalled.
		var gwStatus voice.Status = -1
		var ssrc uint32
		if gw := conn.Gateway(); gw != nil {
			gwStatus = gw.Status()
			ssrc = gw.SSRC()
		}
		m.g.log.Error("voice conn open failed",
			"guild", guildID, "channel", channelID, "session", sessionID,
			"elapsed_ms", time.Since(openStart).Milliseconds(),
			"voice_gateway_status", int(gwStatus), "ssrc", ssrc,
			"hint", "if status=7 (Ready) & ssrc!=0 the voice websocket is fine but UDP media is blocked (check egress/UDP)",
			"err", err)
		// Tear down the half-open connection so it doesn't linger/retry.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn.Close(closeCtx)
		closeCancel()
		return err
	}
	m.g.log.Info("voice conn opened",
		"guild", guildID, "channel", channelID, "session", sessionID,
		"elapsed_ms", time.Since(openStart).Milliseconds())

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

// chunkKey returns the storage key for chunk number seq (zero-padded so lexical
// order matches chronological order when the worker lists and stitches them).
func chunkKey(prefix string, seq int) string {
	return fmt.Sprintf("%s/chunk-%06d.pcm", prefix, seq)
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

// checkpoint uploads the frames accumulated since the last checkpoint as one
// raw-PCM chunk and advances the flushed watermark. A no-op when nothing new.
//
// It holds back the most recent frames (holdbackFrames) from each periodic
// flush so that late-arriving concurrent-speaker packets still land in the
// in-memory slot before it's frozen into a chunk. The final flush at stop time
// passes final=true to drain everything.
func (r *recording) checkpoint() error { return r.checkpointUpTo(holdbackFrames) }

// holdbackFrames is ~1s of 20ms frames kept un-flushed on periodic checkpoints
// so concurrent-speaker mixing settles before a slot is frozen.
const holdbackFrames = 50

func (r *recording) checkpointUpTo(holdback int) error {
	r.mu.Lock()
	upTo := len(r.frames) - holdback
	if upTo <= r.flushed {
		r.mu.Unlock()
		return nil
	}
	// Copy the settled new frames out under the lock, then release it before the
	// upload.
	newFrames := r.frames[r.flushed:upTo]
	pcm := make([]int16, 0, len(newFrames)*audio.FrameSize*audio.Channels)
	for _, f := range newFrames {
		pcm = append(pcm, f...)
	}
	seq := r.chunkSeq
	prefix := r.chunkPrefix
	newFlushed := upTo
	r.mu.Unlock()

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, pcm); err != nil {
		return fmt.Errorf("encode chunk: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := r.g.storage.Put(ctx, chunkKey(prefix, seq), "application/octet-stream", buf); err != nil {
		return fmt.Errorf("upload chunk %d: %w", seq, err)
	}

	// Only advance the watermarks after a successful upload so a failed PUT is
	// retried (with the same frames) on the next tick.
	r.mu.Lock()
	r.flushed = newFlushed
	r.chunkSeq++
	r.mu.Unlock()
	r.g.log.Info("checkpointed recording chunk",
		"guild", r.guildID, "session", r.sessionID, "seq", seq, "frames", len(newFrames))
	return nil
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
	if userID != 0 {
		uid := userID.String()
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

	// Cap retained frames to bound memory (20ms/frame -> minutes*60*50).
	maxFrames := 0
	if m := r.g.cfg.Audio.MaxSessionMinutes; m > 0 {
		maxFrames = m * 60 * 50
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if maxFrames > 0 && len(r.frames) >= maxFrames {
		if !r.capped {
			r.capped = true
			r.g.log.Warn("recording hit max length; further audio dropped",
				"guild", r.guildID, "session", r.sessionID, "maxMinutes", r.g.cfg.Audio.MaxSessionMinutes)
		}
		return nil
	}

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
	r.mixFrame(r.frameIndexFor(pkt.SSRC, pkt.Timestamp), pcm)
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
		// Continue chunk numbering after whatever is already in storage.
		prefix := sess.ChunkPrefix
		if prefix == "" {
			prefix = chunkPrefixFor(sess.GuildID, sess.ID.String())
		}
		lastSeq := 0
		if keys, lerr := m.g.storage.List(ctx, prefix); lerr == nil {
			lastSeq = len(keys)
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
