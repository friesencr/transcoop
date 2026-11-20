package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
)

func TestSelectWorkerEncoders(t *testing.T) {
	var probed []string
	got, err := selectWorkerEncoders(context.Background(), "hb", []string{"x264", "x265_10bit", "nvenc_h265", "qsv_h265", "future_encoder"}, func(_ context.Context, executable, encoder string) error {
		if executable != "hb" {
			t.Fatal(executable)
		}
		probed = append(probed, encoder)
		if encoder == "qsv_h265" {
			return errors.New("unavailable")
		}
		return nil
	})
	if err != nil || !reflect.DeepEqual(got, []string{"x264", "x265_10bit", "nvenc_h265", "future_encoder"}) {
		t.Fatal(got, err)
	}
	if !reflect.DeepEqual(probed, []string{"nvenc_h265", "qsv_h265", "future_encoder"}) {
		t.Fatal(probed)
	}
}

func TestSelectWorkerEncodersFailureAndCancellation(t *testing.T) {
	fail := func(context.Context, string, string) error { return errors.New("unavailable") }
	if _, err := selectWorkerEncoders(context.Background(), "hb", []string{"nvenc_h265"}, fail); err == nil {
		t.Fatal("accepted no encoders")
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := selectWorkerEncoders(ctx, "hb", []string{"nvenc_h265", "x264"}, func(context.Context, string, string) error { cancel(); return ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRealEncoderProbe(t *testing.T) {
	if os.Getenv("TRANSCOOP_TEST_ENCODER_PROBE") != "1" {
		t.Skip("set TRANSCOOP_TEST_ENCODER_PROBE=1 for real HandBrake probe")
	}
	executable, err := exec.LookPath("HandBrakeCLI")
	if err != nil {
		t.Fatal(err)
	}
	if err := probeHardwareEncoder(context.Background(), executable, "x264"); err != nil {
		t.Fatal(err)
	}
	if err := probeHardwareEncoder(context.Background(), executable, "not_an_encoder"); err == nil {
		t.Fatal("accepted invalid encoder")
	}
}
