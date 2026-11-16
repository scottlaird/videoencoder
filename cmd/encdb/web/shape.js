// Turns per-preset aggregate rows from DuckDB into ECharts series: which
// rows make a line, what each line is called, its color and marker, which
// presets are one point (identical files), and which points can't be drawn
// because some clips have no answer.

export const CODEC = { h264: 'H.264', hevc: 'H.265', av1: 'AV1' };

// One color per encoder family (palette.py's, plus green and pink for
// software H.264 and AV1). Software is colored by codec, so libaom shares
// SVT-AV1's color. Checked under simulated protan/deutan/tritan vision: the
// closest pair is about ΔE 6 (OKLab x100), so every family also gets its
// own marker, and lines are labeled.
const SOFTWARE = { h264: '#279b61', hevc: '#d5731f', av1: '#f42aa3' };
const HARDWARE = { NVENC: '#00a0d0', 'Quick Sync': '#9060e8', VideoToolbox: '#9f6c8e' };
const SYMBOL = { NVENC: 'triangle', 'Quick Sync': 'rect', VideoToolbox: 'diamond' };
const OTHER = '#8a8a8a';
// Later members of a color group: dashed and hollow, then dotted.
const VARIANTS = [['solid', ''], ['dashed', 'empty'], ['dotted', 'empty']];

// A line is one combination of these. Its name is family and device, plus
// whichever of the rest it takes to tell it from lines that would
// otherwise share that name.
const LINE_KEYS = ['family', 'device', 'codec', 'variant', 'bit_depth', 'keyframes', 'res'];
const DESCRIBE = [
  ['codec', r => CODEC[r.codec] ?? r.codec],
  ['variant', r => r.variant],
  ['bit_depth', r => r.bit_depth === 'as source' ? 'source depth' : `${r.bit_depth}-bit`],
  ['keyframes', r => r.keyframes],
  ['res', r => r.res],
];

// Line order, which also decides who in a color group is solid: the main
// software encoder before its alternative, the Mac before other machines.
const FAMILY_ORDER = ['x264', 'x265', 'SVT-AV1', 'libaom', 'VideoToolbox', 'NVENC', 'Quick Sync'];
const rank = r => [FAMILY_ORDER.indexOf(r.family) + 1 || 99, r.device === 'M3 Ultra' ? 0 : 1];
const byRank = (a, b) => {
  const [ra, rb] = [rank(a.first), rank(b.first)];
  return ra[0] - rb[0] || ra[1] - rb[1] || a.name.localeCompare(b.name);
};

const colorOf = r => r.software ? (SOFTWARE[r.codec] ?? OTHER) : (HARDWARE[r.family] ?? OTHER);
const symbolOf = (r, v) => {
  const base = r.software ? 'circle' : (SYMBOL[r.family] ?? 'pin');
  const pre = VARIANTS[Math.min(v, VARIANTS.length - 1)][1];
  return pre ? pre + base[0].toUpperCase() + base.slice(1) : base;
};

// Names for line groups [{first}]: family (device), then any described
// attribute that differs among lines sharing that base name.
function nameLines(lines) {
  const base = l => `${l.first.family}|${l.first.device}`;
  const groups = Map.groupBy ? Map.groupBy(lines, base) : lines.reduce((m, l) => m.set(base(l), [...(m.get(base(l)) ?? []), l]), new Map());
  for (const group of groups.values()) {
    const differ = DESCRIBE.filter(([k]) => new Set(group.map(l => l.first[k])).size > 1);
    for (const l of group) {
      const extra = differ.map(([, f]) => f(l.first)).filter(s => s !== 'default');
      l.name = `${l.first.family}${extra.length ? ' ' + extra.join(' ') : ''} (${l.first.device})`;
    }
  }
}

// Presets from fastest to slowest, so a line joins its points in preset
// order whatever the axis: SVT-AV1 and libaom count down (p10, cpu8 fast),
// NVENC up (p1 fast), x264/x265/Quick Sync by name.
const NAMED = ['ultrafast', 'superfast', 'veryfast', 'faster', 'fast', 'medium', 'slow', 'slower', 'veryslow'];
function presetRank(family, preset) {
  const i = NAMED.indexOf(preset);
  if (i >= 0) return i;
  const m = /^(p|cpu)([0-9]+)$/.exec(preset ?? '');
  if (!m) return 0;
  return family === 'NVENC' ? +m[2] : -m[2];
}

