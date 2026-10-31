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
   in the `videoencoder.yaml` of the video's own directory, a sidecar
   `<name>.video.yaml` next to the sources, or both. If both exist,
   the sidecar is merged on top of the project entry. A `videos:`
   entry in a parent directory's file defines a different video, one
   that lives in that directory, so it doesn't apply.
4. Command-line overrides (`--outdir`, `--streamdir`,
   `--thumbnails`).

The built-in defaults sit under layer 1 so it works with no config at
all. They were the hard-coded ladder and are now
`internal/config/default.yaml`, embedded in the binary. A video's
directory is where its sidecar or project entry lives. For a bare
`videoencoder foo.mov`, it's the first file's directory, and the video
is named after that file.

**Merge rules.** Maps merge key by key, recursively. Scalars and lists
replace. An explicit `null` deletes an entry of a map keyed by name
(`sources`, `encoders`, `renditions`, `videos`, `publish.targets`);
anywhere else it changes nothing, since YAML reads a section left
empty (`tune:` with its contents commented out) as null, and erasing
a fixed section is never what that meant. Anything a lower
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
that one. Command-line flags are only for one-off overrides and for a
bare `videoencoder foo.mov`; they're never needed to re-render.
`videoencoder plan` prints the merged config, with the layer each
value came from as a line comment, plus every rendition and whether
it would be rebuilt and why. That's the tool for answering "why did
this rebuild?"

Unknown keys are an error, using strict decoding of each layer. A typo
in a project override should fail, not do nothing.

Credentials are never stored in config. S3 keys come from the usual
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` variables or from
`~/.aws/credentials` with a named profile.

### Planning and incremental rebuilds

`plan` turns (merged config, probe data) into a list of jobs. Each
job has:

- a **slot**, saying what the output is for: `<source label>/<rendition>`
  (e.g. `hdr/av1-2160`), `audio`, `poster` or `thumbs`;
- a **build key**: SHA-256 over a schema version, the source's identity
  and every ffmpeg argument except the input path, so moving a source
  doesn't rebuild anything. Source identity is its size, a hash of its
  first and last MiB (`internal/fileid`) and its mtime in seconds. The
  mtime makes a re-export of identical size rebuild, which errs toward
  rebuilding when in doubt;
- an output named after the slot and the first 10 hex digits of the
  key: `av1-2160.hdr.7c3e1a09bd.mp4`, `audio.<key>.mp4`,
  `poster.<key>.jpg`, `thumbs.<key>/`.

Putting the key in the filename means a changed setting produces a
*new* file instead of overwriting the old one. That helps S3: an
unchanged rendition keeps its name and content, so the sync skips it,
and nothing already published is ever overwritten in place.

Outputs are written to a `.partial` name (or a `.partial` directory
for image sequences) and renamed when ffmpeg succeeds. So **an output
is up to date exactly when the file with its key-name exists**, and
there are no per-output status files: the old `MaybeExecute` and
`IsEncodeNeeded` checks are gone, and with them the crash on a
`.status` file whose output was missing.

Each video keeps `<name>.state.json` in its outdir. It records, per
slot, the key last built there, plus the list of files last written
to the stream directory. That's used for two things:

- explaining a rebuild in `plan`: *new*, *source changed*, *settings
  changed* or *output file is missing*;
- deleting exactly what a rebuild replaces: the previous file for a
  rebuilt slot, the files of slots no longer in the config, and stream
  files no longer produced. Only files this video's manifest recorded
  are ever deleted, so videos can safely share an outdir or streamdir.
  The deletions happen only after the new stream is complete.

Packager writes each input to the stream directory under its own name,
with an HLS playlist named to match (`playlist_name=`). So the stream's
contents are known exactly, without guessing packager's
`stream_N.m3u8` numbering.

The ffmpeg version is deliberately **not** part of the key. A
`brew upgrade` shouldn't re-render the whole library. `--rebuild` takes
a glob over slots to force rebuilds: `*` for everything, `av1-*` for
every source's AV1, `sdr/h264-1080`, or `poster`.

The single-decode, multi-output ffmpeg invocation stays. Every stale
rendition of one source is built in one ffmpeg process, because
decoding (especially 6K H.265) is a large share of the cost.

### Posters

The "album" still image. For each tone present, we write
`poster-{sdr,hdr}-<width>.<key>.<format>` at each configured width up
to the source's width, for example `poster-sdr-640.<key>.avif` and
`poster-hdr-1920.<key>.avif`:

```yaml
poster:
  offset: 1s            # seconds, "1.5s", or "MM:SS.ms"
  widths: [320, 640, 1280, 1920, 3840]
  formats: [avif, jpg]  # also webp
