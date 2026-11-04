package reolink

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/baichuan"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
)

const (
	talkIdleTimeout = 5 * time.Second
	// Bound each packet independently of the camera's encoder block size.
	maxTalkPacketSamples = 4 * 8192
	// talkMaxAhead limits how much audio may wait in the camera. A source faster than real time,
	// such as a TTS file fetched over HTTP, would otherwise overflow the camera's buffer.
	talkMaxAhead = time.Second
	// talkSilenceLevel is the largest sample a packet may hold to count as silence. A muted browser
	// microphone sends digital silence, PCMA 0xd5 decodes to 8.
	talkSilenceLevel = 16
)

type talkSession interface {
	Format() baichuan.TalkFormat
	WriteBlock(context.Context, []byte) error
	Close(context.Context) error
}

type talkStarter func(context.Context) (talkSession, error)

type backchannel struct {
	producer *Producer
	format   baichuan.TalkFormat
	start    talkStarter
	// maxAhead > 0 paces writes to real time (see talkMaxAhead); the producer sets it
	maxAhead time.Duration
	// gateSilence keeps silence from opening a talk session, or from keeping one open
	gateSilence bool

	mu        sync.Mutex
	closed    bool
	talk      talkSession
	encoder   baichuan.ADPCMEncoder
	codec     core.Codec
	transcode func([]byte) []byte
	upsample  *upsampler
	samples   []int16
	idle      *time.Timer
	idleAt    time.Time
	sender    *core.Sender
	playEnd   time.Time
	lastSound time.Time
}

func (p *Producer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	if p.talk == nil || media.Kind != core.KindAudio || media.Direction != core.DirectionSendonly ||
		codec.Channels > 1 || !talkCodecSupported(codec) {
		return fmt.Errorf("reolink: unsupported talkback track")
	}

	p.talkMu.Lock()
	defer p.talkMu.Unlock()
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	if p.talk.sender != nil {
		p.talk.sender.Close()
		p.talk.sender.Wait()
	}
	if err := p.talk.setCodec(codec); err != nil {
		return err
	}
	sender := core.NewSender(media, codec)
	sender.Handler = func(packet *rtp.Packet) {
		if err := p.talk.Write(packet.Payload); err != nil && p.ctx.Err() == nil {
			p.fail(fmt.Errorf("reolink: write talkback: %w", err))
		}
	}
	sender.HandleRTP(track)
	p.talk.sender = sender
	p.Senders = []*core.Sender{sender}
	return nil
}

func talkCodecs() []*core.Codec {
	codecs := []*core.Codec{
		{Name: core.CodecPCMA, ClockRate: 8000, Channels: 1, PayloadType: 8},
		{Name: core.CodecPCMU, ClockRate: 8000, Channels: 1, PayloadType: 0},
	}
	for _, codec := range pcm.ProducerCodecs() {
		if codec.Name != core.CodecPCM && codec.Name != core.CodecPCML ||
			codec.ClockRate != 8000 && codec.ClockRate != 16000 {
			continue
		}
		codec.Channels = 1
		codec.PayloadType = core.PayloadTypeRAW
		codecs = append(codecs, codec)
	}
	return codecs
}

func talkCodecSupported(codec *core.Codec) bool {
	if codec.ClockRate == 0 {
		return false
	}
	for _, candidate := range talkCodecs() {
		if candidate.Name == codec.Name && candidate.ClockRate == codec.ClockRate {
			return true
		}
	}
	return false
}

func (b *backchannel) setCodec(codec *core.Codec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("reolink: talkback closed")
	}
	b.codec = *codec
	target := &core.Codec{Name: core.CodecPCML, ClockRate: b.format.SampleRate, Channels: 1}
	b.upsample = nil
	if b.codec.ClockRate*2 == b.format.SampleRate {
		// 8 kHz from a browser: pcm.Transcode would repeat every sample, which sounds harsh,
		// so decode at the source rate and interpolate with a halfband filter instead
		target.ClockRate = b.codec.ClockRate
		b.upsample = &upsampler{}
	}
	b.transcode = pcm.Transcode(target, &b.codec)
	b.samples = b.samples[:0]
	return nil
}

