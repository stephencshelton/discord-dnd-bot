# Design: Speaker-Attributed Transcription (per-user tracks)

Status: **IMPLEMENTED (Option A — per-speaker labeled blocks)**
Date: 2026-08-26

## Problem

Session summaries can't tell who said what. Discord *does* give us the speaking
user on every voice packet (`ReceiveOpusFrame(userID, pkt)` — that's how the
participant list is populated), but the gateway **sums every speaker into one
shared PCM timeline** (`recording.mixFrame` → `dst[i] = clip16(dst[i]+pcm[i])`).
By the time it reaches Whisper it's a single blended track with no channel
separation, and Whisper does no speaker diarization. Result: the transcript is a
flat wall of text; the summarizer can only attribute a line when a speaker says
their own name aloud.

## Goal

Produce a **speaker-labeled transcript** so the summarizer can attribute actions
to the right players:

```
[00:12:14] Steve: I attack the goblin.
[00:12:17] Dana: Roll for initiative first.
```

We already have reliable identity from Discord, so this is a bookkeeping problem
(keep tracks separate, merge by timestamp) — **not** an ML diarization problem.

## Approach (chosen): per-user tracks + labeled merge

Keep one PCM track **per user** instead of one mixed track. Transcribe each
user's track separately (Whisper returns per-segment timestamps), then interleave
all speakers' segments by start time into one labeled transcript, which is fed to
the summarizer.

Rejected alternative — **diarization** (pyannote on the mixed track): fuzzier
(guesses speaker boundaries, mishandles overlap), needs a new model/infra, and
still has to map "Speaker 1/2" back to Discord users. Not worth it when Discord
hands us ground-truth identity.

## Changes required

### 1. Gateway capture (`internal/gateway/voice.go`) — the hot path

- `recording.frames [][]int16` (single mixed sliding window) becomes
  **per-user**: `tracks map[string]*userTrack`, where each `userTrack` holds its
  own `frames [][]int16` + `frameBase`/`totalFrames`. `mixFrame` becomes
  `appendFrame(userID, absIdx, pcm)` writing into that user's track (no summing
  across users — a user's own overlapping packets are still summed, which
  effectively never happens).
- `ReceiveOpusFrame` routes by `userID.String()` (already available). Users with
  no decoder yet get a track + anchor lazily, same as today.
- Checkpointing: `checkpointUpTo` iterates each track and writes a **per-user
  chunk** to `chunks/<userID>/chunk-NNNNNN.pcm`. Chunk numbering per user.
  Heartbeat/holdback/resume logic is otherwise unchanged.
- Resume (`resumeActive`) and the reaper count chunks under the per-user prefixes.
- The session cap (`MaxSessionMinutes`) becomes a cap on **total retained frames
  across all tracks** so overall RAM stays bounded (see resource section).

### 2. Worker transcription (`internal/worker/transcribe.go`)

- `reassembleSessionAudio` returns **one WAV per user** (list `chunks/<uid>/…`,
  concat, TrimSilence, WriteWAV each).
- Transcribe each user's WAV with `verbose_json` / segment timestamps (needs a
  small addition to `litellm.Transcribe` to request + parse segment timestamps —
  OpenAI/Speaches support `response_format=verbose_json` with `segments[]`).
- Merge all users' segments into one list sorted by segment start time, prefix
  each with the participant's stored display name, and build the labeled
  transcript string. Feed that to the existing summarizer.
- Empty/near-silent user tracks are skipped (silence trim already handles most).

### 3. Storage layout

- Old: `sessions/<guild>/<session>/chunks/chunk-NNN.pcm` (one stream).
- New: `sessions/<guild>/<session>/chunks/<userID>/chunk-NNN.pcm`.
- Backward-compat: reassembly falls back to the flat layout if no per-user
  subdirs exist (so in-flight/legacy sessions still transcribe as a single
  unlabeled track).

### 4. LiteLLM client (`internal/litellm/client.go`)

- Add an option to request `verbose_json` and return `[]Segment{Start, End, Text}`
  alongside the plain text. Backward compatible (default stays plain text).

## Resource impact on the gateway

Baseline facts:
- Frame = `FrameSize(960) * Channels(2) * 2 bytes = 3840 B`, 50 frames/s.
- One continuous speaker ≈ **192 KB/s** of PCM = ~11.5 MB/min.
- Gateway pod today: **requests = limits = 1 CPU / 512Mi**, single replica.
- `checkpointInterval = 30s`, so RAM ≈ one interval of *actively spoken* audio
  per track + holdback, then flushed and freed.

### Memory

The key realization: **memory scales with concurrent speech, not with the number
of participants.** In a D&D session people take turns — usually 1 speaker at a
time, occasionally 2–3 overlapping. Empty time in a user's track is *not*
allocated (silence leaves no frames; a track only grows when that user actually
emits packets).

Per-track live window (30s of that user's *actual speech*, worst case fully
talking): `30s * 192 KB/s ≈ 5.6 MB`. So:

| Scenario | Concurrently speaking | Live PCM in RAM (pre-flush peak) |
|---|---|---|
| Today (mixed) | n/a (1 mixed track) | ~5.6 MB |
| Typical play (1 talker) | 1 | ~5.6 MB (same as today) |
| Lively (3 overlapping) | 3 | ~17 MB |
| Pathological (6 all talking 30s straight) | 6 | ~34 MB |

Plus small fixed per-user overhead: one `gopus.Decoder` (~tens of KB) + slice
headers + anchor entry per SSRC — negligible (a handful of users = well under
1 MB).

**Net memory increase: single-digit to low-tens of MB at peak**, and only while
multiple people talk simultaneously within a 30s window. Against a 512Mi limit
this is comfortably safe. To keep the hard cap bounded regardless of party size,
`MaxSessionMinutes` becomes a *total-frames-across-tracks* cap (same ceiling as
today, just shared).