// mergeSame folds presets whose files are identical on every clip into one
// point named "faster/slower", at the slower one's time, as part5.py does.
function mergeSame(family, pts) {
  const out = [];
  for (const p of [...pts].sort((a, b) => presetRank(family, a.preset) - presetRank(family, b.preset))) {
    const same = p.files && out.find(q => q.files === p.files);
    if (same) {
      same.label = `${same.label}/${p.preset}`;
      same.x = p.x;
      same.vcpu = p.vcpu;
      same.merged.push(p.preset);
    } else {
      out.push({ ...p, label: p.preset, merged: [p.preset] });
    }
  }
  return out;
}

// shape(rows, expected, units) -> { series, legend, missing, partial, shared }.
// rows: one per (line attributes, preset), with n (clips answered), x, y,
// clips (their names) and files (a signature of their output files).
// expected: res -> how many clips a point needs.
export function shape(rows, expected, units = 's') {
  const lines = new Map();
  for (const r of rows) {
    const key = LINE_KEYS.map(k => r[k]).join('|');
    if (!lines.has(key)) lines.set(key, { first: r, rows: [] });
    lines.get(key).rows.push(r);
  }
  const all = [...lines.values()];
  const complete = r => r.n === expected[r.res];
  // Name every line against all the others (for the notes about lines not
  // drawn), then drawn lines only against each other, so a line that can't
  // be drawn doesn't lengthen the names on the chart.
  nameLines(all);
  nameLines(all.filter(l => l.rows.some(complete)));
  all.sort(byRank);

  const missing = [], partial = [];
  const used = new Map();
  const series = [];
  for (const l of all) {
    for (const r of l.rows) if (!complete(r)) missing.push({ line: l.name, ...r });
    const pts = mergeSame(l.first.family, l.rows.filter(complete));
    // The clips this line can show: those of its best-covered preset.
    l.clips = l.rows.reduce((a, r) => r.n > a.length ? r.clips : a, []);
    if (!pts.length) { partial.push(l); continue; }
    const color = colorOf(l.first);
    const v = used.get(color) ?? 0;
    used.set(color, v + 1);
    series.push({
      name: l.name, type: 'line', smooth: false, color, software: l.first.software, device: l.first.device,
      symbol: symbolOf(l.first, v), symbolSize: 9,
      lineStyle: { type: VARIANTS[Math.min(v, VARIANTS.length - 1)][0] },
      label: { show: true, formatter: '{b}', fontSize: 12, color: 'inherit', position: 'right' },
      data: pts.map(p => ({
        name: p.label || l.first.family,
        value: [+p.x.toFixed(2), Math.round(p.y)],
        vcpu: p.vcpu, bytes: p.bytes,
        tooltip: { formatter: `${l.name}<br/>${p.label || '(no preset)'}: ${p.x.toFixed(1)} ${units}, ` +
          `${Math.round(p.y).toLocaleString()} kb/s<br/>${p.n} clips` +
          (p.merged.length > 1 ? `<br/>${p.merged.join(', ')} give identical files` : '') },
      })),
    });
  }
  // The clips every line (drawn or not) has for at least one preset.
  let shared = null;
  for (const l of all) shared = shared ? shared.filter(c => l.clips.includes(c)) : [...l.clips];
  return { series, legend: series.map(s => s.name), missing, partial, shared: shared ?? [] };
}

// points(series) -> every drawn point, flattened: line, color, software,
// device, preset label, x, y, vcpu (vCPU-seconds) and bytes.
export function points(series) {
  return series.flatMap(s => s.data.map(d => ({
    line: s.name, color: s.color, software: s.software, device: s.device, preset: d.name,
    x: d.value[0], y: d.value[1], vcpu: d.vcpu, bytes: d.bytes,
  })));
}

// frontier(series) -> the size/speed Pareto frontier of the drawn points,
// fastest first: each point that no other point beats on both time and
// bitrate (ties on both count once). Each carries its line, preset label,
// x, y and the tooltip's data item.
export function frontier(series) {
  const pts = points(series);
  pts.sort((a, b) => a.x - b.x || a.y - b.y);
  const out = [];
  for (const p of pts) {
    // Sorted by time, a point is on the frontier if it's smaller than
    // every faster point kept so far.
    if (!out.length || p.y < out[out.length - 1].y) out.push(p);
  }
  return out;
}
