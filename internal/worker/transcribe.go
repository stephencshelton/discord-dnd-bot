package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/google/uuid"

	"github.com/stephencshelton/discord-dnd-bot/internal/audio"
	"github.com/stephencshelton/discord-dnd-bot/internal/db"
	"github.com/stephencshelton/discord-dnd-bot/internal/litellm"
	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
	"github.com/stephencshelton/discord-dnd-bot/internal/prompts"
	"github.com/stephencshelton/discord-dnd-bot/internal/queue"
)

// handleTranscribeSession downloads the recorded audio, transcribes it, writes
// session notes with the chat model, persists both, and posts the notes to the
// guild's configured notes channel.
//
// lastAttempt is true when the worker will NOT retry this job again if it fails
// (retries exhausted). Transient failures only surface a user-visible "failed"
// message on the last attempt, so an intermittent LiteLLM/storage blip that
// succeeds on retry stays invisible to players. Failures that can never succeed
// on retry are wrapped with queue.Permanent so the worker drops them at once.
func (w *Worker) handleTranscribeSession(ctx context.Context, raw json.RawMessage, lastAttempt bool) error {
	p, err := unmarshal[queue.TranscribeSessionPayload](raw)
	if err != nil {
		return queue.Permanent(fmt.Errorf("decode payload: %w", err))
	}
	sessionID, err := uuid.Parse(p.SessionID)
	if err != nil {
		return queue.Permanent(fmt.Errorf("parse session id: %w", err))
	}

	sess, err := w.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	// Build a userID -> display name map so per-user tracks can be labeled with
	// the speaker's name in the transcript.
	names := map[string]string{}
	if parts, perr := w.store.ListParticipants(ctx, sessionID); perr == nil {
		for _, part := range parts {
			if part.DisplayName != "" {
				names[part.UserID] = part.DisplayName
			}
		}
	}

	// Reassemble + transcribe the recording from the per-user PCM checkpoint
	// chunks the gateway wrote (sessions/<guild>/<session>/chunks/<userID>/
	// chunk-NNNNNN.pcm). To keep worker memory bounded regardless of session
	// length or speaker count, this STREAMS each speaker's chunks through a
	// bounded segment buffer — it never holds a whole track (let alone all
	// tracks) in RAM, which is what OOM-killed the worker.
	transcript, hadAudio, err := w.transcribeSession(ctx, sess, names)
	if err != nil {
		metrics.AIRequests.WithLabelValues("transcribe", "error").Inc()
		// Transcription/storage failures are usually transient — only tell the
		// players it failed once we've stopped retrying.
		if lastAttempt {
			w.markFailed(ctx, sessionID, p.GuildID, "Transcription failed. Please try again later.")
		}
		return fmt.Errorf("transcribe: %w", err)
	}
	if !hadAudio {
		// No audio ever made it to storage — retrying can't conjure it.
		w.markFailed(ctx, sessionID, p.GuildID, "No audio was captured for that session.")
		return queue.Permanent(fmt.Errorf("session %s has no audio", sessionID))
	}
	if strings.TrimSpace(transcript) == "" {
		// The recording had no speech — a retry produces the same empty result.
		w.markFailed(ctx, sessionID, p.GuildID, "The recording had no discernible speech to transcribe.")
		return queue.Permanent(fmt.Errorf("session %s produced an empty transcript", sessionID))
	}
	metrics.AIRequests.WithLabelValues("transcribe", "ok").Inc()

	// 3) Summarize the transcript into structured session notes.
	camp, cerr := w.store.GetCampaign(ctx, sess.CampaignID)
	campName, campSystem, campPremise := "", "", ""
	if cerr == nil {
		campName, campSystem, campPremise = camp.Name, camp.System, camp.Premise
	}

	// Gather concrete session metadata so the notes know *when* the session
	// happened and *who* was in the call, rather than inferring it.
	sessionDate := sess.StartedAt.UTC().Format("Monday, 2 January 2006 15:04 MST")
	var participantNames []string
	for _, n := range names {
		participantNames = append(participantNames, n)
	}
	sort.Strings(participantNames)

	notes, err := w.ai.Chat(ctx, w.cfg.LiteLLM.Notes(), []litellm.Message{
		{Role: "system", Content: prompts.SessionNotesSystem},
		{Role: "user", Content: prompts.SessionNotesUser(campName, campSystem, campPremise, sessionDate, participantNames, transcript)},
	}, 2000)
	if err != nil {
		metrics.AIRequests.WithLabelValues("chat", "error").Inc()
		// A chat failure is usually transient. Keep the transcript either way so
		// nothing is lost, but only mark the session failed / notify players once
		// retries are exhausted — a retry may well produce the notes. Saving the
		// transcript with status "processing" lets a retry re-run summarization.
		if lastAttempt {
			_ = w.store.SetSessionResult(ctx, sessionID, transcript, "", "failed")
			w.notify(p.GuildID, sess.VoiceChannelID, "I transcribed the session but couldn't write the notes. Your transcript is saved.")
		} else {
			_ = w.store.SetSessionResult(ctx, sessionID, transcript, "", "processing")
		}
		return fmt.Errorf("summarize: %w", err)
	}
	metrics.AIRequests.WithLabelValues("chat", "ok").Inc()

	// 4) Persist results (status "complete" so recaps can find these notes).
	if err := w.store.SetSessionResult(ctx, sessionID, transcript, notes, "complete"); err != nil {
		return fmt.Errorf("save result: %w", err)
	}

	// 4b) Embed the full transcript for grounded /ask retrieval. The transcript
	// (not the summarized notes) is indexed so /ask can surface details that the
	// lossy summary may have dropped. Best-effort: a failure here must not fail
	// the session (the notes are already saved and posted).
	if err := w.embedSessionNotes(ctx, sessionID, sess.CampaignID, transcript); err != nil {
		metrics.AIRequests.WithLabelValues("embed", "error").Inc()
		w.log.Warn("embed session transcript", "session", sessionID, "err", err)
	} else {
		metrics.AIRequests.WithLabelValues("embed", "ok").Inc()
	}

	// 5) Post the notes to the guild's notes channel (fall back to the voice
	// channel the session was recorded in).
	channelID := w.notesChannel(ctx, p.GuildID, sess.VoiceChannelID)
	w.postNotes(channelID, campName, notes)
	return nil
}