func (b *backchannel) Write(payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("reolink: talkback closed")
	}
	inputSamples := uint64(len(payload))
	if b.codec.Name == core.CodecPCM || b.codec.Name == core.CodecPCML {
		if len(payload)&1 != 0 {
			return fmt.Errorf("reolink: talkback PCM payload has odd length %d", len(payload))
		}
		inputSamples /= 2
	}
	if inputSamples > maxTalkPacketSamples*uint64(b.codec.ClockRate)/uint64(b.format.SampleRate) {
		return fmt.Errorf("reolink: talkback packet exceeds sample limit")
	}
	pcmBytes := b.transcode(payload)
	if len(pcmBytes)&1 != 0 {
		return fmt.Errorf("reolink: talkback produced odd PCM length %d", len(pcmBytes))
	}
	samples := make([]int16, len(pcmBytes)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcmBytes[2*i:]))
	}
	if b.upsample != nil {
		samples = b.upsample.process(samples)
	}
	if b.gateSilence {
		now := time.Now()
		quiet := silent(samples)
		if !quiet {
			b.lastSound = now
		}
		// Silence opens no session, and after talkIdleTimeout of silence it no longer keeps one
		// open, so a muted browser microphone leaves the camera free for the Reolink app.
		// Pauses in speech shorter than that are still sent.
		if quiet && (b.talk == nil || now.Sub(b.lastSound) >= talkIdleTimeout) {
			return nil
		}
	}
	if err := b.open(); err != nil {
		return err
	}
	blockSize := int(b.format.SamplesPerBlock)
	if cap(b.samples) < blockSize {
		b.samples = make([]int16, 0, blockSize)
	}
	for _, sample := range samples {
		b.samples = append(b.samples, sample)
		if len(b.samples) < blockSize {
			continue
		}
		block, err := b.encoder.EncodeBlock(b.samples[:blockSize])
		b.samples = b.samples[:0]
		if err != nil {
			return err
		}
		if err = b.pace(blockSize); err != nil {
			return err
		}
		if err = b.talk.WriteBlock(b.producer.ctx, block); err != nil {
			return err
		}
		b.producer.addSend(len(block))
	}
	b.idleAt = time.Now().Add(talkIdleTimeout)
	if b.idle == nil {
		b.idle = time.AfterFunc(talkIdleTimeout, b.closeIdle)
	} else {
		b.idle.Reset(talkIdleTimeout)
	}
	return nil
}

func (b *backchannel) open() error {
	if b.talk != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(b.producer.ctx, baichuan.DefaultTimeout)
	start := b.start
	if start == nil {
		start = b.producer.profile.startTalk
	}
	talk, err := start(ctx)
	if err == nil {
		if format := talk.Format(); format != b.format {
			err = fmt.Errorf("reolink: talkback format changed from %+v to %+v", b.format, format)
		} else {
			b.talk = talk
			b.encoder = baichuan.ADPCMEncoder{}
			b.playEnd = time.Time{}
		}
	}
	cancel()
	if err != nil {
		var cleanup error
		if talk != nil {
			closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(b.producer.ctx), baichuan.DefaultTimeout)
			cleanup = errors.Join(cleanup, talk.Close(closeCtx))
			closeCancel()
		}
		return fmt.Errorf("reolink: start talkback: %w", errors.Join(err, cleanup))
	}
	return nil
}

// pace waits while more than maxAhead of audio is queued in the camera, then books one block.
// It runs under b.mu; the wait ends early when the producer stops.
func (b *backchannel) pace(samples int) error {
	if b.maxAhead <= 0 {
		return nil
	}
	now := time.Now()
	if b.playEnd.Before(now) {
		b.playEnd = now
	}
	if ahead := b.playEnd.Sub(now); ahead > b.maxAhead {
		if err := b.wait(ahead - b.maxAhead); err != nil {
			return err
		}
	}
	b.playEnd = b.playEnd.Add(time.Duration(samples) * time.Second / time.Duration(b.format.SampleRate))
	return nil
}

func (b *backchannel) wait(d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-b.producer.ctx.Done():
		return b.producer.ctx.Err()
	}
}

func silent(samples []int16) bool {
	for _, s := range samples {
		if s > talkSilenceLevel || s < -talkSilenceLevel {
			return false
		}
	}
	return true
}

func (b *backchannel) closeIdle() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	if wait := time.Until(b.idleAt); wait > 0 {
		b.idle.Reset(wait)
		b.mu.Unlock()
		return
	}
	err := b.closeSession()
	b.mu.Unlock()
	if err != nil && b.producer.ctx.Err() == nil {
		b.producer.fail(fmt.Errorf("reolink: stop idle talkback: %w", err))
	}
}

func (b *backchannel) closeSession() error {
	if b.talk == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(b.producer.ctx), baichuan.DefaultTimeout)
	err := b.talk.Close(ctx)
	cancel()
	b.talk = nil
	b.samples = b.samples[:0]
	return err
}

func (b *backchannel) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	if b.idle != nil {
		b.idle.Stop()
	}
	b.mu.Unlock()

	if b.sender != nil {
		b.sender.Close()
		b.sender.Wait()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeSession()
}
