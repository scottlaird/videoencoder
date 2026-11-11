# videoencoder

`videoencoder` helps re-encode video files for streaming over HTTP.  It
reads one or two source video files, optionally analyzes them for
optimal compressibility, and then generates a set of streamable `.mp4`
files at various resolutions with various codecs, along with DASH and
HLS manifests so common web-video players can stream directly over
HTTP with no special server support.

When encoding, `videoencoder` produces a mix of H.264, H.265, and AV1
encodings at common resolutions from 480p up to the original source
resolution.  By default, H.264 is only encoded up to 1080p, H.265 is
encoded up to 4K, and AV1 is encoded up to 8K; these can be overridden
via config file.

Features:

- Support for combined DASH and HLS output, so nearly any modern
  web-based video player can play encoded videos.
- Support for up to 8K video output.
- Incremental rebuild support.  `videoencoder` will only
  rebuild/recompress files that have changed.  For example, if you
  change the compression settings for H.265 files, then only the H.265
  outputs will be regenerated, saving encoding time.
- Mixed HDR/SDR support.  `videoencoder` can take distinct input files
  with an SDR and an HDR encoding of the same video and produce a
  DASH/HLS output that players can use to play either based on the
  viewer's hardware capabilities.
- Automatic video quality tuning.  `videoencoder tune` will analyze
  snippets of videos and evaluate how well each codec compresses each
  video at different resolutions.  It will then tune the encoders'
  quality slider to meet a specified VMAF quality level.  This lets
  you produce low-bitrate videos with consistent quality with less
  manual work.
- Thumbnail support.  `videoencoder` will automatically generate
  seekable thumbnail images for the output streams, giving a better UX
  in players that understand DASH or HLS thumbnails.
- Poster image support.  `videoencoder` will produce JPG, AVIF, and
  WebP images showing a specified frame of the video, for use as
  posters or thumbnails in index pages.  The poster images are
  produced at multiple resolutions, optionally with HDR gain maps, and
  JSON metadata is provided for HTML image `srcset` support.
- S3 upload.  `videoencoder publish` can optionally push streaming
  output files directly into a specified S3 (or S3-compatible) bucket,
  in addition to writing to local directories.
- Support for hierarchical configuration.  You can create a directory
  tree full of video files with a top-level configuration file plus
  per-video override configs, and then `videoencoder` can render new
  videos and re-render existing videos as needed.
- A benchmark tool, `encbench`, that can be used to benchmark encoder
  settings, measuring encode time, output quality, and file size
  across multiple input videos automatically.  This uses the same
  basic logic as `videoencoder tune`.

## Why?

I've had *terrible* luck getting YouTube, Facebook, Reddit, etc, to
actually play back my video content the way that I want it to look.
YouTube is really inconsistent with HDR support and over-compresses a
lot of videos.  Facebook noise-reduces videos so heavily that pictures
of the night sky can lose their stars, and then it compresses
everything down to very low bitrates, leaving everything blurry.
Slack can stream videos, even in HDR, but it clamps maximum
brightness so low that it breaks HDR.  They all make their own
decisions around video compression that don't really fit what I want.
Sometimes that's fine, and sometimes I'm *really* unhappy with the
results.

It's not actually very difficult to stream video yourself these days.
There are many open-source tools that will handle the difficult client
side of things, but actually turning a high-quality source video into
something that web players can handle is sort of a black art.
`videoencoder` aims to simplify that as much as possible.

