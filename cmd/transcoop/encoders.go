package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Synthetic one-second 720p H.264 clip, generated with ffmpeg's color source.
// Embedding it avoids requiring ffmpeg on worker machines.
//
//go:embed testdata/encoder-probe.mp4
var encoderProbeClip []byte

func softwareEncoder(name string) bool {
	switch strings.ToLower(name) {
	case "x264", "x264_10bit", "x265", "x265_10bit", "x265_12bit",
		"svt_av1", "svt_av1_10bit", "mpeg2", "mpeg4", "vp8", "vp9", "vp9_10bit",
		"theora", "ffv1", "dnxhr", "dnxhr_10bit", "ff_prores":
		return true
	}
	// Unknown encoders must pass a probe too, rather than assuming compatibility.
	return false
}

func selectWorkerEncoders(ctx context.Context, executable string, available []string, probe func(context.Context, string, string) error) ([]string, error) {
	var accepted []string
	for _, encoder := range available {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !softwareEncoder(encoder) {
			fmt.Printf("Checking %s with a short encode…\n", encoder)
			err := probe(ctx, executable, encoder)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				fmt.Printf("  Skipped %s: %v\n", encoder, err)
				continue
			}
			fmt.Printf("  %s passed\n", encoder)
		}
		accepted = append(accepted, encoder)
	}
	if len(accepted) == 0 {
		return nil, errors.New("no usable HandBrake video encoders found")
	}
	return accepted, nil
}

func probeHardwareEncoder(ctx context.Context, executable, encoder string) error {
	dir, err := os.MkdirTemp("", "transcoop-encoder-probe-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	source := filepath.Join(dir, "source.mp4")
	output := filepath.Join(dir, "output.mkv")
	if err := os.WriteFile(source, encoderProbeClip, 0600); err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	log, err := exec.CommandContext(probeCtx, executable,
		"--input", source, "--output", output, "--encoder", encoder,
		"--format", "av_mkv", "--audio", "none", "--min-duration", "0",
		"--width", "1280", "--height", "720", "--crop", "0:0:0:0",
		"--previews", "1:0").CombinedOutput()
	if probeCtx.Err() != nil {
		return fmt.Errorf("encode check: %w", probeCtx.Err())
	}
	if err != nil {
		return fmt.Errorf("encode check failed: %w (check GPU support and drivers)", err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Size() == 0 || !strings.Contains(string(log), "Encode done!") {
		return errors.New("HandBrake did not complete the encode (check GPU support and drivers)")
	}
	return nil
}
