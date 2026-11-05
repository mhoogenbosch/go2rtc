package webrtc

import (
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
)

// TalkDuck mutes the camera audio sent to a browser while that same browser is
// talking through the camera speaker, and for this long after the last word.
// Cameras without echo suppression (e.g. Dahua/Imou over dahua://) pick up their
// own speaker, so the talker otherwise hears himself back with a delay; the
// hold has to outlast that round trip. Only the talking connection is muted:
// recordings and other viewers keep the full audio. 0 (the default) turns it
// off. Set from the `webrtc: talk_duck:` config.
var TalkDuck time.Duration

// talkLevel is the RMS, on the 16-bit scale, above which a microphone packet
// counts as speech (~ -40 dBFS). Browsers run noise suppression and AGC on the
// microphone, so a quiet room stays well below it.
const talkLevel = 300

// talkActive reports whether a G.711 microphone payload carries speech. Other
// codecs are not inspected (returns false), so ducking only applies to
// G.711 backchannels.
func talkActive(codec *core.Codec, payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	var decode func(byte) int16
	switch codec.Name {
	case core.CodecPCMA:
		decode = pcm.PCMAtoPCM
	case core.CodecPCMU:
		decode = pcm.PCMUtoPCM
	default:
		return false
	}
	var sum int64
	for _, b := range payload {
		s := int64(decode(b))
		sum += s * s
	}
	return sum > int64(talkLevel)*int64(talkLevel)*int64(len(payload))
}

// noteTalk is called for every microphone packet of this connection.
func (c *Conn) noteTalk(codec *core.Codec, packet *rtp.Packet) {
	if talkActive(codec, packet.Payload) {
		c.talkUntil.Store(time.Now().Add(TalkDuck).UnixNano())
	}
}

// duckAudio wraps the handler that sends camera audio to this browser and drops
// packets while this connection is talking.
func (c *Conn) duckAudio(handler core.HandlerFunc) core.HandlerFunc {
	return func(packet *rtp.Packet) {
		if time.Now().UnixNano() < c.talkUntil.Load() {
			return
		}
		handler(packet)
	}
}
