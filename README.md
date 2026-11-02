# videoencoder

videoencoder turns a video file into a directory you can serve over
plain HTTP. The directory holds a ladder of H.264, H.265 and AV1
renditions, DASH and HLS manifests, posters and seek thumbnails, and
any adaptive player, such as Shaka Player, hls.js or Safari, can
stream it.

Each video is described by a small YAML file next to its sources, so a
library of videos can be rebuilt at any time without hunting down
sources or remembering settings. Re-running is cheap. Only outputs
whose source or settings changed are rebuilt, and only changed files
are uploaded when publishing.

The repository also contains **encbench**, a tool for comparing
encoder settings. It finds the cheapest quality setting that reaches a
target VMAF for each set of flags on a set of sample clips. See
[encbench](#encbench) below.

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

## What it produces

For each video, videoencoder makes:

- **Renditions.** One per rung of the ladder, for each source. The
  default ladder is H.264 up to 1080p, H.265 up to 2160p and AV1 up to
  4320p, but never above the source's resolution.
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
file, and its name, alone. That's also what lets a CDN cache them
forever.

## Commands

```
videoencoder [flags] [dir | name.video.yaml | media files ...]
videoencoder plan    [flags] [same]
videoencoder tune    [flags] [same]
videoencoder publish [flags] [same]
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

Flags:

| Flag | Meaning |
|:--|:--|
| `--config FILE` | Global config file (default `~/.config/videoencoder/config.yaml`) |
| `--rebuild GLOB` | Rebuild matching outputs even if they're up to date: `'*'`, `'av1-*'`, `sdr/h264-1080`, `audio`, `'poster/*'` |
| `--outdir DIR` | Override `outdir` |
| `--streamdir DIR` | Override `streamdir` |
| `--thumbnails=false` | Skip seek thumbnails |
| `--to a,b` | Publish to these targets instead of `publish.to` |
| `--publish=false` | Don't publish after rendering |
| `--prune` | When publishing, delete objects that are no longer part of the stream |

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
`sources`, `encoders`, `renditions`, `videos` and `publish.targets` are
maps keyed by name, so a layer can change a single rendition without
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
| `encoders` | ffmpeg encoders by name, described below. |
| `renditions` | The ladder, described below. |
| `tune` | How `videoencoder tune` works: `target_vmaf` (default 95), `start` (0s), `duration` (8s), `anchors` (2), and `vmaf_model` and `vmaf_model_2160` (see below). |
| `poster` | `offset`, `widths`, `formats` (`avif`, `jpg`, `webp`), `gainmap_formats`, and `hdr` for plain PQ HDR posters. |
| `thumbnails` | `enabled`, `interval` in seconds, `height`, and `columns` × `rows` per tile. |
| `publish` | `to` and `targets`; see [Publishing](#publishing). |

An encoder is an ffmpeg codec with its fixed options:

```yaml
encoders:
  av1:
    codec: libsvtav1
    quality_flag: -crf
    args: [-preset, "5", -pix_fmt, yuv420p10le]
  h264:
    codec: libx264
    quality_flag: -crf
    args: [-preset, faster, -pix_fmt, yuv420p, -profile:v, high]
    cap_bitrate: true   # pass each rendition's bitrate as -b:v, with -maxrate and -bufsize at 1.5x
    sdr_only: true      # never make HDR H.264
```

Pixel formats should be explicit, since encoders otherwise keep the
source's, and 4:2:2 or 10-bit H.264 plays almost nowhere. Without
`cap_bitrate`, a rendition's bitrate is just a label in the manifest.

A rendition is one rung of the ladder:

```yaml
renditions:
  av1-1080:  {encoder: av1,  height: 1080, quality: 31, bitrate: 2M}
  h264-480-capped: {encoder: h264, height: 480, quality: 23, bitrate: 1.5M, tune: false}
```

- `quality` is the value passed with the encoder's `quality_flag`.
- `bitrate` takes an optional `k` or `M` suffix.
- `args` adds options for this rendition only.
- `target_vmaf` overrides `tune.target_vmaf` for this rendition.
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
  turn it off with `quality_from: ""`. The bitrate isn't derived, so
  set a cap by hand.

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

1. It cuts a sample from the first SDR source, `tune.duration`
   seconds from `tune.start`. For an HDR-only video, it uses the HDR
   source.
2. For each encoder, it searches the anchor renditions, the shortest
   and tallest (plus the middle with `anchors: 3`), for the quality
   setting that just reaches the target VMAF.
3. It interpolates the renditions in between, linearly in log height.
4. It writes each rendition's `quality`, and `bitrate` for encoders
   that cap it, into the video's sidecar. Each value gets a comment
   saying how it was found.

Review the sidecar, then render as usual.

```yaml
# written by videoencoder tune
renditions:
  av1-2160:
    quality: 31 # tuned 2026-10-31: VMAF 90.5 (target 90)
  av1-1440:
    quality: 29 # interpolated by tune 2026-10-31 for VMAF 90
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

```yaml
tune:
  target_vmaf: 93
  vmaf_model: /opt/vmaf/hdr_vmaf_v1.1.8_3d0h.json
  vmaf_model_2160: /opt/vmaf/hdr_vmaf_v1.1.8_1d5h_2160.json
```

An HDR model, one whose JSON has a PQ `conversion_target`, judges HDR
as HDR. Tune samples the HDR source instead of the SDR one, and
converts both sides to the PQ ICtCp 4:2:0 16-bit pictures the model
expects instead of tone-mapping them. libvmaf 3.2 doesn't do that
conversion itself. SDR-only encoders' renditions, which an HDR model
can't judge, fall back to `v0`. These models are clipped to 0–100,
like the others.

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

### encbench configuration

```yaml
# bench.yaml
samples: samples        # directory of sample clips, about 30 s each
workdir: work
scales: [2160, 1080]    # test at these heights; taller samples are downscaled once
target_vmaf: 95
vmaf_threads: 4         # libvmaf n_threads
vmaf_jobs: 3            # scorings run at once
references:             # score a sample against another file in samples/
  yuri-h265.mov: yuri.mov
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
  size, with the 4K model at 2160p.
- **References.** A sample is normally scored against itself. To
  measure what a lossy master costs, use `references` to score an
  export against the original it was made from.
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
```

- **`run`** searches every unfinished sample and flagset pair. `-n`
  shows what would be encoded, and `-sample` and `-flagset` limit the
  run.
- **`report`** prints the answers as Markdown tables, one per sample.
- **`prune`** deletes kept encodes of samples and flagsets no longer in
  the config.

Encodes run one at a time, so timings are comparable. Scoring runs in
parallel between encodes. Don't run anything else CPU-heavy on the
machine while encbench is running, or the timings will be meaningless.

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
- **HDR.** HDR samples are encoded as HDR, but both sides are
  tone-mapped to SDR for VMAF. The color shift measures are computed
  in HDR itself.
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
