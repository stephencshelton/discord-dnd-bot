// Package prompts centralizes the system/user prompt templates used with the AI
// models. Keeping them here (not scattered in handlers) makes the bot's "voice"
// easy to tune and keeps provider-agnostic text separate from transport code.
package prompts

import (
	"fmt"
	"strings"
)

// SessionNotesSystem instructs the model to act as a fantasy scribe.
const SessionNotesSystem = `You are an expert tabletop RPG scribe.
You turn raw, messy voice-session transcripts into clear, evocative session notes.
Write in past tense. Be faithful to what happened; never invent major events.
Prefer the players' character names over real names when they are clear from context.`

// SessionNotesUser builds the user prompt for summarizing a transcript. It
// includes concrete session metadata — when the session occurred and who was in
// the voice call — so the model can attribute events accurately instead of
// guessing player identities from the transcript.
func SessionNotesUser(campaignName, system, premise, sessionDate string, participants []string, transcript string) string {
	var b strings.Builder
	b.WriteString("Campaign: ")
	b.WriteString(nonEmpty(campaignName, "Untitled Campaign"))
	b.WriteString("\nGame system: ")
	b.WriteString(nonEmpty(system, "unspecified"))
	if premise != "" {
		b.WriteString("\nPremise: ")
		b.WriteString(premise)
	}
	if sessionDate != "" {
		b.WriteString("\nSession date: ")
		b.WriteString(sessionDate)
	}
	if len(participants) > 0 {
		b.WriteString("\nParticipants (Discord voice call): ")
		b.WriteString(strings.Join(participants, ", "))
	}
	b.WriteString("\n\nProduce Markdown session notes with EXACTLY these sections:\n")
	b.WriteString("## Recap (2-4 sentences)\n")
	b.WriteString("## Key Events (bullet list, chronological)\n")
	b.WriteString("## NPCs & Factions (name — one line each; omit if none)\n")
	b.WriteString("## Locations (name — one line each; omit if none)\n")
	b.WriteString("## Loot & Rewards (omit if none)\n")
	b.WriteString("## Open Threads / Cliffhangers\n")
	b.WriteString("\nTranscript follows between the markers.\n<<<TRANSCRIPT\n")
	b.WriteString(transcript)
	b.WriteString("\nTRANSCRIPT>>>")
	return b.String()
}

// LoreSystem is used for the /lore free-form worldbuilding assistant.
const LoreSystem = `You are a creative tabletop RPG worldbuilding assistant.
Answer concisely and in-genre. When asked to create content (NPCs, locations,
plot hooks), make it usable at the table immediately. Keep responses under 250 words
unless asked for more.`

// LoreUser wraps a user's lore question with campaign context.
func LoreUser(campaignName, system, premise, question string) string {
	ctx := fmt.Sprintf("Campaign: %s | System: %s", nonEmpty(campaignName, "Untitled"), nonEmpty(system, "unspecified"))
	if premise != "" {
		ctx += " | Premise: " + premise
	}
	return ctx + "\n\nRequest: " + question
}

// RecapUser asks for a short "previously on" recap from prior session notes.
func RecapUser(campaignName string, priorNotes []string) string {
	var b strings.Builder
	b.WriteString("Write a dramatic \"Previously, on ")
	b.WriteString(nonEmpty(campaignName, "our campaign"))
	b.WriteString("...\" recap (max 150 words) from these prior session notes, newest last:\n\n")
	for i, n := range priorNotes {
		b.WriteString(fmt.Sprintf("--- Session %d ---\n%s\n\n", i+1, n))
	}
	return b.String()
}

// AskSystem instructs the model to answer strictly from retrieved session notes.
const AskSystem = `You are the campaign's loremaster and record-keeper.
Answer the player's question using ONLY the session note excerpts provided as context.
If the answer is not contained in the excerpts, say you don't have a record of it —
never invent events, names, or outcomes. Be concise and in-genre.`

// AskUser builds the grounded /ask prompt: the question plus the retrieved
// context passages. Passages are already ordered most-relevant first.
func AskUser(campaignName, question string, passages []string) string {
	var b strings.Builder
	b.WriteString("Campaign: ")
	b.WriteString(nonEmpty(campaignName, "Untitled Campaign"))
	b.WriteString("\n\nContext — session note excerpts (most relevant first):\n")
	if len(passages) == 0 {
		b.WriteString("(no matching session notes)\n")
	}
	for i, p := range passages {
		fmt.Fprintf(&b, "\n--- Excerpt %d ---\n%s\n", i+1, strings.TrimSpace(p))
	}
	b.WriteString("\nQuestion: ")
	b.WriteString(strings.TrimSpace(question))
	b.WriteString("\n\nAnswer using only the excerpts above.")
	return b.String()
}

// ArtPrompt refines a user's scene description into an image-generation prompt.
func ArtPrompt(system, scene string) string {
	styleHint := "digital fantasy illustration, dramatic lighting, painterly, high detail"
	if strings.Contains(strings.ToLower(system), "cyberpunk") || strings.Contains(strings.ToLower(system), "shadowrun") {
		styleHint = "neon cyberpunk concept art, cinematic, high detail"
	}
	return fmt.Sprintf("%s. Style: %s. No text or watermarks.", scene, styleHint)
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
