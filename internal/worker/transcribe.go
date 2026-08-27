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

	// Reassemble the recording from the per-user PCM checkpoint chunks the gateway
	// wrote (sessions/<guild>/<session>/chunks/<userID>/chunk-NNNNNN.pcm). Each
	// user's chunks concatenate into that speaker's own track; a mid-session pod
	// crash only drops the downtime window, not the earlier audio.
	tracks, err := w.reassembleSessionAudio(ctx, sess)
	if err != nil {
		// Storage read failure is usually transient — only tell the players it
		// failed once we've stopped retrying.
		if lastAttempt {
			w.markFailed(ctx, sessionID, p.GuildID, "I couldn't read the recording from storage.")
		}
		return fmt.Errorf("reassemble audio: %w", err)
	}
	if len(tracks) == 0 {
		// No audio ever made it to storage — retrying can't conjure it.
		w.markFailed(ctx, sessionID, p.GuildID, "No audio was captured for that session.")
		return queue.Permanent(fmt.Errorf("session %s has no audio", sessionID))
	}

	// 2) Transcribe each speaker's track separately (provider-agnostic, via the
	// long-timeout client), then merge the timestamped segments across speakers
	// into one chronological, speaker-labeled transcript.
	transcript, err := w.transcribeTracks(ctx, tracks, names)
	if err != nil {
		metrics.AIRequests.WithLabelValues("transcribe", "error").Inc()
		if lastAttempt {
			w.markFailed(ctx, sessionID, p.GuildID, "Transcription failed. Please try again later.")
		}
		return fmt.Errorf("transcribe: %w", err)
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

	// 4b) Embed the notes for grounded /ask retrieval. Best-effort: a failure
	// here must not fail the session (the notes are already saved and posted).
	if err := w.embedSessionNotes(ctx, sessionID, sess.CampaignID, notes); err != nil {
		metrics.AIRequests.WithLabelValues("embed", "error").Inc()
		w.log.Warn("embed session notes", "session", sessionID, "err", err)
	} else {
		metrics.AIRequests.WithLabelValues("embed", "ok").Inc()
	}

	// 5) Post the notes to the guild's notes channel (fall back to the voice
	// channel the session was recorded in).
	channelID := w.notesChannel(ctx, p.GuildID, sess.VoiceChannelID)
	w.postNotes(channelID, campName, notes)
	return nil
}

// userAudio is one speaker's reassembled recording, ready for transcription.
// It carries raw interleaved PCM (not a pre-encoded WAV) so transcribeTracks
// can encode one bounded segment at a time — holding a whole multi-hour track's
// WAV copy in memory is what OOM-killed the worker.
type userAudio struct {
	userID string
	pcm    []int16
}

// reassembleSessionAudio downloads the session's per-user PCM checkpoint chunks
// and reassembles one WAV per speaker. Chunks live under
// sessions/<guild>/<session>/chunks/<userID>/chunk-NNNNNN.pcm; concatenating a
// user's chunks in order reconstructs that speaker's track. If a pod crashed
// mid-session, only the chunks that made it to storage are present — the
// downtime gap is simply absent. Tracks are returned sorted by userID for a
// stable transcript order.
//
// Backward compatibility: a session recorded before per-user tracks stored a
// single flat set of chunks directly under .../chunks/. Those are returned as
// one unlabeled track (userID "").
func (w *Worker) reassembleSessionAudio(ctx context.Context, sess *db.Session) ([]userAudio, error) {
	prefix := sess.ChunkPrefix
	if prefix == "" {
		prefix = fmt.Sprintf("sessions/%s/%s/chunks", sess.GuildID, sess.ID)
	}
	keys, err := w.storage.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("list chunks: %w", err)
	}
	if len(keys) == 0 {
		return nil, nil
	}

	// Group keys by user. A key looks like <prefix>/<userID>/chunk-NNN.pcm for
	// per-user tracks, or <prefix>/chunk-NNN.pcm for the legacy flat layout.
	byUser := map[string][]string{}
	for _, key := range keys {
		rest := strings.TrimPrefix(key, prefix)
		rest = strings.TrimPrefix(rest, "/")
		userID := ""
		if i := strings.Index(rest, "/"); i >= 0 {
			userID = rest[:i] // per-user subdir
		}
		byUser[userID] = append(byUser[userID], key)
	}

	userIDs := make([]string, 0, len(byUser))
	for uid := range byUser {
		userIDs = append(userIDs, uid)
	}
	sort.Strings(userIDs)

	var tracks []userAudio
	for _, uid := range userIDs {
		ukeys := byUser[uid]
		// Keys are zero-padded, so lexical order is chronological.
		sort.Strings(ukeys)
		var pcm []int16
		for _, key := range ukeys {
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
			pcm = append(pcm, samples...)
		}
		// Optionally drop near-silent frames to cut billed transcription minutes.
		if w.cfg.Audio.SilenceTrim {
			pcm = audio.TrimSilence(pcm, audio.FrameSize*audio.Channels, w.cfg.Audio.SilenceRMSThreshold)
		}
		if len(pcm) == 0 {
			continue // track was all silence
		}
		tracks = append(tracks, userAudio{userID: uid, pcm: pcm})
	}
	return tracks, nil
}