// transcribeSession streams the session's per-user PCM checkpoint chunks
// through a bounded segment buffer and returns the merged, speaker-labeled
// transcript. Chunks live under sessions/<guild>/<session>/chunks/<userID>/
// chunk-NNNNNN.pcm (a pre-per-user-track session used a flat set of chunks
// directly under .../chunks/, surfaced as one unlabeled track, userID "").
//
// Memory: this is the OOM-safe path. It NEVER reassembles a whole track (let
// alone every speaker's track) in RAM. It processes one user at a time, and for
// each user streams chunks into an accumulator that flushes (encode WAV ->
// transcribe -> discard) whenever it fills a <= cfg.Audio.TranscribeSegmentMinutes
// window, keeping only a few seconds of overlap between flushes. Peak memory is
// therefore ~one segment (~11.5 MB/min of PCM) regardless of session length or
// speaker count. hadAudio is false only when no chunks exist at all.
func (w *Worker) transcribeSession(ctx context.Context, sess *db.Session, names map[string]string) (transcript string, hadAudio bool, err error) {
	prefix := sess.ChunkPrefix
	if prefix == "" {
		prefix = fmt.Sprintf("sessions/%s/%s/chunks", sess.GuildID, sess.ID)
	}
	keys, err := w.storage.List(ctx, prefix)
	if err != nil {
		return "", false, fmt.Errorf("list chunks: %w", err)
	}
	if len(keys) == 0 {
		return "", false, nil
	}

	// Group keys by user (strings only — cheap). A key looks like
	// <prefix>/<userID>/chunk-NNN.pcm for per-user tracks, or
	// <prefix>/chunk-NNN.pcm for the legacy flat layout (userID "").
	byUser := map[string][]string{}
	for _, key := range keys {
		rest := strings.TrimPrefix(strings.TrimPrefix(key, prefix), "/")
		userID := ""
		if i := strings.Index(rest, "/"); i >= 0 {
			userID = rest[:i]
		}
		byUser[userID] = append(byUser[userID], key)
	}
	userIDs := make([]string, 0, len(byUser))
	for uid := range byUser {
		userIDs = append(userIDs, uid)
	}
	sort.Strings(userIDs)

	// Segment window sizing (interleaved sample count across all channels).
	segSamples := 0
	if m := w.cfg.Audio.TranscribeSegmentMinutes; m > 0 {
		segSamples = m * 60 * audio.SampleRate * audio.Channels
	}
	overlapSamples := 3 * audio.SampleRate * audio.Channels // ~3s so boundary words aren't lost

	legacySingle := len(userIDs) == 1 && userIDs[0] == ""

	var b strings.Builder
	for _, uid := range userIDs {
		text, terr := w.transcribeUserTrack(ctx, sess, byUser[uid], segSamples, overlapSamples)
		if terr != nil {
			return "", true, fmt.Errorf("transcribe track %s: %w", uid, terr)
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if legacySingle {
			return text, true, nil
		}
		label := names[uid]
		if label == "" {
			if uid == "" {
				label = "Unknown speaker"
			} else {
				label = uid
			}
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "=== %s ===\n%s", label, text)
	}
	return b.String(), true, nil
}

// transcribeUserTrack streams one speaker's chunk keys (in chronological order)
// through a bounded accumulator, transcribing each ~segSamples-long WAV segment
// and joining the results. It downloads one chunk at a time and flushes a
// segment as soon as the buffer fills, so at most ~one segment plus one chunk of
// PCM is resident at any moment. segSamples <= 0 disables segmenting (buffer the
// whole track — only safe for short sessions).
func (w *Worker) transcribeUserTrack(ctx context.Context, sess *db.Session, keys []string, segSamples, overlapSamples int) (string, error) {
	// Keys are zero-padded, so lexical order == chronological order.
	sort.Strings(keys)

	var (
		out strings.Builder
		buf []int16 // current segment being accumulated
	)

	// transcribeBuf downmixes the buffered stereo PCM to mono, encodes it as one
	// mono WAV, transcribes it, and appends the text. Mono halves the bytes the
	// STT backend must decode/hold (transcription doesn't need stereo). Callers
	// manage what stays in buf afterward.
	transcribeBuf := func(seg []int16) error {
		if len(seg) == 0 {
			return nil
		}
		mono := audio.DownmixToMono(seg, audio.Channels)
		var wav bytes.Buffer
		if err := audio.WriteWAV(&wav, mono, audio.SampleRate, 1); err != nil {
			return fmt.Errorf("encode wav segment: %w", err)
		}
		text, err := w.transcribeAI.Transcribe(ctx, w.cfg.LiteLLM.TranscribeModel, "session.wav", &wav)
		wav.Reset() // release the WAV bytes promptly
		if err != nil {
			return err
		}
		if text = strings.TrimSpace(text); text != "" {
			if out.Len() > 0 {
				out.WriteByte(' ')
			}
			out.WriteString(text)
		}
		return nil
	}

	for _, key := range keys {
		raw, gerr := w.storage.Get(ctx, key)
		if gerr != nil {
			// A missing/corrupt chunk shouldn't sink the whole recording.
			w.log.Warn("skip unreadable chunk", "session", sess.ID, "key", key, "err", gerr)
			continue
		}
		samples := make([]int16, len(raw)/2)
		if err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, &samples); err != nil {
			w.log.Warn("skip undecodable chunk", "session", sess.ID, "key", key, "err", err)
			continue
		}
		// Optionally drop near-silent frames to cut billed transcription minutes.
		// TrimSilence is frame-aligned and chunks are whole 20ms frames, so
		// applying it per chunk matches trimming the concatenated track.
		if w.cfg.Audio.SilenceTrim {
			samples = audio.TrimSilence(samples, audio.FrameSize*audio.Channels, w.cfg.Audio.SilenceRMSThreshold)
		}
		if len(samples) == 0 {
			continue
		}
		buf = append(buf, samples...)

		// Flush whole segments as the buffer fills (a single fat chunk could
		// fill more than one segment, so loop). Retain overlapSamples of context
		// after each flush so a word spanning the boundary is transcribed in the
		// next segment too.
		for segSamples > 0 && len(buf) >= segSamples {
			if err := transcribeBuf(buf[:segSamples]); err != nil {
				return "", err
			}
			advance := segSamples - overlapSamples
			if advance < 1 {
				advance = segSamples
			}
			// Copy the retained tail into a freshly sized slice so the large
			// backing array is released rather than pinned by a sub-slice.
			rest := buf[advance:]
			kept := make([]int16, len(rest))
			copy(kept, rest)
			buf = kept
		}
	}

	// Transcribe whatever remains (the final partial segment, or the whole
	// track when segmenting is disabled).
	if err := transcribeBuf(buf); err != nil {
		return "", err
	}
	return out.String(), nil
}

