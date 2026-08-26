// Package audio holds small, dependency-light audio helpers shared between the
// gateway (which records) and other tooling. Discord voice is 48 kHz stereo
// 16-bit PCM once Opus is decoded; we wrap it in a WAV container for
// portability and a well-known transcription format.
package audio

import (
	"encoding/binary"
	"io"
	"math"
)

// Discord voice constants.
const (
	SampleRate = 48000
	Channels   = 2
	FrameSize  = 960 // samples per channel per 20ms Opus frame at 48kHz
)

// WriteWAV writes a 16-bit PCM WAV file for the given interleaved samples.
func WriteWAV(w io.Writer, pcm []int16, sampleRate, channels int) error {
	dataBytes := len(pcm) * 2
	byteRate := sampleRate * channels * 2
	blockAlign := channels * 2

	// RIFF header
	if _, err := w.Write([]byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(36+dataBytes)); err != nil {
		return err
	}
	if _, err := w.Write([]byte("WAVEfmt ")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(16)); err != nil { // fmt chunk size
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(1)); err != nil { // PCM
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(channels)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(sampleRate)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(byteRate)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(blockAlign)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(16)); err != nil { // bits per sample
		return err
	}
	if _, err := w.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(dataBytes)); err != nil {
		return err
	}
	return binary.Write(w, binary.LittleEndian, pcm)
}

// TrimSilence drops near-silent 20ms frames from interleaved PCM to cut billed
// transcription duration. A frame is dropped when its RMS amplitude is below
// rmsThreshold (0..32767); non-silent audio is left bit-for-bit unchanged.
//
// samplesPerFrame is the int16 sample count per frame across all channels
// (e.g. FrameSize*Channels for Discord stereo). A trailing partial frame is
// preserved as-is.
func TrimSilence(pcm []int16, samplesPerFrame, rmsThreshold int) []int16 {
	if rmsThreshold <= 0 || samplesPerFrame <= 0 || len(pcm) == 0 {
		return pcm
	}
	out := make([]int16, 0, len(pcm))
	for start := 0; start < len(pcm); start += samplesPerFrame {
		end := start + samplesPerFrame
		if end > len(pcm) {
			end = len(pcm)
		}
		frame := pcm[start:end]
		if len(frame) < samplesPerFrame || frameRMS(frame) >= float64(rmsThreshold) {
			out = append(out, frame...)
		}
	}
	return out
}

// frameRMS returns the root-mean-square amplitude of a PCM frame.
func frameRMS(frame []int16) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sumSq float64
	for _, s := range frame {
		v := float64(s)
		sumSq += v * v
	}
	return math.Sqrt(sumSq / float64(len(frame)))
}