// transcribeTracks transcribes each speaker's track and concatenates the results
// into one speaker-labeled transcript. Each user's block is prefixed with their
// display name (falling back to the userID, or "Unknown speaker") so the notes
// summarizer knows who said what. A single flat/legacy track (userID "") is
// returned unlabeled. Uses the long-timeout transcribe client.
//
// Each track is split into <= cfg.Audio.TranscribeSegmentMinutes-long WAV
// segments (encoded one at a time) so peak memory — worker AND STT backend —
// scales with the segment length, not the whole (possibly multi-hour) session.
// A few seconds of overlap between segments means a word spanning a boundary is
// transcribed in both and not dropped. Processed tracks are freed as we go so
// we never hold every speaker's audio at once.
func (w *Worker) transcribeTracks(ctx context.Context, tracks []userAudio, names map[string]string) (string, error) {
	// Segment window sizing (interleaved sample count across channels).
	segMinutes := w.cfg.Audio.TranscribeSegmentMinutes
	segSamples := 0
	if segMinutes > 0 {
		segSamples = segMinutes * 60 * audio.SampleRate * audio.Channels
	}
	// ~3s of overlap so a word straddling a segment boundary isn't lost.
	overlapSamples := 3 * audio.SampleRate * audio.Channels

	legacySingle := len(tracks) == 1 && tracks[0].userID == ""

	var b strings.Builder
	for i := range tracks {
		t := tracks[i]
		text, err := w.transcribeOneTrack(ctx, t.pcm, segSamples, overlapSamples)
		// Free this speaker's PCM before moving to the next track.
		tracks[i].pcm = nil
		if err != nil {
			return "", fmt.Errorf("transcribe track %s: %w", t.userID, err)
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		// Legacy single unlabeled track: return the text as-is.
		if legacySingle {
			return text, nil
		}
		label := names[t.userID]
		if label == "" {
			if t.userID == "" {
				label = "Unknown speaker"
			} else {
				label = t.userID
			}
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "=== %s ===\n%s", label, text)
	}
	return b.String(), nil
}

// transcribeOneTrack transcribes a single speaker's PCM track by splitting it
// into bounded WAV segments and joining the per-segment text. Each segment's
// WAV is encoded, sent, and discarded before the next is encoded, so worker
// memory stays flat regardless of track length.
func (w *Worker) transcribeOneTrack(ctx context.Context, pcm []int16, segSamples, overlapSamples int) (string, error) {
	segments := audio.SegmentPCM(pcm, segSamples, overlapSamples)
	var b strings.Builder
	for _, seg := range segments {
		var buf bytes.Buffer
		if err := audio.WriteWAV(&buf, seg, audio.SampleRate, audio.Channels); err != nil {
			return "", fmt.Errorf("encode wav segment: %w", err)
		}
		text, err := w.transcribeAI.Transcribe(ctx, w.cfg.LiteLLM.TranscribeModel, "session.wav", &buf)
		// Release the segment's WAV bytes promptly.
		buf.Reset()
		if err != nil {
			return "", err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(text)
	}
	return b.String(), nil
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
