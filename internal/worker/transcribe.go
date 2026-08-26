package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"

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
func (w *Worker) handleTranscribeSession(ctx context.Context, raw json.RawMessage) error {
	p, err := unmarshal[queue.TranscribeSessionPayload](raw)
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	sessionID, err := uuid.Parse(p.SessionID)
	if err != nil {
		return fmt.Errorf("parse session id: %w", err)
	}

	sess, err := w.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	// Reassemble the recording from the PCM checkpoint chunks the gateway wrote
	// (sessions/<guild>/<session>/chunks/chunk-NNNNNN.pcm). Concatenating the
	// chunks in order reconstructs the whole recording; a mid-session pod crash
	// only drops the downtime window, not the earlier audio.
	audioBytes, err := w.reassembleSessionAudio(ctx, sess)
	if err != nil {
		w.markFailed(ctx, sessionID, p.GuildID, "I couldn't read the recording from storage.")
		return fmt.Errorf("reassemble audio: %w", err)
	}
	if len(audioBytes) == 0 {
		w.markFailed(ctx, sessionID, p.GuildID, "No audio was captured for that session.")
		return fmt.Errorf("session %s has no audio", sessionID)
	}

	// 2) Transcribe via LiteLLM (provider-agnostic).
	transcript, err := w.ai.Transcribe(ctx, w.cfg.LiteLLM.TranscribeModel, "session.wav", bytes.NewReader(audioBytes))
	if err != nil {
		metrics.AIRequests.WithLabelValues("transcribe", "error").Inc()
		w.markFailed(ctx, sessionID, p.GuildID, "Transcription failed. Please try again later.")
		return fmt.Errorf("transcribe: %w", err)
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
	if parts, perr := w.store.ListParticipants(ctx, sessionID); perr == nil {
		for _, p := range parts {
			if p.DisplayName != "" {
				participantNames = append(participantNames, p.DisplayName)
			}
		}
	}

	notes, err := w.ai.Chat(ctx, w.cfg.LiteLLM.Notes(), []litellm.Message{
		{Role: "system", Content: prompts.SessionNotesSystem},
		{Role: "user", Content: prompts.SessionNotesUser(campName, campSystem, campPremise, sessionDate, participantNames, transcript)},
	}, 2000)
	if err != nil {
		metrics.AIRequests.WithLabelValues("chat", "error").Inc()
		// We still keep the transcript so nothing is lost.
		_ = w.store.SetSessionResult(ctx, sessionID, transcript, "", "failed")
		w.notify(p.GuildID, sess.VoiceChannelID, "I transcribed the session but couldn't write the notes. Your transcript is saved.")
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

// reassembleSessionAudio downloads the session's PCM checkpoint chunks in order,
// concatenates them into one interleaved 48kHz stereo stream, optionally trims
// silence, and wraps the result in a WAV container ready for transcription.
// Concatenating the chunks reconstructs the full recording; if a pod crashed
// mid-session, only the chunks that made it to storage (everything up to the
// crash, minus at most one un-flushed interval) are present — the downtime gap
// is simply absent.
func (w *Worker) reassembleSessionAudio(ctx context.Context, sess *db.Session) ([]byte, error) {
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
	// Keys are zero-padded, so lexical order (as returned by List) is
	// chronological. Download and concatenate the raw little-endian int16 PCM.
	var pcm []int16
	for _, key := range keys {
		raw, gerr := w.storage.Get(ctx, key)
		if gerr != nil {
			// A missing/corrupt chunk shouldn't sink the whole recording; log and
			// skip it (leaves a small gap) rather than fail the session.
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
	if len(pcm) == 0 {
		return nil, nil
	}

	// Optionally drop near-silent frames to cut billed transcription minutes
	// (mirrors what the gateway used to do at stop time).
	if w.cfg.Audio.SilenceTrim {
		pcm = audio.TrimSilence(pcm, audio.FrameSize*audio.Channels, w.cfg.Audio.SilenceRMSThreshold)
	}

	var buf bytes.Buffer
	if err := audio.WriteWAV(&buf, pcm, audio.SampleRate, audio.Channels); err != nil {
		return nil, fmt.Errorf("encode wav: %w", err)
	}
	return buf.Bytes(), nil
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
