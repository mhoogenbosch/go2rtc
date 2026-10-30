package reolink

import "math"

// halfbandTaps is the length of the interpolation filter, which delays the audio by 1 ms at 8 kHz
const halfbandTaps = 31

// halfband holds the odd taps of a windowed-sinc halfband filter with gain 2; the even taps are zero
// except the centre one, which is 1, so every input sample passes unchanged
var halfband = func() (h [halfbandTaps/2 + 1]float64) {
	const n = halfbandTaps
	for i := range h {
		k := 2*i - n/2 // odd offsets -15, -13 ... 15
		x := float64(k) / 2
		window := 0.42 + 0.5*math.Cos(math.Pi*float64(k)/float64(n/2+1)) + 0.08*math.Cos(2*math.Pi*float64(k)/float64(n/2+1))
		h[i] = math.Sin(math.Pi*x) / (math.Pi * x) * window
	}
	return
}()

// upsampler doubles the sample rate. Repeating every sample (pcm.Upsample) leaves images of the
// signal above 4 kHz when 8 kHz audio from a browser goes to a 16 kHz camera, which sounds harsh.
type upsampler struct {
	history [len(halfband)]float64
}

func (u *upsampler) process(src []int16) []int16 {
	const delay = len(halfband) / 2 // centre of the history

	dst := make([]int16, 0, 2*len(src))
	for _, sample := range src {
		copy(u.history[:], u.history[1:])
		u.history[len(u.history)-1] = float64(sample)

		var odd float64
		for i, h := range halfband {
			odd += h * u.history[len(u.history)-1-i]
		}
		dst = append(dst, int16(u.history[delay-1]), clamp16(odd))
	}
	return dst
}

func clamp16(v float64) int16 {
	return int16(min(max(math.Round(v), -32768), 32767))
}