```

- The frame is found by seeking before the input (`-ss` before `-i`),
  which is frame-accurate when transcoding and doesn't decode the 100 GB
  that precede it. It's decoded once, and each variant is one output of
  the same ffmpeg process, so posters get the same build keys,
  atomic writes and cleanup as renditions. An offset past the end of a
  source is an error.
- SDR posters come in every configured format. AVIF uses libaom with
  `-still-picture 1`.
- Plain HDR posters are AVIF only, 10-bit, tagged PQ/BT.2020, and are
  made only with `poster.hdr: true`. Gain-mapped posters (below)
  replaced them: checked by eye on 2026-10-24, the gain-mapped and
  plain HDR posters looked alike on an HDR display, but on an SDR
  display the browser's own tone mapping made the plain HDR AVIF too
  contrasty, while the gain-mapped ones matched the SDR poster.
- The source's own width is always one of the widths, alongside the
  configured ones that fit.
- **Fixed names.** The best poster of each width and format,
  gain-mapped where there is one and SDR otherwise, is also copied to
  `poster-<width>.<format>`. The full-resolution one is copied to
  `poster.<format>` as well. Pages can use these names without reading
  `poster.json`, which records each alias against the image it copies.
  They aren't key-named, so they're published with `no-cache`.
- SDR comes from the first SDR source. If there's no SDR source, it's
  tone-mapped from the HDR one (`zscale` + `tonemap=hable`), so every
  video has an SDR poster.
- `poster.json` lists every variant with its file, tone, format, width
  and height, so a page can build `<picture>`/`srcset` markup without
  guessing names. For pages that still use the single poster earlier
  versions wrote, `thumbnail.jpg` is a copy of the largest SDR JPEG
  that's at most 1920 wide.

**Gain-mapped posters.** With an HDR source, each width also gets
`poster-gainmap-<width>.<key>.{jpg,avif}` (`poster.gainmap_formats`,
default both). These are SDR images that HDR displays brighten to the
HDR rendition. They're listed in `poster.json` with tone `gainmap`,
alongside the separate SDR and HDR posters.

- The inputs come from ffmpeg at the poster offset and size. The SDR
  base is 8-bit RGB from the SDR source, or tone-mapped from the HDR
  one. The HDR rendition is converted to linear float with BT.709
  primaries, scaled so 203 nits (BT.2408 reference white) is 1.0,
  matching SDR white.
- **JPEG, in pure Go** (`internal/gainmap`):
  - The gain map is single-channel, computed on luminance as
    `log2((Y_hdr + 1/64) / (Y_sdr + 1/64))`, normalized to its range
    and quantized to 8 bits.
  - Both images are encoded with `image/jpeg` and joined by an MPF
    index.
  - The metadata is written twice: as ISO 21496-1 binary (a version
    marker in the primary, parameters in the gain map image, laid out
    as libultrahdr writes it) and as hdrgm XMP (the container directory
    in the primary, parameters in the gain map). Readers that know
    either form work, and readers that know neither see an SDR JPEG.
  - Checked against libultrahdr's `ultrahdr_app`: it recognizes the
    file and reads the same parameters from either form alone. Its
    linear decode reproduces the HDR luminance we computed the map from
    to within 8%.
- **AVIF:** ffmpeg writes SDR and HDR (PQ/BT.2020, 16-bit) PNGs, and
  libavif's `avifgainmaputil combine` makes the file. That tool is
  needed only when `avif` is in `gainmap_formats`, and its absence is
  reported before any encoding starts.
- **Settings, measured.** On a real HDR frame (Big Four, 1280×720,
  sunlit snow 1.8 stops over SDR white), I decoded each variant to
  linear light (libultrahdr for JPEG, `avifgainmaputil tonemap` for
  AVIF) and measured luminance error in stops against the true HDR
  frame:

  | variant | bytes | mean | p99 |
  |:--|--:|--:|--:|
  | JPEG base 100, map 100, full size (method floor) | 1534695 | 0.017 | 0.097 |
  | JPEG base 85, map 85, full size | 465487 | 0.103 | 0.436 |
  | **JPEG base 85, map 70, full size** (chosen) | 391542 | 0.106 | 0.450 |
  | JPEG base 85, map 70, ½ size | 298324 | 0.127 | 0.567 |
  | JPEG base 85, map 70, ¼ size | 271604 | 0.141 | 0.653 |
  | AVIF q75, map 75, 3-channel (libavif default shape) | 478194 | 0.073 | 0.279 |
  | **AVIF q55, map 50, 1-channel** (chosen) | 200881 | 0.144 | 0.607 |
  | AVIF q60, map 60, 1-channel, ½ size | 185231 | 0.148 | 0.657 |
  | plain HDR AVIF poster (for comparison) | 66174 | 0.183 | 0.779 |

  Most of the error comes from compressing the SDR base, which is
  the loss any JPEG of the scene has anyway. So the bases are set to
  match the plain SDR posters' size (JPEG quality 85, AVIF `-q 55`),
  and the map's own quality barely matters. Downscaling the map does
  matter: with a global tone curve the gain follows image detail, so a
  smaller map smears it, and maps are kept full size. A single-channel
  map costs nothing in accuracy. Every chosen variant is more accurate
  than the plain HDR AVIF. At 1280 wide that's about 390 KB for JPEG
  (vs 262 KB plain SDR) and 200 KB for AVIF (vs 113 KB).
- Both formats fully apply the gain at a display headroom of 4 stops
  (16× SDR white), libavif's default. Viewers scale the gain by the
  fraction of that headroom the display has. Advertising an image's
  full, sometimes 7+ stop, range instead would make it look dim on
  real HDR displays.

### Seek thumbnails

Shaka Packager 3.9 has no thumbnail support. The old
`--add_thumbnail_adaptation_set` flag isn't a packager flag, so
`--thumbnails` made packager fail. Thumbnails are now a separate output
plus a post-processing step, on by default:

```yaml
thumbnails: {enabled: true, interval: 5, height: 180, columns: 5, rows: 5}
```

1. ffmpeg, in the same process as the first source's renditions so the
   decode is shared, runs `select='isnan(prev_selected_t)+gte(t-prev_selected_t,5)',
   scale=…:180,format=yuvj420p,tile=5x5` into `thumbs.<key>/tile-%d.jpg`.
   `select` is used rather than `fps=1/5` because `fps` writes nothing
   for a video shorter than one interval. Each tile covers 25 × 5 s =
   125 s.
2. DASH: before `</Period>` in `dash.mpd` we insert an
   `AdaptationSet contentType="image" mimeType="image/jpeg"` with a
   `SegmentTemplate` (`media="thumbs.<key>/tile-$Number$.jpg"`,
   `duration="125"`, `startNumber="1"`), one `Representation` whose
   `width`/`height` are the whole grid's, and `EssentialProperty
   schemeIdUri="http://dashif.org/thumbnail_tile" value="5x5"`. This
   is the DASH-IF IOP form that Shaka Player and dash.js read. It's a
   text insertion, not an XML round trip, since `encoding/xml` would
   mangle packager's namespaces; the tests check that the result still
   parses.
3. HLS: we append `#EXT-X-IMAGE-STREAM-INF` to `hls.m3u8` and write
   `thumbnails.m3u8`, an `#EXT-X-IMAGES-ONLY` playlist with
   `#EXT-X-TILES:RESOLUTION=320x180,LAYOUT=5x5,DURATION=5.000` per
   tile. That's the Roku/Apple image-playlist form, which Shaka Player
   also reads.

