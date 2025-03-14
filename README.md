# videoencoder

Wrapper around ffmpeg for creating streaming videos with DASH and HLS.

This will be a tool for turning a single video file into a directory
full of streamable video content, including DASH and HLS manifests, to
allow streaming video over HTTP.



## Notes


- Render a thumbnail: `ffmpeg -i input.mp4 -ss 00:00:01.000 -vframes 1 thumbnail.jpg`
- Use `*_nvenv` where possible.
- Keep original frame rate.
- Render at an assortment of resolutions and bitrates up to the
  original resolution.
- Need a config file to select bitrate/resolution/codec preferences.
- There are ~3 useful codecs today: h.264, h.265, and av1.  Where
  possible, prefer av1, but many older clients can't play it back
  well.  Apple is the biggest issue; they support AV1 on A17 Pro/M3
  and up.  Older CPUs probably won't be able to play AV1.  H.265/HEVC
  has wider support, basically newer Windows and all current Apple
  hardware.  Everything recent supports H.264/AVC.  AV1 is
  substantailly better at low-to-moderate bit rates than either of the
  others, so a reasonable strategy is probably to provide 720p and
  1080p in H.264, maybe add 1440p in H.265, and then provide all
  useful resolutions in AV1.
- For nVidia's renderer, there are flags that will allow parallelism
  in encoding: `-hwaccel cuda -hwaccel_output_format cuda
  -split_encode_mode <int>`.  Setting the split mode to `0` should let
  ffmpeg automate the setting.  Other settings force enable or
  disable.  *Not clear that these aren't obsolete.*
- It's not clear what the best way to encode video on nVidia is -- do
  we render multiples at once, try to use parallelism up front, or what?

My current Makefile includes these lines:

```make
define encode_h264
        -s:$(1) $(2) -c:v:$(1) libx264 -pix_fmt:$(1) yuv420p -crf:$(1) 23 -b:v:$(1) $(3) -maxrate:$(1) $(4) -bufsize:$(1) $(4)
endef
#       -s:$(1) $(2) -c:v:$(1) h264_nvenc -rgb_mode:$(1) yuv420 -b:v:$(1) $(3) -maxrate:$(1) $(4) -bufsize:$(1) $(4)

define encode_h265
        -s:$(1) $(2) -c:v:$(1) hevc_nvenc -b:v:$(1) $(3) -maxrate:$(1) $(4) -bufsize:$(1) $(4)
endef
#       -s:$(1) $(2) -c:v:$(1) hevc -crf:$(1) 23 -b:v:$(1) $(3) -maxrate:$(1) $(4) -bufsize:$(1) $(4)

define encode_av1
        -s:$(1) $(2) -c:v:$(1) av1_nvenc -preset:$(1) p4 -tune:$(1) hq -b:v:$(1) $(3) -maxrate:$(1) $(4) -bufsize:$(1) $(4) -rc:$(1) vbr -cq:$(1) 19
endef

.SECONDEXPANSION:
web/%/dash.mpd: $$(notdir $$(@D)).mov
        mkdir -p `dirname $@`
        ffmpeg -i $< \
          -preset ${PRESET} \
          -keyint_min ${GOP_SIZE} -g ${GOP_SIZE} \
          -sc_threshold 0 \
          -r ${FPS} \
          -c:a aac -b:a 32k -ac 1 -ar 48000 \
          $(call encode_h264,0,1280x720,3M,6M) \
          $(call encode_h265,1,1280x720,0.95M,2M) \
          $(call encode_av1,2,1280x720,0.5M,1.0M,) \
          $(call encode_av1,3,1280x720,2M,4M) \
          $(call encode_h264,4,1920x1080,8M,16M) \
          $(call encode_h265,5,1920x1080,3M,6M) \
          $(call encode_av1,6,1920x1080,3.5M,5M) \
          $(call encode_h265,7,3840x2160,10M,20M) \
          $(call encode_av1,8,2560x1440,4M,6M) \
          $(call encode_av1,9,3840x2160,5M,10M) \
          -map v:0 -map v:0 -map v:0 -map v:0 -map v:0 -map v:0 \
          -map v:0 -map v:0 -map v:0 -map v:0 \
          -map a:0 \
          -single_file_name '$(basename $<)-stream$$RepresentationID$$.$$ext$$' \
          -adaptation_sets "id=0,streams=v id=1,streams=a" \
          -hls_playlist true \
          -f dash $@
```


### Timing measurements

*Note that `time` below is the video length, not the elapsed time.  Compare the `speed` numbers instead.*

