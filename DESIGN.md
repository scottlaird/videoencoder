# Design

This covers two related tools that live in this repo:

1. **videoencoder**: turns one video (or an HDR+SDR pair) into a
   DASH/HLS stream, re-rendering only what changed, and publishes it to
   a local directory or an S3-compatible bucket.
2. **encbench**: searches for the quality setting that makes each
   candidate ffmpeg flagset reach a target VMAF on a set of sample
   clips. It records every encode, so new flagsets and new samples only
   cost the encodes they actually need.

Both share the ffmpeg/ffprobe plumbing, so they share a module. Open
questions are marked **Q:**.

## Package layout

```
cmd/videoencoder/   CLI: render, plan, publish
cmd/encbench/       CLI: run, report, prune
internal/config/    layered YAML loading and merging
internal/probe/     ffprobe wrapper (VideoData, color/HDR detection)
internal/ffmpeg/    command building, running with progress, atomic outputs
internal/plan/      config + probe -> renditions, each with a build key
internal/state/     build-key bookkeeping (replaces .status files)
internal/thumbs/    poster images and seek-thumbnail tiles
internal/pack/      shaka-packager invocation + manifest post-processing
internal/publish/   local-dir and S3 sync
internal/bench/     search, VMAF, results CSV
```

`main` stays thin. Each package returns errors instead of panicking.
Tests use the stdlib `testing` package with table-driven got/want.
Anything that shells out goes behind a small `Runner` interface, so
plan, state and search logic can be tested without running ffmpeg. A
few integration tests make a 2-second synthetic clip with `testsrc2`
and run real ffmpeg. They skip when ffmpeg isn't on `PATH`.

---

## 1. videoencoder

### Configuration

YAML via `go.yaml.in/yaml/v4` (the github.com/yaml/go-yaml module).
Layers are merged from lowest to highest priority:

1. `$XDG_CONFIG_HOME/videoencoder/config.yaml`, defaulting to
   `~/.config/videoencoder/config.yaml`. These are the system-wide
   defaults: encoders, the rendition ladder, packaging, publish
   targets and path mappings.
2. Every `videoencoder.yaml` found walking down from the filesystem
   root to the video's directory, shallowest first. A project
   directory can override settings for all the videos beneath it.
3. The video's own settings. These can be an entry under `videos:`
   in a project's `videoencoder.yaml`, a sidecar `<name>.video.yaml`
   next to the sources, or both. If both exist, the sidecar is merged
   on top of the project entry.