4. WebVTT: `thumbnails.vtt` has one cue per thumbnail, pointing into
   its tile with a `#xywh=x,y,w,h` fragment, the form Video.js, Plyr
   and most players without DASH/HLS image-track support use. The last
   cue ends at the end of the video.

### Packaging

Same as now (one shaka-packager run over every rendition plus audio),
with two changes: packager writes into a staging directory, and the
thumbnail post-processor runs before publish. Audio comes from the
first source, which is fine since the sources differ only in color.

Packager rewrites every output on every run, and its output isn't
byte-deterministic on its own. I tested this on 2026-10-24 with packager
3.9 and unchanged inputs:

- Every media file differs in exactly 6 bytes: the wall-clock creation
  and modification times in `mvhd`/`tkhd`/`mdhd`. After packaging,
  `internal/mp4.ZeroTimes` sets those to zero in place, walking the
  boxes with no new dependency and moving nothing. With that, unchanged
  renditions are byte-identical across runs, and an end-to-end test
  checks it with real packager runs.
- `dash.mpd` assigns AdaptationSet ids in thread order, so they
  shuffle between runs. Manifests are tiny and always uploaded last
  anyway, so that's left alone.

Packager runs with `--quiet`; its per-file INFO logging buried
everything else.

### Publishing

Publish targets are a local directory or an S3-compatible bucket
(`github.com/minio/minio-go/v7`, approved 2026-10-24, pure Go):

