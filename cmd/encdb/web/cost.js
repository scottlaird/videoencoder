// The cost of an encode and of streaming it, for the frontier table.
//
//   cost(point, plays) = vcpu_s * $/vCPU-s            (encode, once)
//                      + GB * $/GB-month * months     (storage, once)
//                      + plays * GB * $/GB            (delivery, per play)
//
// vcpu_s is the encode's CPU time (user + sys, in vCPU-seconds) and GB its
// size, both geometric means over the clips, so these are costs for a
// typical clip. Encoding and storage are both fixed costs of keeping the
// file; only delivery grows with plays. Units are decimal: 1 GB = 10^9 bytes.
//
// Hardware encoders are priced like software, by the CPU time ffmpeg logged
// (mostly decoding the source); the machine with the encoder isn't modeled.

// Price sets: $ per vCPU-hour, $ per GB delivered and $ per GB-month
// stored, at list prices (2026-11; video-blog/cost_model.py has the
// compute and CDN sources). Storage is the first tier of each provider's
// standard class in a US region.
const GIB = 1e9 / 2 ** 30;  // $/GiB -> $/GB
const S3 = 0.023, S3_IA = 0.0125, GCS = 0.020 * GIB;
export const PRESETS = [
  { id: 'aws', label: 'AWS: c7i on-demand + CloudFront + S3', cpu: 0.0446, gb: 0.085, store: S3 },
  { id: 'aws-spot', label: 'AWS: c7i spot + CloudFront + S3', cpu: 0.0446 * 0.38, gb: 0.085, store: S3 },
  { id: 'aws-ia', label: 'AWS: c7i spot + CloudFront + S3 Infrequent Access', cpu: 0.0446 * 0.38, gb: 0.085, store: S3_IA },
  { id: 'aws-150tb', label: 'AWS: c7i on-demand + CloudFront at 150–500 TB/mo + S3', cpu: 0.0446, gb: 0.040, store: S3 },
  // C3 highcpu ($0.170/h for 4 vCPUs) has c7i's 2 GB per vCPU. Cloud CDN
  // ($0.08) and Cloud Storage ($0.020/month) bill per GiB.
  { id: 'gcp', label: 'GCP: c3-highcpu on-demand + Cloud CDN + Cloud Storage', cpu: 0.170 / 4, gb: 0.08 * GIB, store: GCS },
  // Bunny Storage: $0.01/GB-month for one region (more regions cost more).
  { id: 'bunny', label: 'AWS c7i spot + Bunny CDN + Bunny Storage', cpu: 0.0446 * 0.38, gb: 0.01, store: 0.01 },
  // Your own machines and disks: only storage costs anything.
  { id: 'own', label: 'Own hardware: storage only', cpu: 0, gb: 0, store: S3 },
];

// The Mac's CPU-seconds per encode are within 0-20% of a Zen 5 SMT
// thread's (render2, the same x265 encodes): count one as 1.1 vCPU-s.
// Other machines' hardware threads count as one vCPU each.
export const VCPU_PER_CPU_S = { 'M3 Ultra': 1.1 };

// costs(p, prices) -> { encode, store, fixed, play } in dollars for one
// clip: fixed = encode + store, paid once; play per play. prices has cpu
// ($/vCPU-hour), gb ($/GB delivered), store ($/GB-month) and months.
export function costs(p, prices) {
  const gb = p.bytes / 1e9;
  const encode = p.vcpu * prices.cpu / 3600, store = gb * prices.store * prices.months;
  return { encode, store, fixed: encode + store, play: gb * prices.gb };
}

// breakEven(p, base, prices) -> the plays at which p and base cost the
// same, with which side wins past it:
//   { plays, after: 'smaller' } - the smaller file wins past `plays`;
//   { always: p | base } - one is never more expensive;
//   null if they're the same point.
export function breakEven(p, base, prices) {
  const a = costs(p, prices), b = costs(base, prices);
  const dFixed = a.fixed - b.fixed, dPlay = b.play - a.play;
  if (dFixed === 0 && dPlay === 0) return null;
  // Costs cross where dFixed = plays * dPlay.
  if (dPlay !== 0 && dFixed / dPlay > 0) return { plays: dFixed / dPlay };
  // p is cheaper (or as cheap) on both counts, or base is.
  return { always: dFixed <= 0 && dPlay >= 0 ? p : base };
}

// cheapest(pts, prices) -> [{ point, from, to }]: which point is cheapest
// over each range of plays from 0 to Infinity (the lower envelope of the
// cost lines). Ties go to the earlier point in pts.
export function cheapest(pts, prices) {
  const c = pts.map(p => costs(p, prices));
  const cuts = new Set();
  for (let i = 0; i < c.length; i++) {
    for (let j = i + 1; j < c.length; j++) {
      const dp = c[j].play - c[i].play;
      if (dp === 0) continue;
      const n = (c[i].fixed - c[j].fixed) / dp;
      if (n > 0 && Number.isFinite(n)) cuts.add(n);
    }
  }
  const edges = [0, ...[...cuts].sort((a, b) => a - b), Infinity];
  const best = n => {
    let k = 0;
    for (let i = 1; i < c.length; i++) {
      if (c[i].fixed + n * c[i].play < c[k].fixed + n * c[k].play - 1e-15) k = i;
    }
    return k;
  };
  const out = [];
  for (let e = 0; e + 1 < edges.length; e++) {
    const [lo, hi] = [edges[e], edges[e + 1]];
    const k = best(hi === Infinity ? lo * 2 + 1 : (lo + hi) / 2);
    if (out.length && out[out.length - 1].i === k) out[out.length - 1].to = hi;
    else out.push({ i: k, point: pts[k], from: lo, to: hi });
  }
  return out;
}

// Dollar amounts down to a hundredth of a cent with two significant figures.
export function dollars(v) {
  if (v === 0) return '$0';
  if (v >= 1) return '$' + v.toFixed(2);
  return '$' + v.toPrecision(2).replace(/^0/, '0');
}

// Play counts: fractional below 10 (a partial play is real bytes).
export function plays(n) {
  if (n === Infinity) return '∞';
  if (n < 0.1) return n.toPrecision(1);
  if (n < 10) return n.toFixed(1);
  return Math.round(n).toLocaleString();
}