// notesChannel resolves where to post: the guild's configured notes channel if
// set, otherwise the provided fallback channel.
func (w *Worker) notesChannel(ctx context.Context, guildID, fallback string) string {
	if g, err := w.store.GetGuild(ctx, guildID); err == nil && g.NotesChannelID != "" {
		return g.NotesChannelID
	}
	return fallback
}

// postNotes sends the notes, chunking to respect Discord's 2000-char limit and
// attaching the full notes as a Markdown file for convenience.
func (w *Worker) postNotes(channelID, campaign, notes string) {
	if channelID == "" {
		w.log.Warn("no channel to post session notes")
		return
	}
	header := "📜 **Session notes are ready!**"
	if campaign != "" {
		header = fmt.Sprintf("📜 **%s — session notes are ready!**", campaign)
	}
	if err := w.sendMessage(channelID, discord.MessageCreate{Content: header}); err != nil {
		w.log.Error("post notes header", "err", err)
	}
	// Attach full notes as a file (avoids message-length juggling and is nicer
	// to copy into a campaign wiki).
	err := w.sendMessage(channelID, discord.MessageCreate{
		Files: []*discord.File{discord.NewFile("session-notes.md", "", bytes.NewReader([]byte(notes)))},
	})
	if err != nil {
		w.log.Error("attach notes file", "err", err)
		// Fall back to chunked inline messages.
		for _, chunk := range chunkString(notes, 1900) {
			if e := w.sendMessage(channelID, discord.MessageCreate{Content: chunk}); e != nil {
				w.log.Error("post notes chunk", "err", e)
			}
		}
	}
}

// markFailed records a failed session and notifies the channel.
func (w *Worker) markFailed(ctx context.Context, sessionID uuid.UUID, guildID, msg string) {
	_ = w.store.SetSessionResult(ctx, sessionID, "", "", "failed")
	// Best-effort notify in the notes channel.
	if ch := w.notesChannel(ctx, guildID, ""); ch != "" {
		w.notify(guildID, ch, "⚠️ "+msg)
	}
}

// notify posts a short message to a channel (best effort).
func (w *Worker) notify(_ string, channelID, msg string) {
	if channelID == "" {
		return
	}
	if err := w.sendMessage(channelID, discord.MessageCreate{Content: msg}); err != nil {
		w.log.Error("notify", "err", err)
	}
}

// chunkString splits s into pieces no longer than size runes.
func chunkString(s string, size int) []string {
	if size <= 0 {
		return []string{s}
	}
	var out []string
	r := []rune(s)
	for len(r) > size {
		out = append(out, string(r[:size]))
		r = r[size:]
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}