The built-in defaults (today's hard-coded ladder) sit under layer 1, so
it works with no config at all.

**Merge rules.** Maps merge key by key, recursively. Scalars and lists
replace. An explicit `null` deletes the inherited key. Anything a lower
layer might want to patch individually is therefore a **map keyed by
name, never a list**. This is the main reason inheritance hurts in CUE
and in most YAML schemes, and keyed maps sidestep it:

```yaml
# ~/.config/videoencoder/config.yaml
encoders:
  av1:
    codec: libsvtav1
    quality_flag: -crf
    args: [-preset, "5"]
  h265:
    codec: libx265
    quality_flag: -crf
    args: [-preset, faster, -profile:v, main422-10, -tag:v, hvc1]
renditions:
  h264-1080: {encoder: h264, height: 1080, quality: 18, bitrate: 4M}
  av1-2160:  {encoder: av1,  height: 2160, quality: 32}
  av1-4320:  {encoder: av1,  height: 4320, quality: 32}
poster:
  offset: 1s
  widths: [320, 640, 1280, 1920, 3840]
  formats: [avif, jpg]
seek_thumbnails: {interval: 5s, height: 180, grid: 5x5}
paths:                       # logical root -> per-OS mount point
  nas: {darwin: /Volumes/photography, linux: /mnt/photography}
publish:
  default: seaweed
  targets:
    seaweed:
      s3: {endpoint: http://seaweed:8333, bucket: video, region: us-east-1}
      prefix: "{{ .Name }}/"
```

```yaml
# 20250301-deception1.video.yaml
sources:
  hdr: 20250301-deception1.mov
  sdr: 20250301-deception1-sdr.mov
poster: {offset: 01:12.5}
renditions:
  av1-2160: {quality: 30}    # only this rendition re-renders
  av1-4320: null             # dropped for this video
```

Running `videoencoder` with no arguments in a directory renders every
video defined under it, whether by a sidecar or a `videos:` entry, and
counts each video once. `videoencoder foo.video.yaml` renders just
that one. You still need command-line flags for one-off overrides and
for a bare `videoencoder foo.mov` with no job file, but they are never
required to re-render. `videoencoder plan` prints the merged config,
with the layer each value came from, plus the list of renditions to
build and why. That's the debugging tool for "why did this rebuild?"

Unknown keys are an error, using strict decoding of each layer. A typo
in a project override should fail, not do nothing.

Credentials are never stored in config. S3 keys come from the usual
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` variables or from
`~/.aws/credentials` with a named profile.

### Planning and incremental rebuilds

`plan` turns (merged config, probe data) into a list of renditions.
Each rendition has:

- a **build key**: the SHA-256 of the canonical argument vector (with
  paths made relative), plus the source file's identity (size,
  mtime, and a hash of its first and last MiB), plus a
  schema version;
- an output name derived from the rendition name and a short key
  prefix, e.g. `av1-2160.7c3e1a.mp4`.

Putting the key in the filename means a changed setting produces a
*new* object instead of overwriting the old one. That helps S3: an
unchanged rendition keeps its name and ETag, so the sync skips it, and
nothing that is already published is ever overwritten in place.

A single `state.Needs(key, path)` replaces both `MaybeExecute` and
`IsEncodeNeeded`. It returns a rebuild reason whenever the output is
missing, whatever the state file says, which fixes the crash on a
`.status` file whose output is gone. State lives in one
`state.json` per video work directory, not one `.status` file per
output. Outputs are written to `name.partial` and renamed on success,
so an interrupted ffmpeg run never looks finished. Every output
directory is created with `MkdirAll` up front.

The ffmpeg version is deliberately **not** part of the key. A
`brew upgrade` shouldn't re-render the whole library. `--rebuild` and
`--rebuild=av1-*` force it.

The single-decode, multi-output ffmpeg invocation stays. Every stale
rendition of one source is built in one ffmpeg process, because
decoding (especially 6K H.265) is a large share of the cost.

### Posters

The "album" still image. For each source tone that is present, we
write `poster-{sdr,hdr}-{width}.{fmt}`, for example
`poster-sdr-640.avif` and `poster-hdr-1920.avif`:

- The frame is taken at `poster.offset` with an accurate seek (`-ss`
  after `-i`, which is slower but frame-exact). It's decoded once and
  then scaled to each width.
- SDR: AVIF and JPEG (and WebP if configured).
- HDR: AVIF only, 10-bit, tagged PQ/BT.2020. JPEG can't carry it
  without gain maps, which are a later step.
- If a video has only an HDR source, the SDR poster is tone-mapped
  with `zscale`+`tonemap` (or `libplacebo`), so every page always
  has an SDR poster.
- We also write a small `poster.json` listing every variant with its
  width, height, format and tone, so a page template can build
  `<picture>`/`srcset` without guessing names.

Later: merge the SDR and HDR posters into one gain-mapped AVIF/JPEG.

### Seek thumbnails

Shaka Packager 3.9 has no thumbnail support. The existing
`--add_thumbnail_adaptation_set` flag doesn't exist in packager and
would make it fail with `--thumbnails`. So thumbnails become a
post-processing step:

1. ffmpeg: `fps=1/5,scale=-2:180,tile=5x5` produces
   `thumbs/tile-%d.jpg`. Each tile covers 25 × 5 s = 125 s.
2. DASH: we parse `dash.mpd` with `encoding/xml` and append one
   `AdaptationSet contentType="image" mimeType="image/jpeg"`. It holds
   a `SegmentTemplate` (`media="thumbs/tile-$Number$.jpg"`,
   `duration` = the tile's time span, `startNumber=1`), one
   `Representation` whose `width`/`height` are the whole tile image's,
   and `EssentialProperty
   schemeIdUri="http://dashif.org/thumbnail_tile" value="5x5"`. This
   is the DASH-IF IOP form that Shaka Player and dash.js read.
3. HLS: we append `#EXT-X-IMAGE-STREAM-INF` to `hls.m3u8` and write
   an image playlist using `#EXT-X-IMAGES-ONLY` and
   `#EXT-X-TILES:RESOLUTION=…,LAYOUT=5x5,DURATION=5`. That's the
   Roku/Apple image-media-playlist form, which Shaka Player also
   reads.
4. Optionally a WebVTT sprite track (`#xywh=`) for players that only
   know that format.

All four steps are deterministic, so they rerun cheaply whenever the
manifests change.

### Packaging

Same as now (one shaka-packager run over every rendition plus audio),
with two changes: packager writes into a staging directory, and the
thumbnail post-processor runs before publish. Audio comes from the
first source, which is fine since the sources differ only in color.

Packager rewrites every output on every run. The question for S3 is
whether unchanged inputs give byte-identical outputs. I'll test this
first. If they don't, we key packaged files by their input build key
and only republish the ones whose key changed, rather than trusting
their content hash.

### Publishing

Publish targets are a local directory or S3, using
`github.com/minio/minio-go/v7` (approved 2026-10-24). Publishing syncs
the staging directory to the target:

- List the remote prefix, which returns sizes and ETags. Upload a
  file only if it's missing or its size/MD5 differs. Files up to
  5 GiB use a single PUT, where the ETag is the MD5 on SeaweedFS,
  MinIO and AWS alike. Larger files use multipart upload, and we
  store their SHA-256 in object metadata and compare against that.
- **Manifests go last**, after every segment they reference has been
  uploaded. A player never sees a manifest that points at a missing
  object.
- Objects that are no longer referenced are left alone by default.
  `--prune` deletes them, but only after the new manifests are up.
  Old rendition names are cheap to keep, and keeping them means
  players already streaming the old manifest still work.

Changing the quality of AV1 2160p re-encodes that one rendition and
reruns packager. If packager's output is deterministic, the upload is
the new `av1-2160.<key>.mp4` plus the manifests and nothing else.

### Temporal (later)

Not part of the first pass, but the structure above is meant to
make it a thin layer:

- Workflow `RenderVideo(jobFile)`, with activities `Plan`,
  `EncodeSource` (all stale renditions of one source in one ffmpeg
  run), `Posters`, `Package` and `Publish`.
- Workflow inputs use **logical paths** (`nas:Videos/2025/foo.mov`).
  Each worker maps them using `paths:` from its own config, so the
  Linux and Mac mount points never meet.
- One video per machine: heavy activities go to a task queue whose
  workers run `MaxConcurrentActivityExecutionSize: 1`. Light ones
  (plan, publish) go to a shared queue. Fifty videos then spread
  across N machines, one at a time each.
- Splitting renditions of one video across machines is opt-in per
  job (`split: by-encoder`). It trades an extra decode for
  parallelism, which only pays off for high-resolution AV1.
- Intermediate renditions have to be visible to whichever worker
  packages them. They live in a work prefix on the NAS (or in a
  scratch bucket), keyed by build key, so they double as the
  cross-machine cache.

This needs the Temporal Go SDK as a dependency, which needs approval
when we get there.

---

## 2. encbench

### Inputs

```yaml
# bench.yaml
samples: samples/           # every *.mov/*.mp4 here is a sample clip
target_vmaf: 95
models: [vmaf_v0.6.1, vmaf_v0.6.1neg]   # first one drives the search
keep: best                  # keep the passing encode closest to target
flagsets:
  x265-slow-fastdecode:
    args: [-g, "600", -keyint_min, "600", -c:v, libx265, -preset, slow, -tune, fastdecode]
    quality: {flag: -crf, min: 10, max: 35, step: 0.1}
  nvenc-p7-uhq:
    args: [-pix_fmt, yuv420p10le, -rc, vbr, -c:v, hevc_nvenc, -preset, p7, -tune, uhq]
    quality: {flag: -cq:v, min: 20, max: 45, step: 0.1}
  svtav1-p5:
    args: [-c:v, libsvtav1, -preset, "5"]
    quality: {flag: -crf, min: 20, max: 63, step: 1}
```

`quality` falls back to per-encoder defaults: x264/x265 `-crf`
0–51 in steps of 0.1, SVT-AV1 `-crf` 1–63 integers, NVENC `-cq:v`,
VideoToolbox `-q:v` where *higher* means better. Each default
records which direction is "better", so the search works for all of
them.

### Search

VMAF falls, more or less monotonically, as the quality number rises
(x265's CRF). The data from the H.265 post shows two wrinkles: NVENC
has flat plateaus where several CQ values produce the same bitstream,
and there's a little noise. The goal for each (sample, flagset) is the
*worst* quality setting, meaning the smallest file, whose VMAF is
≥ the target:

1. **Seed** from an earlier result for the same flagset on a
   different sample, or from a similar flagset (same encoder) on this
   sample. Otherwise start at the middle of the range.
2. **Bracket**: step outward from the seed until one point passes and
   one fails. If a range bound still passes or fails, record that and
   stop.
3. **Narrow**: interpolate between the bracket's (q, VMAF) points
   to pick the next q (regula falsi), falling back to bisection if
   interpolation stalls. Round to `step`. Stop when the bracket is
   one step wide.
4. **Confirm**: the answer is the passing end of the final bracket.
   If two neighbouring q values produce byte-identical files (a
   plateau), we record that and move on.

That's typically 5–8 encodes per (sample, flagset) instead of the
50–100 a linear sweep at 0.1 needs.

### Caching and incrementality

Every encode is keyed by a hash of (the sample's identity, the
canonical args, the quality flag and value, and the VMAF model
list). Before encoding, the search looks the key up in the results
log and reuses whatever it finds. So:

- adding a flagset runs only that flagset, on every sample;
- adding a sample runs every flagset on only that sample, using
  the other samples' answers as seeds;
- renaming a flagset reruns nothing, since the key comes from its
  args, not its name. Editing its args makes a new flagset.

### Measurement

- Timing: wall, user and sys time from `os.ProcessState`
  (`SysUsage` → rusage), plus max RSS. Encodes run one at a time so
  timings are comparable. VMAF scoring runs in parallel with
  `n_threads`, but only between encodes, never alongside one.
- VMAF uses `libvmaf=log_fmt=json:log_path=…:model=version=…`,
  parsed from the JSON log, never by scraping stderr. Both streams get
  `setpts=PTS-STARTPTS` so frames line up. We record the mean,
  harmonic mean and the 1st-percentile frame score. The
  1st-percentile score catches the "one ugly second" that a mean
  hides, which is one of the ways VMAF misleads.
- Encodes below the reference resolution are scaled back up
  (bicubic) before scoring. 2160p samples use the `vmaf_4k` model.
- Hosts are recorded. Timings from different machines are compared
  only with themselves.

### Output

- `results.csv`: append-only, one row per encode. Columns: `sample`,
  `encoder`, `flagset`, `args`, `quality`, `key`, `bytes`, `kbps`,
  `wall_s`, `user_s`, `sys_s`, `max_rss_kb`, `vmaf`, `vmaf_hmean`,
  `vmaf_p1`, `vmaf_neg`, `ffmpeg_version`, `host`, `time`. It's one
  file for everything, with sample and encoder as the leading
  columns, which keeps cross-sample queries easy.
- `best.csv`: regenerated on every run, one row per
  (sample, flagset) holding the answer. Sorted by sample, then size.
- `keep/<sample>/<flagset>.q<quality>.mp4`: the best encode of each
  (sample, flagset), kept for viewing by eye. Every other encode is
  deleted once it has been scored. `encbench prune` deletes keepers
  whose flagset or sample has gone from the config.
- `encbench report` prints `best.csv` as a Markdown table (pipe
  syntax, as in the blog post).

Later: a small static HTML page for A/B-comparing keepers at the
same timestamp, and distributing encodes over the same Temporal
workers.

---

## Order of work

1. **Foundation.** Split into packages, add error propagation and
   tests, merge the two rebuild checks into one (which fixes the
   missing-output crash), add `MkdirAll` and atomic outputs. No
   behaviour change otherwise.
2. **encbench**, which only needs `probe` and `ffmpeg` from step 1:
   config, runner, VMAF JSON, results log/cache, search, keepers,
   reports. It's independent of steps 3–7, so it can be built in
   parallel.
3. **Layered config**, with the built-in defaults moved into it. Add
   `plan`.
4. **Build keys and state.json**, with keyed output names.
5. **Posters**, then **seek thumbnails** (the manifest
   post-processor).
6. **Publish**: local directory first, then S3.
7. **Temporal**, with explicit approval for the SDK.

Each step is its own PR. Steps 3–6 stack on step 1, and step 2 is
based on step 1.
