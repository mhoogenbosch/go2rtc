package reolink

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/baichuan"
	"github.com/AlexxIT/go2rtc/pkg/core"
)

func TestUpsampler(t *testing.T) {
	var sum float64
	for _, h := range halfband {
		sum += h
	}
	if math.Abs(sum-1) > 0.01 {
		t.Fatalf("interpolated samples change the level: tap sum %v", sum)
	}

	for _, freq := range []float64{300, 1000, 3000} {
		src := make([]int16, 800)
		for i := range src {
			src[i] = int16(10000 * math.Sin(2*math.Pi*freq*float64(i)/8000))
		}
		dst := (&upsampler{}).process(src)
		if len(dst) != 1600 {
			t.Fatalf("output length %d, want 1600", len(dst))
		}

		var signal, noise float64
		for m := 100; m < len(dst); m++ {
			want := 10000 * math.Sin(2*math.Pi*freq*float64(m-16)/16000) // delay: 8 input samples
			signal += want * want
			noise += (want - float64(dst[m])) * (want - float64(dst[m]))
		}
		if snr := 10 * math.Log10(signal/noise); snr < 30 {
			t.Fatalf("%v Hz: SNR %.1f dB, want > 30", freq, snr)
		}
	}
}

// TestUpsamplerImage checks the image of 3.4 kHz at 4.6 kHz, which sample repetition leaves at -15 dB
func TestUpsamplerImage(t *testing.T) {
	src := make([]int16, 8000)
	for i := range src {
		src[i] = int16(10000 * math.Sin(2*math.Pi*3400*float64(i)/8000))
	}
	dst := (&upsampler{}).process(src)[100:]

	level := func(freq float64) float64 { // Goertzel
		k := 2 * math.Cos(2*math.Pi*freq/16000)
		var s1, s2 float64
		for _, v := range dst {
			s1, s2 = float64(v)+k*s1-s2, s1
		}
		return 10 * math.Log10(s1*s1+s2*s2-k*s1*s2)
	}
	if d := level(3400) - level(4600); d < 30 {
		t.Fatalf("image only %.1f dB down, want > 30", d)
	}
}

func TestBackchannelUpsamplesBrowserAudio(t *testing.T) {
	format := baichuan.TalkFormat{SampleRate: 16000, SamplePrecision: 16, SamplesPerBlock: 4}
	for _, test := range []struct {
		codec  string
		rate   uint32
		filter bool
	}{
		{core.CodecPCMA, 8000, true}, {core.CodecPCMU, 8000, true}, {core.CodecPCML, 8000, true},
		{core.CodecPCM, 8000, true}, {core.CodecPCML, 16000, false}, {core.CodecPCM, 16000, false},
	} {
		b := &backchannel{producer: &Producer{ctx: context.Background()}, format: format}
		if err := b.setCodec(&core.Codec{Name: test.codec, ClockRate: test.rate, Channels: 1}); err != nil {
			t.Fatal(err)
		}
		if got := b.upsample != nil; got != test.filter {
			t.Fatalf("%s/%d: filter %v, want %v", test.codec, test.rate, got, test.filter)
		}
	}

	// 8 kHz PCM through Write: every even output sample is the input, the odd ones interpolate
	var blocks [][]int16
	talk := &fakeTalk{format: format, writeFn: func(_ context.Context, block []byte) error {
		samples, err := baichuan.DecodeADPCMBlock(block)
		blocks = append(blocks, samples)
		return err
	}}
	b := &backchannel{producer: &Producer{ctx: context.Background()}, format: format, talk: talk}
	if err := b.setCodec(&core.Codec{Name: core.CodecPCML, ClockRate: 8000, Channels: 1}); err != nil {
		t.Fatal(err)
	}
	if err := b.Write(pcml(make([]int16, 64), 1000)); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 32 {
		t.Fatalf("blocks: %d, want 32 (64 samples at 8 kHz = 128 at 16 kHz)", len(blocks))
	}
}

func TestBackchannelSilenceGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	format := baichuan.TalkFormat{SampleRate: 16000, SamplePrecision: 16, SamplesPerBlock: 4}
	opens, blocks := 0, 0
	b := &backchannel{producer: &Producer{ctx: ctx, cancel: cancel}, format: format, gateSilence: true}
	b.start = func(context.Context) (talkSession, error) {
		opens++
		return &fakeTalk{format: format, writeFn: func(context.Context, []byte) error {
			blocks++
			return nil
		}}, nil
	}
	if err := b.setCodec(&core.Codec{Name: core.CodecPCMA, ClockRate: 8000, Channels: 1}); err != nil {
		t.Fatal(err)
	}
	quiet := make([]byte, 160)
	for i := range quiet {
		quiet[i] = 0xd5 // A-law digital silence, decodes to 8
	}
	loud := make([]byte, 160)
	for i := range loud {
		loud[i] = 0xaa // A-law near full scale
	}

	if err := b.Write(quiet); err != nil {
		t.Fatal(err)
	}
	if opens != 0 || blocks != 0 {
		t.Fatalf("silence opened a session: opens=%d blocks=%d", opens, blocks)
	}
	if err := b.Write(loud); err != nil {
		t.Fatal(err)
	}
	if opens != 1 || blocks != 80 {
		t.Fatalf("speech: opens=%d blocks=%d, want 1 and 80", opens, blocks)
	}
	if err := b.Write(quiet); err != nil { // a pause in speech is sent
		t.Fatal(err)
	}
	if blocks != 160 {
		t.Fatalf("pause in speech not sent: blocks=%d, want 160", blocks)
	}

	b.mu.Lock()
	b.lastSound = time.Now().Add(-talkIdleTimeout)
	idleAt := b.idleAt
	b.mu.Unlock()
	if err := b.Write(quiet); err != nil { // long silence no longer keeps the session busy
		t.Fatal(err)
	}
	b.mu.Lock()
	sameIdle := b.idleAt.Equal(idleAt)
	b.mu.Unlock()
	if blocks != 160 || !sameIdle {
		t.Fatalf("long silence was sent: blocks=%d, idle timer reset=%v", blocks, !sameIdle)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBackchannelPacing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// scaled down: blocks of 1 ms and at most 20 ms ahead
	format := baichuan.TalkFormat{SampleRate: 16000, SamplePrecision: 16, SamplesPerBlock: 16}
	b := &backchannel{
		producer: &Producer{ctx: ctx, cancel: cancel}, format: format, maxAhead: 20 * time.Millisecond,
		talk: &fakeTalk{format: format, writeFn: func(context.Context, []byte) error { return nil }},
	}
	if err := b.setCodec(&core.Codec{Name: core.CodecPCML, ClockRate: 16000, Channels: 1}); err != nil {
		t.Fatal(err)
	}
	// 120 ms of audio at once, as from a TTS file fetched faster than real time
	start := time.Now()
	if err := b.Write(pcml(make([]int16, 1920), 1000)); err != nil {
		t.Fatal(err)
	}
	// all but maxAhead and the last block must have been waited for: about 99 ms
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("paced %v, want about 99 ms", elapsed)
	}
	if ahead := time.Until(b.playEnd); ahead > 22*time.Millisecond {
		t.Fatalf("%v queued in the camera, want at most 21 ms", ahead)
	}
}

func TestBackchannelPacingStopsWithProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	format := baichuan.TalkFormat{SampleRate: 16000, SamplePrecision: 16, SamplesPerBlock: 1024}
	b := &backchannel{producer: &Producer{ctx: ctx, cancel: cancel}, format: format, maxAhead: time.Second}
	b.playEnd = time.Now().Add(time.Hour)
	cancel()
	done := make(chan error, 1)
	go func() { done <- b.pace(1024) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pace after stop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pace kept waiting after the producer stopped")
	}
}

// pcml fills samples with value and encodes them as little-endian PCM
func pcml(samples []int16, value int16) []byte {
	b := make([]byte, 2*len(samples))
	for i := range samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(value))
	}
	return b
}