See [this blog post from
2025](https://scottstuff.net/posts/2025/03/18/video-encoding-101/) for
a walkthrough.

## Requirements

- Go 1.26 or later, to build.
- `ffmpeg` and `ffprobe`. You need a build with libx264, libx265 and
  libsvtav1 for the default encoders, and libvmaf for
  `videoencoder tune` and encbench.
- [Shaka Packager](https://github.com/shaka-project/shaka-packager),
  installed in your path as `packager`.
- Optionally, `avifgainmaputil` from
  [libavif](https://github.com/AOMediaCodec/libavif)
  (`brew install libavif`), for gain-mapped AVIF posters of HDR
  videos. Gain-mapped JPEGs need nothing extra.

## Installation

```
go install github.com/scottlaird/videoencoder/cmd/videoencoder@latest
go install github.com/scottlaird/videoencoder/cmd/encbench@latest
```

Or, from a checkout, `go install ./cmd/...`.

## Quick start

To encode one file with the built-in defaults:

```
videoencoder clip.mov
```

This writes renditions to `transcodes/clip/` and the stream to
`stream/clip/`, both next to `clip.mov`. To keep the settings with the
video, put them in a sidecar file named `clip.video.yaml`:

```yaml
# clip.video.yaml
sources:
  sdr: clip.mov
```

Then `videoencoder clip.video.yaml`, or just `videoencoder` in that
directory, renders it. Usually you'll want to tune the quality
settings for the video first, as described in
[Tuning quality](#tuning-quality).

## Output

For each video, videoencoder makes:

- **Renditions.** One per rung of the resolution ladder, for each
  source. The default ladder is H.264 up to 1080p, H.265 up to 4K
  and AV1 up to 8K, but never above the source's resolution.
- **Audio.** Encoded once from the first source.
- **Manifests.** `dash.mpd` and `hls.m3u8`, which list every rendition.
  With both an SDR and an HDR source, one stream carries both, and the
  player picks whichever the display supports.
- **Posters.** Still frames taken at `poster.offset`, in several
  widths and formats, listed in `poster.json`.
  - When there's an HDR source, there are also gain-mapped JPEG and
    AVIF posters, which HDR displays show brighter.
  - The best poster at each width is also copied to a fixed name,
    `poster-<width>.<format>`, and the full-resolution one to
    `poster.<format>`.
- **Seek thumbnails.** Tiled JPEGs, listed in a `thumbnails.vtt` track
  and added to both manifests.

Media files are named after what they are and a hash of their source
and settings, such as `av1-2160.hdr.7c3e1a09bd.mp4`. Changing one
rendition's settings produces one new file and leaves every other
file, and its name, alone. This makes CDN caching easier.

## Commands

```
videoencoder [flags] [dir | name.video.yaml | media files ...]
videoencoder plan    [flags] [same]
videoencoder tune    [flags] [same]
videoencoder publish [flags] [same]
videoencoder help    [command]
```

- **`videoencoder`** renders. Given a directory, which defaults to the
  current one, it renders every video defined beneath it. Given
  sidecars, it renders those videos. Given media files, it makes a
  one-off video from them, named after the first file.
- **`videoencoder plan`** renders nothing. It prints each video's
  merged configuration, with a comment on each value saying which file
  it came from. It then lists every output and whether it would be
  rebuilt and why: new, source changed, settings changed, or output
  missing.
- **`videoencoder tune`** picks quality settings for each video and
  writes them into its sidecar. See [Tuning quality](#tuning-quality).
- **`videoencoder publish`** copies finished streams to their publish
  targets without rendering.
- **`videoencoder help`** lists the commands, and `videoencoder help
  tune` (or `videoencoder tune --help`) shows one command's flags.

While it works, everything but `plan` shows its progress. In a
terminal, the screen splits into the videos (finished ones with their
times, the current one's elapsed time, and those to come), the tasks of
the current video (each rendition's encode or tune search, with its
latest trial, and the steps after), and the end of the log. Ctrl-C
stops the run. When it ends, it prints each video's time and outcome,
and saves the full log under your cache directory
(`~/Library/Caches/videoencoder/logs` on macOS, `~/.cache/...` on
Linux), keeping the last 50. Without a terminal, or with `--tui=off`,
it logs each video's place in the run (`n=3/15`) and how long it took.

Each command takes only the flags that apply to it, and rejects the
rest, so `tune --prune` is an error instead of being ignored. Flags:

| Flag | Meaning |
|:--|:--|
| `--config FILE` | Global config file (default `~/.config/videoencoder/config.yaml`) |
| `--rebuild GLOB` | Rebuild matching outputs even if they're up to date: `'*'`, `'av1-*'`, `sdr/h264-1080`, `audio`, `'poster/*'` |
| `--only GLOB` | Build only matching outputs, matched as `--rebuild` matches them, and stop: no packaging, pruning or publishing. For checking one rendition, such as `hdr/av1-2160`, after a change that leaves many stale. The next full render updates the stream. Works with `plan` too. |
| `--outdir DIR` | Override `outdir` |
| `--streamdir DIR` | Override `streamdir` |
| `--thumbnails=false` | Skip seek thumbnails |
| `--to a,b` | Publish to these targets instead of `publish.to` |
| `--publish=false` | Don't publish after rendering |
| `--prune` | When publishing, delete objects that are no longer part of the stream |
| `--jobs N` | Override `tune.jobs`: how many anchor searches `tune` runs at once |
| `--tui MODE` | Show progress in a terminal UI: `auto` (the default) when stderr is a terminal and `$TERM` isn't `dumb`, `on`, or `off` for plain logs |

## Configuration

Settings are YAML. The full default configuration, with comments, is
[`internal/config/default.yaml`](internal/config/default.yaml).

### Where settings come from

Settings are merged from these layers, lowest priority first:

1. The built-in defaults.
2. `~/.config/videoencoder/config.yaml`, or the file given with
   `--config`.
3. Every `videoencoder.yaml` from `/` down to the video's directory,
   shallowest first. A file applies to every video at or below its
   directory.
4. The video's entry under `videos:` in its own directory's
   `videoencoder.yaml`, if it has one.
5. The video's sidecar, `<name>.video.yaml`.
6. Flags on the command line.

Maps merge key by key, and everything else replaces what's below it.
`sources`, `presets`, `encoders`, `renditions`, `videos` and
`publish.targets` are maps keyed by name, so a layer can change a single rendition without
repeating the rest, or remove one with `null`. Anywhere else, `null`
does nothing. A section left empty, such as `tune:` with its contents
commented out, keeps the settings it would have inherited.

Unknown keys are an error, so a typo fails instead of being ignored.

Relative paths are relative to the directory of the video they belong
to, and `{name}` in a path is the video's name.

### A source library

One way to organize videos is a directory per video, with shared
settings at the top:

```
webvideo/
  source/
    videoencoder.yaml          # settings for every video below
    20261025-snow2/
      20261025-snow2.mov
      20261025-snow2.video.yaml
  transcodes/                  # renditions, safe to delete
  stream/                      # finished streams, safe to delete
```

```yaml
# source/videoencoder.yaml
outdir: ../../transcodes/{name}
streamdir: ../../stream/{name}
publish:
  to: [web]
  targets:
    web: {s3: {bucket: video, prefix: "{name}/"}}
```

```yaml
# source/20261025-snow2/20261025-snow2.video.yaml
sources:
  sdr: 20261025-snow2.mov
poster: {offset: 12.5s}
renditions:
  h264-480: null               # not for this one
  av1-2160: {quality: 30}      # only this rendition re-encodes
```

Running `videoencoder` in `source/` renders every video in the
library. Videos can also be defined without a sidecar, as entries
under `videos:` in a `videoencoder.yaml`:

```yaml
videos:
  hike:
    sources: {hdr: hike-hdr.mov, sdr: hike-sdr.mov}
```

### Settings

| Key | Meaning |
|:--|:--|
| `sources` | Source files by label, such as `sdr` and `hdr`. Sources are encoded in label order, and the first supplies the audio and poster. |
| `outdir` | Where renditions and the video's state file go. Default `transcodes/{name}`. |
| `streamdir` | Where the finished stream goes. Default `stream/{name}`. |
| `copy_sources` | Copy each source file into the stream under its own name, so it's published with the renditions. For sharing originals, such as test clips people can re-encode. Unchanged copies aren't redone. Default `false`. |
| `segment_duration` | The length of the stream's segments, default `5s`. Every rendition gets a keyframe at each segment boundary, so players can switch between renditions and codecs cleanly. |
| `presets` | Named encoder settings that encoders can start from, described below. |
| `encoders` | The ladder's encoder slots by name, described below. |
| `renditions` | The ladder, described below. |
| `tune` | How `videoencoder tune` works: `target_vmaf` (default 95), `ranges` (`[{start: 0s, duration: 8s}]`), `anchors` (2), `jobs` (anchor searches run at once; default 0, one per eight CPU threads), `vmaf_model` and `vmaf_model_2160`, and `hdr` with HDR renditions' `target_vmaf`, `vmaf_model` and `vmaf_model_2160` (see below). |
| `poster` | `offset`, `widths`, `formats` (`avif`, `jpg`, `webp`), `gainmap_formats`, and `hdr` for plain PQ HDR posters. |
| `thumbnails` | `enabled`, `interval` in seconds, `height`, and `columns` × `rows` per tile. |
| `publish` | `to` and `targets`; see [Publishing](#publishing). |

An encoder is one slot of the ladder, which renditions name. The
defaults are `h264`, `h265` and `av1`, and each starts from a preset:

```yaml
encoders:
  h264: {preset: x264-faster}
  h265: {preset: x265-faster}
  av1:  {preset: svtav1-p5}
```

A preset is an ffmpeg encoder with its fixed options:

```yaml
presets:
  x264-faster:
    codec: libx264
    quality: 18         # for renditions that don't set their own
    args: [-preset, faster, -pix_fmt, yuv420p, -profile:v, high]
    cap_bitrate: true   # pass each rendition's bitrate as -b:v, with -maxrate and -bufsize at 1.5x
    sdr_only: true      # never make HDR H.264
```

The built-in presets are `x264-faster`, `x265-faster`, `svtav1-p5`, and
Apple's hardware encoders, `hevc-videotoolbox` and
`h264-videotoolbox`. An encoder's own fields replace its preset's, so
switching H.265 to VideoToolbox and making no H.264 at all is just:

```yaml
encoders:
  h264: null
  h265: {preset: hevc-videotoolbox}
```

Deleting an encoder with `null` deletes its renditions too. To add an
option without repeating the preset's `args`, use `extra_args`, which
follow them and `gop_args`:

```yaml
encoders:
  h265: {extra_args: [-x265-params, aq-mode=3]}
```

An encoder or preset can set every field itself, with no preset:
`codec`, `quality_flag`, `quality`, `args`, `gop_args`, `extra_args`,
`cap_bitrate` and `sdr_only`. For codecs videoencoder knows (x264,
x265, SVT-AV1, libaom, rav1e, NVENC and VideoToolbox), `quality_flag`
and `quality` default to the codec's usual flag and a typical value.

Pixel formats should be explicit, since encoders otherwise keep the
source's, and 4:2:2 or 10-bit H.264 plays almost nowhere. Without
`cap_bitrate`, a rendition's bitrate is just a label in the manifest.

Players can only switch renditions at a segment boundary, and
packager can only start a segment at a keyframe. So every encode is
given `-force_key_frames` at each multiple of `segment_duration`, and
`gop_args` caps the encoder's own keyframe interval at one segment, with
`{gop}` replaced by the number of frames in a segment at the source's
frame rate (rounded up). Every built-in preset uses `-g {gop}`. x265's
also adds `-flags +cgop`: its GOPs are open by default, which makes its
forced keyframes CRA frames, not IDR, so a segment's first frames
could depend on the one before it. An encoder without `gop_args` still
gets the forced keyframes. Before this setting existed, each encoder
used its own default interval, and on a 24 fps clip H.264, H.265 and
AV1 produced segments of 15, 10 and 7 seconds that never lined up.

Quality numbers only mean something to one codec: CRF 20 for x265 is
not `-q:v 20` for VideoToolbox, where higher numbers are better. So
when `videoencoder tune` writes a rendition's quality, it also writes
`tuned_for`, the codec it was tuned for. If the encoder later uses a
different codec, videoencoder refuses the old value and asks for a
new tune. Switching between presets of the same codec, such as two x265
speed presets, keeps the tuned values.

A rendition is one rung of the ladder:

```yaml
renditions:
  av1-1080:  {encoder: av1,  height: 1080, quality: 31, bitrate: 2M}
  h264-480-capped: {encoder: h264, height: 480, quality: 23, bitrate: 1.5M, tune: false}
```

- `quality` is the value passed with the encoder's `quality_flag`.
  Without one, the rendition takes its encoder's `quality`.
- `bitrate` takes an optional `k` or `M` suffix.
- `args` adds options for this rendition only.
- `target_vmaf` overrides `tune.target_vmaf` for this rendition.
- `hdr` holds `quality`, `bitrate` and `target_vmaf` for the
  rendition's HDR encodes, in place of the ones above (see
  [SDR and HDR](#sdr-and-hdr)).
- `tune: false` keeps tune from changing it.
- `quality_from` and `quality_offset` take the quality from another
  rendition of the same encoder, plus an offset, in place of
  `quality`. The reference can be set in any layer, so this works in a
  project file for a rendition that only the defaults define:

  ```yaml
  renditions:
    av1-3240: {quality_from: av1-2160, quality_offset: 2}
    av1-4320: {quality_from: av1-2160, quality_offset: 4}
  ```

  It's worked out after all the layers are merged, so it follows the
  value tune writes for `av1-2160`, and `plan` shows the result with a
  comment like `# av1-2160 + 2`. Chains are allowed. A later layer can
  turn it off with `quality_from: ""`. If the reference has an
  `hdr.quality`, the HDR quality follows it with the same offset. The
  bitrate isn't derived, so set a cap by hand.

The width follows the source's aspect ratio. Renditions taller than
the source are dropped. The exception is an encoder whose renditions
all fall short of the source: that encoder's next taller rendition is
made at the source's own resolution.

### SDR and HDR

videoencoder handles SDR and HDR (PQ or HLG) sources without extra
settings. To make a stream that carries both, list both sources, or
run `videoencoder hdr.mov sdr.mov`. They should be the same length,
and ideally the same resolution. Each is encoded separately, and both
go into the same manifests.

An encoder with `sdr_only: true` encodes only the SDR source. If the
video has only an HDR source, that encoder gets a tone-mapped copy of
it instead.

HDR needs fewer bits than SDR at the same quality setting: AV1 at the
same CRF made HDR files about two-thirds the size of SDR ones, at a
slightly lower score. So each rendition's HDR encodes can have their
own settings under `hdr`, and any left unset follow the SDR ones:

```yaml
renditions:
  av1-1080: {quality: 32, hdr: {quality: 27}}
```

Tune writes `hdr.quality` itself (see [Tuning quality](#tuning-quality)).

## Rebuilding

An output is up to date when a file with its key in the name exists.
The key is a hash of everything that went into the output except the
input's path. So:

- Changing a rendition's settings rebuilds only that rendition.
- Re-exporting a source rebuilds everything made from it.
- Moving or renaming a source rebuilds nothing.

Upgrading ffmpeg doesn't count as a change, so use `--rebuild '*'` if
you want to re-encode everything with a new version.

Outputs are written under a `.partial` name and renamed when they're
complete, so an interrupted run never leaves a truncated file that
looks finished. The rest of the cleanup waits until the new stream is
complete: replaced and removed outputs are deleted then. Each video's
`<name>.state.json` in its `outdir` records what was built, so a
deletion only ever touches files that video made, and several videos
can share an `outdir` or `streamdir`.

Every stale rendition of a source is encoded in one ffmpeg process,
because decoding is often a large share of the cost.

## Tuning quality

The default quality settings are a starting point. Footage differs
enormously in how hard it is to compress, so the setting that's
plenty for one clip falls well short for another.
`videoencoder tune` measures this for each video:

1. It cuts samples from the first SDR source, one for each of
   `tune.ranges` (`duration` from `start`), and the same from the
   first HDR source. A quality setting passes only if it reaches the
   target on every range, and the bitrate comes from the range that
   needed the most. To skip a title sequence without leaving it
   unchecked, say:

   ```yaml
   tune:
     ranges:
       - {start: 0s, duration: 4s}
       - {start: 30s, duration: 8s}
   ```

   The SDR renditions are tuned on the SDR sample, and the HDR ones on
   the HDR sample. An HDR-only video's SDR-only encoders are tuned on
   the HDR sample, tone-mapped as a render would.
2. For each encoder, it searches the anchor renditions for the quality
   setting that just reaches the target VMAF. The anchors are the
   shortest and tallest renditions each VMAF model scores (plus the
   middle with `anchors: 3`). With the built-in model, whose 4K variant
   takes over at 2160p, a 480p–2160p ladder has anchors at 480p, 1440p
   and 2160p.

   The searches are independent, and each runs one trial encode and
   scoring at a time, which leaves most of a big machine idle. So
   `tune.jobs` of them run at once: by default one per eight CPU
   threads, so 3 on a 28-thread Mac and 8 on a 64-thread
   Threadripper. Set it in your global config to suit each machine,
   or with `--jobs`. Encoders judged at the same size share one
   reference, which is made once.
3. It interpolates the renditions in between, linearly in log height.
   Models score on different scales, so it never interpolates across
   two of them: doing that put 1440p as much as 25 CRF too high.
4. It writes each rendition's `quality`, and `bitrate` for encoders
   that cap it, into the video's sidecar, under `hdr` for the HDR
   encodes, along with `tuned_for`, the codec. Each value gets a
   comment saying how it was found.

Review the sidecar, then render as usual.

```yaml
# written by videoencoder tune
renditions:
  av1-2160:
    tuned_for: libsvtav1
    quality: 31 # tuned 2026-10-31: VMAF 90.5 (target 90)
  av1-1440:
    tuned_for: libsvtav1
    quality: 29 # interpolated by tune 2026-10-31 for VMAF 90
    hdr:
      quality: 24 # interpolated by tune 2026-10-31 for VMAF 90
```

With a `vmaf_model`, the comments name it instead of "VMAF".

Each rendition is scored against the sample scaled to its own size, a
lossless reference, using VMAF's 4K model at 2160p. The score
therefore measures what the encoder lost, not detail the smaller size
couldn't hold anyway. HDR is tone-mapped to SDR for scoring, since
VMAF is an SDR metric.

`tune.vmaf_model` picks the VMAF model. The default, `v0`, is
libvmaf's built-in v0.6.1, with its 4K variant at 2160p. Anything else
is the path of a libvmaf JSON model, such as Netflix's v1 models.
`vmaf_model_2160` replaces it for 2160p and up. Each model scores on
its own scale, so pick `target_vmaf` for the model. Trials scored with
one model are never reused for another.

`tune.hdr` sets `target_vmaf`, `vmaf_model` and `vmaf_model_2160` for
the HDR renditions. Unset, they follow the SDR settings: the target
falls back to the rendition's `target_vmaf`, then `tune.target_vmaf`,
and both models follow the SDR ones unless `hdr.vmaf_model` is set. A
rendition's own `hdr.target_vmaf` beats them all.

```yaml
tune:
  target_vmaf: 95
  vmaf_model: /opt/vmaf/vmaf_v1.0.16_3d0h.json
  hdr:
    target_vmaf: 93
    vmaf_model: /opt/vmaf/hdr_vmaf_v1.1.8_3d0h.json
    vmaf_model_2160: /opt/vmaf/hdr_vmaf_v1.1.8_1d5h_2160.json
```

An HDR model, one whose JSON has a PQ `conversion_target`, judges HDR
as HDR. It converts both sides to the PQ ICtCp 4:2:0 16-bit pictures the model
expects instead of tone-mapping them. Both are also scaled to the
model's display height (its `resolution.display`, 1080 or 2160), so a
480p rendition is judged as it looks on a 1080p screen: bicubic to
enlarge, lanczos to shrink, as libvmaf does it. ffmpeg does all of
this, and libvmaf is given a copy of the model without its
`conversion_target`. libvmaf 3.2 ignores it, and newer versions act on
it only when built with zimg, which static ffmpeg builds often aren't;
this way scores are the same with any of them. An HDR model can't
judge SDR, so with one as `tune.vmaf_model` SDR renditions fall back
to `v0`, with a warning; put it under `tune.hdr` instead. These
models are clipped to 0–100, like the others.

Three kinds of rendition are left as configured. Ones marked
`tune: false` are useful for fixed rungs, such as a bitrate-capped one
for slow networks. Ones taller than 2160p are left alone because VMAF
has no model for 6K or 8K, so set those by hand, or with
`quality_from` relative to a tuned rendition. Ones with `quality_from`
follow their reference, which tune may change.

Every trial encode is logged to `tune-results.csv` in the video's
`outdir`. Each trial is keyed by everything that affects its score:
the source's content, the sample's position and length, the encoder's
arguments, the reference, and the ffmpeg version. A later run reuses
the trials that still apply. Asking for VMAF 93 after 95 starts from
what the earlier run found, and repeating a run encodes nothing.
Delete the file to start over.

VMAF 95 is a demanding target. Very noisy footage, such as falling
snow, can look fine at 90 for a fraction of the bits.

## Publishing

With `publish.to` set, each render ends by copying the stream to the
named targets. A target is either a local directory or an
S3-compatible bucket (SeaweedFS, MinIO, AWS):

```yaml
publish:
  to: [seaweed]
  targets:
    www:     {dir: /srv/www/videos/{name}}
    seaweed: {s3: {bucket: video, prefix: "{name}/"}}
```

- **What's uploaded.** Only new or changed files. Manifests go last,
  so a player never sees one that points at a missing file.
- **Caching.** Key-named files are marked immutable, and manifests are
  marked `no-cache`. Every file gets its proper `Content-Type`.
- **Deleting.** Nothing is deleted unless you pass `--prune`. Even
  then, a target can only be pruned if its `dir` or `prefix` contains
  `{name}`, so pruning one video can never touch another.
- **S3 settings.** An `s3` target also accepts `endpoint`, `region`
  and `profile`. With no `endpoint`, `$S3_ENDPOINT_URL` is used.
- **Credentials** never go in config files. They come from
  `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, or from
  `~/.aws/credentials`.

To serve a bucket straight to browsers, it needs public read access
and CORS headers that allow your site's origin and the `Range` header.

## encbench

encbench answers the question "which encoder settings give the
smallest file at a given quality, and at what cost in encoding time?"

You give it a directory of sample clips and a set of named flagsets.
For each sample and flagset, it searches for the quality setting
(CRF or the encoder's equivalent) that just reaches a target VMAF. It
then records the answer's size, bitrate, encode time and quality
scores.

It searches rather than sweeping the range, which typically takes
3–8 encodes per pair. Every encode is logged, so adding a sample or a
flagset only runs the new combinations.

The sample masters used in the "Video Encoding for the Web in 2026"
posts, about 98 GB in all, can be downloaded with
[`fetch-samples.sh`](fetch-samples.sh):

```sh
./fetch-samples.sh -n             # list what it would download
./fetch-samples.sh -d masters     # all of them, into masters/
./fetch-samples.sh heron3 cnc     # only these
```

It can be rerun at any time: complete files are skipped, interrupted
downloads resume, and masters that have changed on the server are
downloaded again.

### encbench configuration

```yaml
# bench.yaml
samples: samples        # directory of sample clips, about 30 s each
workdir: work
scales: [2160, 1080]    # test at these heights; taller samples are downscaled once
target_vmaf: 95
vmaf_model: v0          # built-in model below 2160p
vmaf_model_2160: vmaf_v0.6.1.json  # the 1080p model at 2160p too
hdr:                    # HDR samples
  target_vmaf: 92
  vmaf_model: hdr_vmaf_v1.1.8_3d0h.json
  vmaf_model_2160: hdr_vmaf_v1.1.8_1d5h_2160.json
vmaf_threads: 4         # libvmaf n_threads
vmaf_jobs: 3            # scorings run at once
references:             # score a sample against another file in samples/
  yuri-h265.mov: yuri.mov
seed_results: mac-results.csv  # another machine's log, to start searches at its answers
batch: 1                # quality settings each search tries per round (see Batches)
flagsets:
  x265-medium:
    args: [-pix_fmt, yuv420p10le, -c:v, libx265, -preset, medium]
  svtav1-p5:
    args: [-c:v, libsvtav1, -preset, "5"]
  x264-faster:
    args: [-pix_fmt, yuv420p, -c:v, libx264, -preset, faster]
    sdr_only: true      # skip HDR samples
  nvenc-hevc:
    args: [-c:v, hevc_nvenc, -preset, p7]
    quality: {flag: -cq:v, min: 20, max: 45, step: 0.5}
```

- **Quality ranges.** Each flagset's quality flag, range and step
  default from its encoder. x264 and x265 search CRF 10–40, SVT-AV1
  and libaom 10–63, and VideoToolbox `-q:v`, where higher is better.
  Override them with `quality`.
- **Scales.** A sample taller than a scale is downscaled once with
  Lanczos, and the copy is cached in `workdir`. A sample no taller
  than a scale is used as it is. Encodes are scored at the sample's
  size, with the 4K model at 2160p unless `vmaf_model_2160` says
  otherwise.
- **VMAF models.** `vmaf_model` and `vmaf_model_2160` pick libvmaf JSON
  models, as for `tune`. `hdr` sets `target_vmaf`, `vmaf_model` and
  `vmaf_model_2160` for HDR samples, falling back to the SDR settings.
  An HDR model scores HDR samples as HDR; any other model scores them
  tone-mapped. Each row's `scorer` names the model, and only rows
  scored the way the config says count. Changing a model searches
  again, and changing it back reuses the earlier encodes.
- **References.** A sample is normally scored against itself. To
  measure what a lossy master costs, use `references` to score an
  export against the original it was made from.
- **Timing on another machine.** Encodes are keyed by what was
  encoded, not where, so copying a `results.csv` to a new machine
  makes its searches look done. Instead, give the new machine a
  `workdir` of its own and the old log as `seed_results`, which is
  read but never written. A search that the old log answers (same
  sample name, flagset args and scoring) starts at that answer and
  tries its neighbors first, so it usually takes two encodes, both
  timed. If the new machine's encodes differ slightly, as x264's do
  with a different thread count, the search widens from there.
- **Batches.** With `batch: N`, each search tries up to N quality
  settings per round instead of one. They're encoded by a single
  ffmpeg, which decodes the sample once for all of them, and scored in
  parallel. Once the answer is bracketed, the settings spread across
  the bracket rather than crowding the best guess, so a guess that's
  off doesn't cost a round. A search then takes fewer rounds but more
  encodes: with seeds 3 to 60 steps from the answer, one setting at a
  time averages about 5.2 rounds and 5.2 encodes, 2 at a time 3.6
  rounds and 7.0 encodes, and 4 at a time 3.2 rounds and 11.5 encodes.
  It pays only where a second encode costs much less than the first,
  and that depends on what limits the encoder. On an RTX 4090, at
  1080p, a second `hevc_nvenc` encode added 7% at `-preset p4` (each
  of its two NVENC engines takes one), 33% at `h264_nvenc -preset p1`,
  and 74% at p7 with `-tune uhq`, which keeps both engines busy alone.
  Software encoders sharing the same cores gain little. So
  `batch_by_encoder` sets it per encoder (as `-c:v` names it) and a
  flagset's own `batch` overrides that, with `batch` the default for
  the rest. The batch's encodes share their time, so their rows are
  logged with `timed` false, and a search that ends on one encodes the
  answer again by itself and logs it again, timed. That later row is
  the one reports use. An interrupted run re-times the answer next
  time, without searching again.
- **Color limits.** `min_ciede2000`, `min_chroma_psnr`,
  `max_color_shift` and `max_color_flicker` can fail encodes for color
  errors, which VMAF ignores entirely. All of them are off by default.
  The color measures are always recorded either way, and reports flag
  outliers.

### encbench commands

```
encbench [-config bench.yaml] run [-n] [-sample GLOB] [-flagset GLOB]
encbench [-config bench.yaml] report
encbench [-config bench.yaml] prune [-n]
encbench queue [-watch] [-report=false] [-poll 5s] [-tui auto] [-http ADDR] DIR
```

- **`run`** searches every unfinished sample and flagset pair. `-n`
  shows what would be encoded, and `-sample` and `-flagset` limit the
  run.
- **`report`** prints the answers as Markdown tables, one per sample.
- **`prune`** deletes kept encodes of samples and flagsets no longer in
  the config.
- **`queue`** works through a directory of work files, one sample and
  flagset (a *unit*) at a time, in a terminal UI. See below.

Encodes run one at a time, so timings are comparable. Scoring runs in
parallel between encodes. Don't run anything else CPU-heavy on the
machine while encbench is running, or the timings will be meaningless.

### encbench queue

`encbench queue DIR` is a long-running way to line up work. Each
`.yaml` or `.yml` file in `DIR` is either an ordinary bench config, which
runs all of its flagsets on all of its samples, or a job that names a
config and picks some of it:

```yaml
config: ../x264-tunes-1080p.yaml
flagsets: [x264-ultrafast-kf10-film, "x264-*-kf10-film"]
samples: [20180529-band.mp4, "2026*-sdr.mov"]   # optional; default all
report: ../report-x264-film.md                  # optional
```

Files run in name order, so names like `00-h264.yaml`, `10-h265.yaml`
and `50-av1-8k.yaml` set their priority. Within a file, flagsets run in
the order the file lists them (or the config does). Each flagset runs
on every sample, in the order listed or else by name, before the next
flagset starts. Entries can be globs, and an entry that matches nothing
is an error.

Only one unit runs at a time. Before starting each one, the queue
rescans `DIR`:

- A new file with a lower name runs next.
- A deleted file's remaining units are dropped.
- An edited file is parsed again. Units that `results.csv` already
  answers are skipped, as `run` skips them.
- A file that doesn't parse, or names a flagset or sample that doesn't
  exist, is shown with its error and skipped until it changes.
- A unit that fails isn't retried until its file changes.

Edits never affect the unit that is already running.

Before each unit, the queue waits while DaVinci Resolve has a `.mov`
open for writing, or while a `videoencoder` or another `encbench` is
running, so that timings stay clean.

When a file's last unit finishes, the queue writes its report to
`report-<name>.md` in `DIR`, or to the path the job's `report:` gives.
`-report=false` turns this off.

Without `-watch`, the queue exits when nothing is left to run. With it,
the queue keeps polling for new work.

In the terminal UI:

- **`p`** pauses after the current unit, or resumes.
- **`q`** quits after the current unit finishes.
- **Ctrl-C** abandons the current unit and quits. Encodes that were
  already scored are kept in `results.csv`, so a later run picks up
  where this one stopped.

Without a terminal (`-tui off`), the queue logs to stderr, and an
interrupt abandons the current unit.

With `-http ADDR`, such as `-http :8090`, the queue also serves a web
view, alongside the terminal UI or without it. The page shows what the
terminal UI does and updates as the queue changes. Its **Pause** button
works like `p`. The same is available as JSON:

- `GET /api/state` is the queue's state.
- `GET /api/events` streams it as server-sent events, on every change.
- `POST /api/pause` and `POST /api/resume` pause after the current unit,
  and resume.

The web view has no login. Anyone who can reach the address can watch
the queue and pause it, so `-http localhost:8090` keeps it to this
machine.

### encbench output

These are all in `workdir`:

- **`results.csv`.** One row per encode, appended as it finishes:
  - the sample, flagset, arguments and quality setting
  - size and bitrate
  - wall, user and system time, and peak memory
  - VMAF: mean, harmonic mean, 1st percentile and NEG
  - PSNR for each plane
  - CIEDE2000 and the color shift and flicker measures
  - the ffmpeg version and host
  - the CPU model and thread count, and the encoder library's version
    and the SIMD it chose where it reports them (for example
    `x265 4.3+1-e9b8812 (NEON Neon_DotProd Neon_I8MM)`, or
    `macOS 26.5.1` for VideoToolbox), so timings from different
    machines and builds can be told apart
  - `timed`: whether the times are the encode's own. They aren't for
    an encode made in a batch, whose times are the whole batch's;
    its answer is logged again, timed, after a re-run by itself.

  Interrupting a run loses only the scorings in progress.
- **`best.csv`.** The answer for each pair, regenerated on every run.
  Changing `target_vmaf` re-ranks the results without re-encoding
  anything already measured.
- **`keep/`.** The best encode of each pair, kept for comparing by eye.
  Other encodes are deleted once they've been scored.

### How encbench measures quality

- **Frame pairing.** Frames are paired by position, not timestamp.
  Timestamps round differently between containers, and pairing by
  timestamp can compare frames that don't match.
- **Upscaling.** Encodes smaller than their reference are scaled back
  up (bicubic) before scoring.
- **HDR.** HDR samples are encoded as HDR. Both sides are tone-mapped
  to SDR for VMAF unless `hdr.vmaf_model` is an HDR model. The color
  shift measures are computed in HDR itself.
- **Recorded scores.** Each row records the mean VMAF, its harmonic
  mean and 1st-percentile frame score, and VMAF NEG. The 1st percentile
  catches a bad second that the mean hides. NEG discounts the score
  boost that sharpening earns.
- **Color.** Each row also records chroma PSNR, CIEDE2000, and
  ΔE_ITP block measures of local and flickering color casts. VMAF is
  blind to chroma: a 20° hue rotation still scores 99.9.

Bitrate at a fixed VMAF varies by more than a factor of 100 across
footage:
- **Nearly static clips** reach VMAF 95 at 4K in a few Mb/s.
- **Handheld and action footage** needs tens of Mb/s.
- **Dense noise** like falling snow needs 100 Mb/s or more.

AV1 usually needs the fewest bits and H.264 the most.

[`DESIGN.md`](DESIGN.md) has the details and the reasoning behind
them.
