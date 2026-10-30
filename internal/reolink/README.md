# Reolink

This source type supports only two-way audio (talk) for [Reolink](https://reolink.com/) cameras, over Reolink's own
Baichuan protocol (TCP port 9000), which the Reolink app uses as well. Many Reolink models have a speaker but no
ONVIF backchannel on their RTSP server, so the RTSP source cannot talk to them. Use it as a second source next to
the video stream.

## Configuration

```yaml
streams:
  reolink_camera:
    - rtsp://admin:password@192.168.1.123/Preview_01_main
    - reolink://admin:password@192.168.1.123               # two-way audio
    - reolink://admin:password@192.168.1.10?channel=2      # camera behind a Reolink NVR or Home Hub (untested)
```

- The default port is 9000, add `:port` when the camera uses another one.
- The camera tells which audio it accepts (all tested models: IMA ADPCM, 16 kHz, mono). go2rtc offers L16 16 kHz,
  PCMA and PCMU to the browser or to FFmpeg and converts that to ADPCM.
- The talk session opens with the first audio and closes after 2 seconds of silence, so the Reolink app can talk again.
- A camera without a speaker still reports that it can talk. Only add this source to cameras that have one.

## Tested models

- RLC-833A (firmware v3.1.0.3016)
- RLC-811A (firmware v3.1.0.1643), the talk session opens, sound not verified yet