```yaml
publish:
  to: [seaweed]                     # used after every render; --to overrides
  targets:
    www:     {dir: /srv/www/videos/{name}}
    seaweed: {s3: {bucket: video, prefix: "{name}/"}}   # endpoint: "" means $S3_ENDPOINT_URL
```

Credentials never go in config. They come from `AWS_ACCESS_KEY_ID` /
`AWS_SECRET_ACCESS_KEY`, or from `~/.aws/credentials` (`s3.profile`).

After rendering, or on `videoencoder publish`, the files recorded as
the video's stream are synced to each target:

- A file is uploaded only if it's missing or different. Files up to
  16 MiB (manifests, playlists, posters, tiles) are compared by MD5,
  which is the ETag for single-part uploads on SeaweedFS, MinIO and AWS
  alike. Larger files are compared by name and size. Their names carry
  the build key and packaging is deterministic, so same name + size
  means same content, and we never hash gigabytes on every publish.
- **Manifests go last** (`dash.mpd`, `hls.m3u8`, `poster.json`), after
  everything they reference, so a player never sees a manifest that
  points at a missing object.
- Key-named objects get `Cache-Control: public, max-age=31536000,
  immutable`; manifests and other fixed names get `no-cache`. Each
  object gets its proper `Content-Type`.
- Nothing is deleted by default. `--prune` deletes objects under the
  target's prefix that aren't part of the current stream, but only
  after the new manifests are up, and only if the target's `dir` or
  `prefix` contains `{name}`. A prefix shared by several videos can't
  be pruned by any one of them.

Tested against SeaweedFS (2026-10-24) with a 6K slice. Changing only the
4K AV1 quality re-encoded that one rendition and uploaded 4 of 46
files: the new media file and its playlist, plus the two manifests.
`--prune` then removed exactly the old pair. Re-running with no changes
uploads only `dash.mpd`, whose AdaptationSet ids packager shuffles.

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
samples: samples/           # every video file here is a sample clip
workdir: work               # results.csv, best.csv, keep/, tmp/
target_vmaf: 95
min_ciede2000: 0            # optional color limits, see Measurement
min_chroma_psnr: 0
vmaf_threads: 4             # libvmaf n_threads
vmaf_jobs: 3                # scorings run at once
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
10–40 in steps of 0.1, SVT-AV1 and libaom `-crf` 10–63 in whole
steps, NVENC `-cq:v`, and VideoToolbox `-q:v`, where *higher* means
better. Each default records which direction is "better", so the
search works for all of them. Each also has a typical answer to start
from (`quality.seed`).

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
4. **Answer**: the passing end of the final bracket. Plateaus need no
   special handling, because the bracket narrows through them like
   anywhere else.

