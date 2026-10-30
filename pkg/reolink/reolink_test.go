package reolink

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// talkAbility is the reply of an RLC-833A, RLC-811A, RLC-510WA and E1 Pro (all identical)
const talkAbility = `<?xml version="1.0" encoding="UTF-8" ?>
<body>
<TalkAbility version="1.1">
<duplexList>
<duplex>FDX</duplex>
</duplexList>
<audioStreamModeList>
<audioStreamMode>followVideoStream</audioStreamMode>
</audioStreamModeList>
<audioConfigList>
<audioConfig>
<priority>0</priority>
<audioType>adpcm</audioType>
<sampleRate>16000</sampleRate>
<samplePrecision>16</samplePrecision>
<lengthPerEncoder>1024</lengthPerEncoder>
<soundTrack>mono</soundTrack>
</audioConfig>
</audioConfigList>
</TalkAbility>
</body>
`

func TestTalkConfig(t *testing.T) {
	cfg, err := parseTalkAbility(talkAbility)
	require.NoError(t, err)
	require.Equal(t, talkParams{"FDX", "followVideoStream", "adpcm", "mono", 16000, 16, 1024}, cfg)

	// the camera refuses anything but its own formatting, these are the sizes it sends itself
	require.Len(t, channelExt(0), 104)
	require.Len(t, cfg.talkConfig(0), 394)
}

func TestUnsupportedAudio(t *testing.T) {
	_, err := parseTalkAbility(`<audioType>aac</audioType>`)
	require.Error(t, err)
}

// the streams API marshals every producer, a func field would break it
func TestMarshalJSON(t *testing.T) {
	_, err := json.Marshal(&Client{OnError: func(error) {}})
	require.NoError(t, err)
}

func TestMD5Modern(t *testing.T) {
	require.Equal(t, "900150983CD24FB0D6963F7D28E17F7", md5Modern("abc"))
}

func TestXOR(t *testing.T) {
	plain := []byte(xmlHeader + "<body/>")
	enc := xorBC(plain, hostChannel)
	require.NotEqual(t, plain, enc)
	require.Equal(t, plain, xorBC(enc, hostChannel))
}

func TestAES(t *testing.T) {
	c := &conn{key: []byte(md5Modern("nonce-pass")[:16])}
	msg := &message{body: c.encrypt([]byte(talkAbility))}
	require.Equal(t, talkAbility, c.decryptXML(msg))
}

func TestHeader(t *testing.T) {
	var buf bytes.Buffer
	c := &conn{Conn: &fakeConn{Writer: &buf}}
	require.NoError(t, c.write(cmdTalk, 0x01000101, classModern, []byte("ext"), []byte("body")))

	msg, err := readMessage(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Equal(t, uint32(cmdTalk), msg.cmd)
	require.Equal(t, uint32(0x01000101), msg.id)
	require.Equal(t, []byte("ext"), msg.ext)
	require.Equal(t, []byte("body"), msg.body)
}

type fakeConn struct {
	net.Conn
	Writer io.Writer
}

func (f *fakeConn) Write(b []byte) (int, error)      { return f.Writer.Write(b) }
func (f *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func TestFrame(t *testing.T) {
	enc := adpcmEncoder{}
	block := enc.encodeBlock(make([]int16, 1024))
	require.Len(t, block, 516)

	frame := appendFrame(nil, block)
	require.Len(t, frame, 12+516+4) // header, block, padding to 8 bytes
	require.Equal(t, adpcmMagic, frame[:4])
	require.Equal(t, uint16(520), binary.LittleEndian.Uint16(frame[4:]))
	require.Equal(t, uint16(520), binary.LittleEndian.Uint16(frame[6:]))
	require.Equal(t, []byte{0x00, 0x01}, frame[8:10])
	require.Equal(t, uint16(256), binary.LittleEndian.Uint16(frame[10:]))
}

// TestADPCM decodes the encoder output with a reference IMA decoder and checks the signal to noise ratio
func TestADPCM(t *testing.T) {
	const n = 1024
	src := make([]int16, 4*n)
	for i := range src {
		src[i] = int16(8000 * math.Sin(2*math.Pi*784*float64(i)/16000))
	}

	enc := adpcmEncoder{}
	var signal, noise float64
	for off := 0; off < len(src); off += n {
		block := enc.encodeBlock(src[off : off+n])
		out := decodeBlock(block)
		for i, v := range out {
			s := float64(src[off+i])
			signal += s * s
			noise += (s - float64(v)) * (s - float64(v))
		}
	}
	snr := 10 * math.Log10(signal/noise)
	require.Greater(t, snr, 20.0)
}

func decodeBlock(b []byte) []int16 {
	predictor := int(int16(binary.LittleEndian.Uint16(b)))
	index := int(b[2])
	var out []int16
	decode := func(code int) {
		step := imaStep[index]
		delta := step >> 3
		if code&4 != 0 {
			delta += step
		}
		if code&2 != 0 {
			delta += step >> 1
		}
		if code&1 != 0 {
			delta += step >> 2
		}
		if code&8 != 0 {
			predictor -= delta
		} else {
			predictor += delta
		}
		predictor = min(max(predictor, -32768), 32767)
		index = min(max(index+imaIndex[code], 0), 88)
		out = append(out, int16(predictor))
	}
	for _, v := range b[4:] {
		decode(int(v & 0x0f))
		decode(int(v >> 4))
	}
	return out
}
