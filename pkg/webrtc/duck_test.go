package webrtc

import (
	"math"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
)

func g711Tone(enc func(int16) byte, amp float64, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = enc(int16(amp * math.Sin(2*math.Pi*440*float64(i)/8000)))
	}
	return b
}

func TestTalkActiveG711(t *testing.T) {
	pcma := &core.Codec{Name: core.CodecPCMA}
	pcmu := &core.Codec{Name: core.CodecPCMU}

	if talkActive(pcma, g711Tone(pcm.PCMtoPCMA, 0, 160)) {
		t.Error("PCMA silence counted as speech")
	}
	if talkActive(pcma, g711Tone(pcm.PCMtoPCMA, 100, 160)) { // ~ -50 dBFS room noise
		t.Error("quiet PCMA noise counted as speech")
	}
	if !talkActive(pcma, g711Tone(pcm.PCMtoPCMA, 4000, 160)) { // ~ -18 dBFS speech
		t.Error("PCMA speech not detected")
	}
	if !talkActive(pcmu, g711Tone(pcm.PCMtoPCMU, 4000, 160)) {
		t.Error("PCMU speech not detected")
	}
	if talkActive(&core.Codec{Name: core.CodecOpus}, make([]byte, 80)) {
		t.Error("opus must not be inspected")
	}
	if talkActive(pcma, nil) {
		t.Error("empty payload counted as speech")
	}
}

func TestDuckAudioMutesOnlyWhileTalking(t *testing.T) {
	old := TalkDuck
	TalkDuck = 50 * time.Millisecond
	defer func() { TalkDuck = old }()

	c := &Conn{}
	var passed int
	h := c.duckAudio(func(*rtp.Packet) { passed++ })

	h(&rtp.Packet{}) // nobody talking: camera audio passes
	if passed != 1 {
		t.Fatalf("audio dropped while nobody talks (passed=%d)", passed)
	}

	pcma := &core.Codec{Name: core.CodecPCMA}
	c.noteTalk(pcma, &rtp.Packet{Payload: g711Tone(pcm.PCMtoPCMA, 4000, 160)})
	h(&rtp.Packet{})
	if passed != 1 {
		t.Fatal("camera audio not muted while talking")
	}

	c.noteTalk(pcma, &rtp.Packet{Payload: g711Tone(pcm.PCMtoPCMA, 0, 160)}) // silence does not extend
	time.Sleep(70 * time.Millisecond)
	h(&rtp.Packet{})
	if passed != 2 {
		t.Fatal("camera audio still muted after the hold")
	}
}