Searches advance in rounds. Each round, every unfinished search asks
for one encode. The encodes run one after another, then all of them
are scored in parallel. In the tests (a 300-step CRF range) a search
with no seed took 7 encodes, and seeded ones took 3–4.

### Sample sizes and HDR

`scales: [1080]` tests every sample at those heights. A taller sample
is downscaled once (Lanczos, ProRes 422 HQ, video only, color tags
kept) into `work/samples/<name>@<height>p.mov`. A sample no taller
than a scale is used as it is, so an existing 1080p sample keeps its
results. A copy's identity is derived from its source's identity and
the height, not from the copy's bytes, so remaking it doesn't
invalidate anything. Large originals (8K ProRes) are then decoded only
once, and VMAF runs at sizes its models were trained for.

HDR (PQ/HLG) samples are encoded as they are, but both sides are
tone-mapped to SDR (`zscale` + `tonemap=hable`) before scoring.
VMAF and CIEDE2000 are only calibrated for SDR, so this is a proxy.
It still catches artifacts and color shifts. An HDR/SDR pair of the
same footage gives a check on how far the proxy can be trusted.

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
- a pair the log already settles is skipped outright: one with a
  passing setting whose next step fails, or a pass or fail at the end
  of the range. Searching it again would start from a seed that may
  have moved (seeds come from other samples' answers), and a different
  path needs encodes the log doesn't have.

### Measurement

- Timing: wall, user and sys time from `os.ProcessState`
  (`SysUsage` → rusage), plus max RSS. Encodes run one at a time so
  timings are comparable. VMAF scoring runs in parallel with
  `n_threads`, but only between encodes, never alongside one.
- VMAF uses `libvmaf=log_fmt=json:log_path=…:model=version=…`,
  parsed from the JSON log, never by scraping stderr. Frames are paired
  by position (`settb=AVTB,setpts=N` on both), not by timestamp:
  Matroska rounds timestamps to the millisecond, and pairing a 30 fps
  `.mkv` against an `.mp4` by timestamp compared every third frame with
  the wrong one, which cost about 12 VMAF points. For the `.mov`
  samples so far, both methods give identical scores. We record the mean,
  harmonic mean and the 1st-percentile frame score. The
  1st-percentile score catches the "one ugly second" that a mean
  hides, which is one of the ways VMAF misleads.
- **VMAF ignores chroma entirely.** A 20° hue rotation of a test
  pattern still scores VMAF 99.9. So the same libvmaf pass also
  computes per-plane PSNR (`psnr_y/cb/cr`) and CIEDE2000, where higher
  is better: about 51 for a 2° hue shift, 43 for 5°. `min_ciede2000`
  and `min_chroma_psnr` make color error fail an encode. This is aimed
  at the AV1 color cast in the water of the North Cascades video, which
  more quality only partly fixes. Both inputs are converted to
  `yuv444p10le` before scoring, so chroma subsampling loss counts too.
- CIEDE2000 costs about 9× as much as VMAF per frame. On a 3 s test,
  VMAF took 3.1 s, both VMAF models 5.5 s, and adding CIEDE2000 28 s.
  So it runs in a second branch of the same filtergraph, on every
  sixth frame at half resolution, the chroma's own resolution in
  4:2:0, for about 1/24 the cost. A color cast is broad and
  persistent, so sampling doesn't miss it. Chroma PSNR is nearly free
  and stays on every frame.
- **Local color shift.** Frame averages miss a color cast confined
  to part of the picture. On Pyramid Creek, every SVT-AV1 encode shifted
  the whitewater toward magenta and yellow, but its frame-mean CIEDE2000
  matched x265's.
  - So a third branch of the scoring graph area-averages both sides
    into 64×64 blocks (AV1's superblock size), as 16-bit RGB on every
    frame. Go converts each block to ICtCp and takes the ΔE_ITP
    chroma error (ITU-R BT.2124: hue and saturation, ignoring
    lightness, scaled so 1 is about the smallest visible difference).
  - HDR is measured as HDR: the blocks skip the tone map that VMAF
    and CIEDE2000 need, and are read as PQ BT.2020; HLG is converted to
    PQ first. SDR is read as BT.1886 at 100 nits and converted to
    BT.2020, so both land on one scale. A tone map compresses
    highlights, which is where whitewater's cast was, and hid much of it.
  - Two measures, each at p99 and p99.9:
    - **Shift** (`color_shift_*`) is each block's error.
    - **Flicker** (`color_flicker_*`) is how far a block's error strays
      from its own average over 16-frame windows. What made the AV1
      cast annoying was that it came and went, in rectangular patches,
      frame by frame. Sixteen frames spans several of SVT-AV1's
      hierarchical layers (its mini-GOP at preset 5 is 32 frames),
      whose cycle of quantizers is the likely driver; 8 and 32 also
      worked, 16 separated the encoders most.
      Comparing only adjacent frames doesn't separate the encoders,
      since neighbors at nearby levels err alike.
  - Checked against the Pyramid Creek HDR keepers as judged by eye, at
    p99.9:

    | | shift | flicker |
    |:--|--:|--:|
    | VideoToolbox | 1.13 | 0.68 |
    | x265 medium | 1.59 | 0.95 |
    | x265 slow | 2.54 | 1.12 |
    | every SVT-AV1 variant (visible cast) | 2.92–4.50 | 1.32–1.81 |
    | every SDR encode | ≤ 1.79 | ≤ 0.98 |

  - **Later finding (2026-10-26): neither works as a pass criterion.**
    Across 21 clips, flicker reads camera and subject motion as flicker
    (a handheld cat walk needed VMAF 98.6 to pass), and shift varies with
    SVT-AV1's preset on fast motion (p7 and up sit at a constant 3–5 on
    two home videos even at CRF 10). Both stay in every row, and reports
    flag an answer whose shift or flicker is at least 1.5× its sample's
    median and above it by a margin, as something to look at. Pyramid
    Creek's AV1 cast is flagged that way. The suggested limits below
    are from before this and are now 0 in practice.
  - `max_color_shift` (suggested 2.7) and `max_color_flicker`
    (suggested 1.25) fail encodes over the limits at p99.9. They come
    from one clip, so treat them as starting points.
  - Each row records its `scorer` version, and rows from older scorers
    are ignored for answers, seeds and settled checks. The encode key
    includes the scorer, so they're re-encoded rather than reused. v4
    is this measure. v3's CIELAB block metric is void: the raw block
    output ran at a constant frame rate, which, given the renumbered
    microsecond timestamps, kept only about three frames. Block
    outputs now use `-fps_mode passthrough`.
  - Block images go through 16-bit 4:4:4 before RGB. swscale's direct
    8-bit YUV to RGB conversion is imprecise: it measured a lossless
    8-bit copy of a clip at 2.71 (the same at 10-bit: 0.66), which
    failed every 8-bit encode. Through 16-bit, both read 0.69. Scorer v6.
- Each score is logged the moment it finishes, so an interrupted
  round loses only the scorings still running.
- A sample can name another file to be scored against (`references`):
  an H.265 export against the ProRes master it was rendered from, say.
  Scored against itself, an export looks better than it is, since the
  detail its own compression removed isn't counted as lost. Scaled
  copies are paired with the reference's copies, the pair keys the
  encodes, and rows record the `reference`.
- Encodes below the reference resolution are scaled back up
  (bicubic) before scoring. 2160p samples use the `vmaf_4k` model.
- Hosts are recorded. Timings from different machines are compared
  only with themselves.

### Output

- `results.csv`: append-only, one row per encode. Columns: `sample`,
  `encoder`, `flagset`, `args`, `quality_flag`, `quality`, `key`,
  `flagset_key`, `bytes`, `kbps`, `wall_s`, `user_s`, `sys_s`,
  `max_rss_kb`, `vmaf`, `vmaf_hmean`, `vmaf_p1`, `vmaf_neg`,
  `psnr_y`, `psnr_cb`, `psnr_cr`, `ciede2000`, `ffmpeg_version`,
  `host`, `time`. It's one file for everything, with sample and
  encoder as the leading columns, which keeps cross-sample queries
  easy. Encodes are video only (`-map 0:v:0 … -an`), so sizes don't
  include audio, which the March 2025 numbers did.
- `best.csv`: regenerated on every run, one row per
  (sample, flagset) holding the answer. Sorted by sample, then size.
  It's derived from the log under the *current* pass criteria, so
  changing `target_vmaf` re-ranks results without re-encoding
  anything that was already measured.
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

## 3. videoencoder tune

Combines the two: encbench's search and scoring, applied to the video
being encoded, varying only the quality setting.

- **Sample.** `tune.duration` (default 8 s) from `tune.start` (default
  0) of the first SDR source, or the HDR source if there isn't one.
  It's cut by stream copy, so it starts at a keyframe.
- **Anchors.** Per encoder, the shortest and tallest renditions as
  filtered for the source (`tune.anchors: 3` adds the middle one). Each
  is encoded from the sample with its real arguments, but no bitrate
  caps, while bench's search (regula falsi plus bisection) varies the
  quality. The search starts from the configured quality, uses the
  encoder's bench range, and steps no finer than 0.5.
- **Scoring at viewing size**, as Netflix's per-title encoding does.
  Renditions up to 1080p are compared with a lossless (FFV1) 1080p
  reference of the sample, and taller ones with a 2160p reference using
  the 4K model, never above the source. HDR is tone-mapped to SDR on
  both sides first, since VMAF and CIEDE2000 are calibrated for SDR.
- **Interpolation.** The other renditions get quality linear in log
  height between anchors, held flat outside them, rounded to the step.
  Bitrate is interpolated linearly in log bitrate.
- **Bitrate caps.** For encoders with `cap_bitrate`, `bitrate` is set
  to the tuned encode's average (two significant figures). The existing
  1.5× maxrate/bufsize then follows from it. The sample may be calmer
  than the rest of the video, so `tune.start` is worth pointing at a
  busy part.
- **Output.** `quality`/`bitrate` are written into
  `<name>.video.yaml` by editing the YAML node tree, so other settings
  and comments survive. Each value gets a comment such as `tuned
  2026-10-24: VMAF 95.1 (target 95)` or `interpolated by tune …`.
  A sidecar is created if needed, with `sources` for ad hoc videos.
  Tuning never renders; the next render picks the values up like any
  other setting.

## Order of work

1. **Foundation.** Split into packages, add error propagation and
   tests, merge the two rebuild checks into one (which fixes the
   missing-output crash), add `MkdirAll` and atomic outputs. No
   behavior change otherwise.
2. **encbench**, which only needs `probe` and `ffmpeg` from step 1:
   config, runner, VMAF JSON, results log/cache, search, keepers,
   reports. It's independent of steps 3–7, so it can be built in
   parallel.
3. **Layered config**, with the built-in defaults moved into it. Add
   `plan`.
4. **Build keys and state.json**, with keyed output names.
5. **Posters**, then **seek thumbnails** (the manifest
   post-processor), then deterministic packaging.
6. **Publish**: local directory and S3.
7. **Temporal**, with explicit approval for the SDK.

Steps 1–6 are done (PRs #5–#11).

Each step is its own PR. Steps 3–6 stack on step 1, and step 2 is
based on step 1.
