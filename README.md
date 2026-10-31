# videoencoder

Wrapper around ffmpeg for creating streaming videos with DASH and HLS.

This will be a tool for turning a single video file into a directory
full of streamable video content, including DASH and HLS manifests, to
allow streaming video over HTTP.

Requirements:

- ffmpeg + ffprobe
- [Shaka Packager](https://github.com/shaka-project/shaka-packager), installed in the path as `packager`.
- For gain-mapped AVIF posters of HDR videos, `avifgainmaputil` from
  [libavif](https://github.com/AOMediaCodec/libavif) (`brew install
  libavif`).  Gain-mapped JPEGs need nothing extra.

## Usage

- Install via `go install ./cmd/videoencoder` (or `go build ./cmd/videoencoder`).
- Make sure that `ffmpeg`, `ffprobe`, and `packager` are installed.
- Run `videoencoder file.mov` to encode one file, or describe your
  videos in config files (below) and run `videoencoder` in their
  directory.

`videoencoder` will recognize when source files, destination files,
or transcoding settings change, and will rebuild/re-transcode as few
files as possible.  Each output's name includes a hash of its source
and settings (`av1-2160.hdr.7c3e1a09bd.mp4`), so changing one
rendition's settings produces one new file and leaves every other
file, and its name, alone.  Outputs are written under a `.partial`
name and renamed when ffmpeg finishes, so an interrupted run never
leaves a truncated file that looks complete.  Replaced and removed
outputs are deleted once the new stream is complete.  `--rebuild GLOB`
forces a rebuild (`'*'`, `'av1-*'`, `sdr/h264-1080`, `poster`), for
example after upgrading ffmpeg.

By default, `videoencoder` will transcode to H.264 up to 1080p, H.265
up to 4k, and AV1 up to 8k, never above the source's resolution.
Intermediate renditions go in `transcodes/<name>/` and the stream in
`stream/<name>/`, next to the video.

Each stream also gets posters, still images taken at `poster.offset`
in several widths and formats, plus gain-mapped JPEG and AVIF posters
when there's an HDR source, listed in `poster.json`.  The best one of
each size is always at `poster-<width>.<format>` (gain-mapped if
possible, SDR otherwise), and the full-resolution one at
`poster.<format>`.  There are also seek
thumbnails, tiled JPEGs added to `dash.mpd` and `hls.m3u8` and listed
in a `thumbnails.vtt` WebVTT track (turn
them off with `thumbnails: {enabled: false}` or `--thumbnails=false`).

### Tuning quality

`videoencoder tune` picks each rendition's quality setting for you.  It
encodes a few seconds of the video (`tune.start`, `tune.duration`) at
the shortest and tallest rendition of each encoder, searching for the
setting that just reaches `tune.target_vmaf` (95 by default; a
rendition's own `target_vmaf` overrides it), interpolates the
renditions in between, and writes `quality` (and, for encoders with
bitrate caps, `bitrate`) into the video's sidecar with a comment saying
where each value came from.  Review the sidecar, then render as usual.

Renditions up to 1080p are judged at 1080p and taller ones at 2160p
with VMAF's 4K model.  HDR-only videos are tone-mapped to SDR for
scoring; with an SDR source as well, that's used instead.

### Publishing

With `publish.to` set, each render ends by copying the stream to a
local directory or an S3-compatible bucket (SeaweedFS, MinIO, AWS).
Only files that are new or changed are uploaded, and manifests go
last.  `videoencoder publish` publishes without rendering, `--to`
picks targets, and `--prune` deletes objects that are no longer part
of the stream.  S3 credentials come from the usual `AWS_*` variables
or `~/.aws/credentials`, and the endpoint from `s3.endpoint` or
`$S3_ENDPOINT_URL`.

```yaml
publish:
  to: [seaweed]
  targets:
    seaweed: {s3: {bucket: video, prefix: "{name}/"}}
```

### Configuration

Settings are YAML, merged from several layers, lowest priority first:

1. the built-in defaults ([`internal/config/default.yaml`](internal/config/default.yaml)),
2. `~/.config/videoencoder/config.yaml` (or `--config`),
3. every `videoencoder.yaml` from `/` down to the video's directory,
4. the video's entry under `videos:` in its directory's `videoencoder.yaml`,
5. the video's sidecar, `<name>.video.yaml`,
6. `--outdir`, `--streamdir`, `--thumbnails` and `--to` on the command line.

Maps merge key by key, other values replace, and `null` deletes an
inherited key.  Renditions and encoders are maps keyed by name, so a
layer can change or drop one rendition:

```yaml
# trip/videoencoder.yaml: applies to every video in trip/ and below
renditions:
  av1-4320: null              # no 8K for this project
videos:
  hike:
    sources: {hdr: hike-hdr.mov, sdr: hike-sdr.mov}

# trip/hike.video.yaml: just this video
renditions:
  av1-2160: {quality: 30}     # only this rendition re-encodes
```

`videoencoder` (or `videoencoder dir`) renders every video defined
under a directory, and `videoencoder plan` prints each video's merged
config, annotated with the file each value came from, plus which
renditions would be rebuilt and why.  Relative paths are relative to
the video's directory, and `{name}` in `outdir`/`streamdir` is the
video's name.

`videoencoder` can handle both SDR and HDR videos with no additional
flags.  It can also combine SDR and HDR versions of the same source
into a single SDR+HDR stream, where the client can choose which flavor
to stream.  To do this, run `videoencoder hdr.mov sdr.mov`, or list
both under `sources:`.  Sources are encoded in label order, and the
first supplies the audio and poster.  The two
videos should be the same length and ideally the same resolution.
They will be individually transcoded and then bundled into the same
DASH and HLS manifests for streaming.

## encbench

`encbench` finds, for each candidate set of encoder flags, the quality
setting (CRF or equivalent) that just reaches a target VMAF on a set of
sample clips, and records the size, encode time and quality of each
answer.  It searches rather than sweeping, typically 3–8 encodes per
flagset and sample, and logs every encode in `results.csv`, so adding
a flagset or a sample only runs the new combinations.

```yaml
# bench.yaml
samples: samples        # directory of ~30s clips
workdir: work
scales: [1080]          # test at 1080p; taller samples are downscaled once
target_vmaf: 95
max_color_shift: 2.7    # fail local color casts, which VMAF can't see
max_color_flicker: 1.25 # and casts that come and go
references:             # score a sample against another file in samples/:
  yuri-h265.mov: yuri.mov   # an H.265 export against its ProRes master
flagsets:
  x265-slow:
    args: [-c:v, libx265, -preset, slow]
  svtav1-p5:
    args: [-c:v, libsvtav1, -preset, "5"]
  x264-medium:
    args: [-c:v, libx264, -preset, medium, -pix_fmt, yuv420p]
    sdr_only: true      # skip HDR samples
```

```
go install ./cmd/encbench
encbench run            # -n for a dry run; -sample/-flagset globs to limit
encbench report         # Markdown tables of the answers
encbench prune          # delete kept encodes of removed samples/flagsets
```

The best encode for each sample and flagset is kept under
`work/keep/` for comparing by eye.  HDR samples are tone-mapped to SDR
on both sides for scoring, since VMAF is an SDR metric.  See `DESIGN.md` for details.

## TODO

- Optimize the default transcoding settings.

## Notes


- Render a thumbnail: `ffmpeg -i input.mp4 -ss 00:00:01.000 -vframes 1 thumbnail.jpg`
- Use `*_nvenv` where possible.  **Or not, it's terrible for H.265**
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