```
$ $ ffmpeg -i ../../20250301-deception1.mov  -s 1920x1080 -c:v av1_nvenc -preset p4 -tune hq -b:v 1.5M -maxrate 2.5M -bufsize 5M -rc vbr -cq:v 19  test1080.mp4
frame=14411 fps=218 q=50.0 Lsize=   63444kB time=00:04:00.17 bitrate=2164.0kbits/s speed=3.63x

$ ffmpeg -i ../../20250301-deception1.mov  -s 1920x1080 -c:v av1_nvenc -preset p4 -tune hq -b:v 3.5M -maxrate 5M -bufsize 5M -rc vbr -cq:v 19 test1080.mp4
...
frame=14411 fps=212 q=29.0 Lsize=  144205kB time=00:04:00.17 bitrate=4918.7kbits/s speed=3.54x

$ ffmpeg -i ../../20250301-deception1.mov  -s 3840x2160 -c:v av1_nvenc -preset p4 -tune hq -b:v 3.5M -maxrate 5M -bufsize 5M -rc vbr -cq:v 19  test2160.mp4
frame=14411 fps=138 q=109.0 Lsize=   73816kB time=00:04:00.17 bitrate=2517.8kbits/s speed= 2.3x

$ ffmpeg -i ../../20250301-deception1.mov  -s 5376x3024 -c:v av1_nvenc -preset p4 -tune hq -b:v 3.5M -maxrate 5M -bufsize 5M -rc vbr -cq:v 19  test3024.mp4   # source resolution
frame=14411 fps= 70 q=152.0 Lsize=   75012kB time=00:04:00.17 bitrate=2558.6kbits/s speed=1.17x

# my Q is getting high there.  Looks... chunky.  Not horrid, but too low of a bitrate.

$ ffmpeg -i ../../20250301-deception1.mov  -s 5376x3024 -c:v av1_nvenc -preset p4 -tune hq -b:v 10M -maxrate 15M -bufsize 15M -rc vbr -cq:v 19  test3024_10M.mp4
frame=14411 fps= 70 q=95.0 Lsize=  237425kB time=00:04:00.17 bitrate=8098.3kbits/s speed=1.17x


$ ffmpeg -i ../../20250301-deception1.mov  -s 5376x3024 -c:v libsvtav1 -b:v 10M -maxrate 15M -bufsize 15M -crf 19  test3024_10Msvt.mp4
frame=14411 fps= 92 q=20.0 Lsize=  448378kB time=00:04:00.17 bitrate=15293.7kbits/s speed=1.54x

$ ffmpeg -i ../../20250301-deception1.mov  -s 5376x3024 -c:v libsvtav1 -b:v 10M -maxrate 15M -bufsize 15M -crf 35  test3024_10Msvt_crf35.mp4
frame=14411 fps=113 q=33.0 Lsize=  105312kB time=00:04:00.17 bitrate=3592.1kbits/s speed=1.89x


$ ffmpeg -i ../../20250301-deception1.mov  -s 3840x2160 -c:v libsvtav1 -b:v 10M -maxrate 15M -bufsize 15M -crf 35  test2160_10Msvt_crf35.mp4
frame=14411 fps=142 q=33.0 Lsize=   58455kB time=00:04:00.17 bitrate=1993.8kbits/s speed=2.37x
```

Looking at just 1440p; high enough resolution to look good, but small enough to make encoder settings interesting, maybe.

| Encoder   | Settings                                  |  Bitrate |  Q | Speed | Notes                            |
|-----------|-------------------------------------------|---------:|---:|------:|----------------------------------|
| SVT-AV1   | -maxrate 15M -crf 35                      |  1093.1k | 33 | 2.78x | Maybe slight blockiness          |
| SVT-AV1   | -maxrate 15M -crf 30                      |  1646.2k | 28 | 2.76x |                                  |
| SVT-AV1   | -maxrate 15M -crf 25                      |  2510.5k | 24 | 2.76x | LGTM                             |
| av1_nvenc | -maxrate 15M -cq:v 19 -preset p4 -tune hq | 15150.5k | 22 | 3.46x | so. many. bits.                  |
| av1_nvenc | -maxrate 15M -cq:v 30 -preset p4 -tune hq |  5236.4k | 42 |  3.5x | Probably fine, but loads of bits |
| av1_nvenc | -maxrate 4M -cq:v 30 -preset p4 -tune hq  |  2941.1k | 59 | 3.51x | Probably fine.                   |

Honestly, I'm having a hard time seeing a win for av1_nvenc here.  I'm
not sure that `Q` is comparable across codecs, but it's so much easier
to use and only slightly slower.

So, maybe just stick w/ the software codecs for now?  Do them one at a time, and then build a MPD from those?
