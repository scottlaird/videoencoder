# encdb

An in-browser explorer of encbench's results. Every logged encode, from every machine, goes into
one parquet file that [DuckDB-WASM](https://duckdb.org/docs/api/wasm/overview) queries in the
page; nothing runs on the server but a file server.

```sh
go run ./cmd/encdb serve           # http://127.0.0.1:8766/facets.html, on the built-in snapshot
go run ./cmd/encdb build -config cmd/encdb/encdb.yml -o cmd/encdb/web/encodes.parquet
```

`serve -data file` serves another table instead of the snapshot built into the binary.

## Pages

- `facets.html`: the explorer. Filters down the left (ticks within a block are ORed, and each
  block counts what the others leave), a chart of file size against encode time at VMAF 95 (one
  line per encoder and machine, geometric means over the clips), the size/speed frontier, and a
  cost model for the frontier's points.
- `chart.html`: a chart drawn from one canned SQL query.
- `index.html`: a SQL box over the table. `answers` is a view of each search's answer: the
  smallest passing file at VMAF ≥ 95.

The pages load DuckDB-WASM, ECharts and js-yaml from jsDelivr.

## The table

`encdb build` reads the results logs and encbench servers its config lists (see `Config` in
`config.go`, and `encdb.yml`, the blog's), keeps each encode once, and adds the columns the
pages filter on:

- what ran it: `machine`, `hardware`, and `device` (the CPU for software encoders, else the GPU);
- what the flagset is: `family`, `software`, `preset`, `bit_depth` (the setting), `keyframes`,
  `variant`;
- what was encoded: `clip`, `res`, `codec`, and `source_bit_depth`, `input_bit_depth` and
  `out_bit_depth` (source and input depths come from running ffprobe over the config's `probe`
  directories);
- `std_clip` and `std_scorer`: whether it's one of the clips, scored the way, that the charts use.

Then come results.csv's own columns, with `/api/results`'s nulls (empty strings are null too, and
results.csv's `keyframes` is `out_keyframes`), and `log`, the source the row was read from.

A config's `retime` entries give a host's answers the times of a later timing run whose files are
identical. The blog's re-times render1's Quick Sync answers from after its second DIMM went in.

The snapshot's `host` and `config` columns name the machines and paths it was built on.