> We can also cut the peak by shortening `checkpointInterval` (e.g. 30s→15s)
> which halves the live window at the cost of ~2× S3 PUT rate. Not needed at these
> sizes but available if a table ever has 10+ hot mics.

### CPU

Gateway CPU is dominated by **Opus decode**, which is *per received packet* and
therefore **unchanged** — we already decode every packet today regardless of who
sent it. Splitting the mix target doesn't add decode work.

- Mixing: today we sum into one buffer; new code writes into a per-user buffer.
  Same number of sample copies (actually slightly *fewer* adds, since we no
  longer sum across users). **≈ neutral, arguably a hair cheaper.**
- Checkpoint serialization: now N smaller chunks instead of 1 big one per
  interval — same total bytes, marginally more per-object overhead. Negligible
  (a few extra `binary.Write`/PUT calls every 30s).
- Extra allocations for per-user slices — minor GC pressure, bounded by the
  memory numbers above.

**Net CPU increase on the gateway: effectively zero** (within noise). No change
to the 1-CPU request/limit is needed.

### Where the *real* extra cost lands: the worker (not the gateway)

Transcription goes from **1 Whisper pass** over the mixed track to **N passes**
(one per speaking participant). Whisper cost ≈ linear in audio duration, so a
4-person session is ~4× the transcription CPU-time/wall-time on the STT pod.
Mitigations:
- Silence trim already removes each user's dead air, so a player who spoke 20% of
  a 3h session only costs ~36 min of transcription, not 3h.
- Per-user tracks are mostly silence-trimmed to just that user's speech, so the
  **sum of all tracks' billed minutes ≈ total spoken minutes** — i.e. close to
  the single mixed track's spoken minutes, just split into N calls with per-call
  overhead. The increase is modest, not literally N×, once silence is trimmed.
- The worker/STT already got the long-timeout hardening; N sequential passes fit
  within `WORKER_TRANSCRIBE_JOB_TIMEOUT` (4h).

## Risk / rollout

- Hot-path rewrite of `voice.go` — the most delicate part. Preserve the sliding-
  window free-after-upload behavior (the OOM fix) per track.
- Ship behind a config flag (e.g. `AUDIO_PER_USER_TRACKS`, default off first) so
  we can validate on a real session before making it the default.
- Reassembly keeps the flat-layout fallback so sessions recorded before the
  switch still transcribe.
- Full verification suite must stay green (build/vet/test -race/glci v2.13.1/
  gosec/helm) and the memory-leak guarantee must be re-checked per track.

## Estimate

Medium change: ~1–2 focused sessions. Bulk is `voice.go` (per-user sliding
windows + per-user checkpoint) and `transcribe.go` (per-user reassemble +
segment-merge), plus a small `litellm` segment-timestamp addition and chart flag.

## IMPLEMENTATION NOTE (2026-08-26) — Option A chosen

Shipped the **simplest** variant: per-user tracks + **per-speaker labeled
blocks**, NOT time-interleaved segment merging. This dropped all the
`verbose_json`/segment-timestamp/merge complexity (the litellm client stayed the
plain `Transcribe`). The notes summarizer only needs *who said what*; coarse
event/location ordering is reconstructed by the LLM from linguistic cues (and
each speaker's own block is already chronological), so Option A is effectively as
good as B for session-notes purposes. Upgrading A→B later is additive (swap the
per-track transcribe call for a segment version + merge) without redoing the
capture-side work. No feature flag (app not yet live).

### What actually shipped

- **`voice.go`**: `recording.frames` (one summed buffer) → `recording.tracks
  map[string]*userTrack`, each `userTrack{frames, frameBase, totalFrames,
  chunkSeq}`. `mixFrame(userID, absIdx, pcm)` writes into that user's track (no
  cross-user summing). `ssrcUser map[uint32]string` routes SSRC→user (falls back
  to `ssrc-<n>` if unresolved so audio is never dropped). Per-user sliding-window
  free-after-upload preserved (OOM fix, now per track). Session cap =
  `totalFramesAll` across all tracks. `startSeq` is the shared resume base;
  `mixFrame` seeds a new track's `chunkSeq = startSeq+1`.
- **Checkpoint**: `checkpointUpTo(holdback)` loops every track, uploads each to
  `chunkPrefix/<userID>/chunk-NNNNNN.pcm`, drops+advances that track on success;
  a failed per-user PUT is retried next tick without stalling others.
- **Resume**: `maxChunkSeq(keys)` parses the highest `chunk-NNN` across all users
  (recursive List) so a resumed session numbers new chunks after existing ones.
- **`transcribe.go`**: `reassembleSessionAudio` returns `[]userAudio{userID,wav}`
  (groups keys by the `<userID>` path segment, silence-trims + WAVs each, sorted
  by userID; legacy flat layout → one unlabeled track userID=""). New
  `transcribeTracks` transcribes each track with the long-timeout client and
  concatenates `=== <displayName> ===` blocks; single legacy track returned
  unlabeled. Empty overall transcript → markFailed.
- **Tests**: `TestMixFrameSeparatesUsers` (two users at the same slot stay
  separate, not summed) + `TestMixFrameStartSeqOffsetsChunkNumbering`.

### Storage layout change (no migration needed — S3 keys)

Old: `sessions/<guild>/<session>/chunks/chunk-NNN.pcm`
New: `sessions/<guild>/<session>/chunks/<userID>/chunk-NNN.pcm`
Reassembly auto-detects: keys with a `<userID>/` segment → per-user; keys
directly under `chunks/` → one legacy unlabeled track.
