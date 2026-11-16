// Opens DuckDB-WASM with encodes.parquet loaded as the table `encodes`,
// plus the `answers` view. Shared by the demo pages.
import * as duckdb from 'https://cdn.jsdelivr.net/npm/@duckdb/duckdb-wasm@1.29.0/+esm';

// answers: each search's answer at VMAF 95, the smallest passing file for
// a (machine, sample, reference, flagset, scorer). Batched rows are untimed;
// the timed re-encode of the answer sorts first. render1 has two logs of the
// same Quick Sync encodes, before and after its second DIMM: the dual-DIMM
// times win.
export const SETUP = `
CREATE TABLE encodes AS SELECT * FROM 'encodes.parquet';
CREATE VIEW answers AS
SELECT * FROM encodes
WHERE vmaf >= 95
QUALIFY row_number() OVER (PARTITION BY machine, sample, reference, flagset, scorer
                           ORDER BY bytes, timed DESC, log LIKE '%2dimm%' DESC, time DESC) = 1;
`;

// openDb reports progress through status(text) and resolves to
// { conn, rows, mb, newest } once the tables are ready.
export async function openDb(status = () => {}) {
  status('Loading DuckDB…');
  const bundle = await duckdb.selectBundle(duckdb.getJsDelivrBundles());
  const workerUrl = URL.createObjectURL(new Blob([`importScripts("${bundle.mainWorker}");`], { type: 'text/javascript' }));
  const db = new duckdb.AsyncDuckDB(new duckdb.VoidLogger(), new Worker(workerUrl));
  await db.instantiate(bundle.mainModule, bundle.pthreadWorker);
  URL.revokeObjectURL(workerUrl);
  status('Loading encodes.parquet…');
  // Python's http.server doesn't do range requests, so fetch the whole file.
  const buf = new Uint8Array(await (await fetch('encodes.parquet', { cache: 'no-cache' })).arrayBuffer());
  const mb = buf.length / 1e6;  // (the buffer is handed to the worker)
  await db.registerFileBuffer('encodes.parquet', buf);
  const conn = await db.connect();
  await conn.query(SETUP);
  const n = (await conn.query('SELECT count(*) AS n, max(time) AS t FROM encodes')).toArray()[0];
  return { conn, rows: Number(n.n), mb, newest: new Date(Number(n.t)) };
}

// rowsOf turns an Arrow result into plain objects: BigInts become Numbers
// and timestamps Dates.
export function rowsOf(res) {
  const time = new Set(res.schema.fields.filter(f => /^(Timestamp|Date)/.test(String(f.type))).map(f => f.name));
  return res.toArray().map(r => {
    const o = {};
    for (const f of res.schema.fields) {
      let v = r[f.name];
      if (typeof v === 'bigint') v = Number(v);
      if (time.has(f.name) && v !== null) v = new Date(Number(v));
      o[f.name] = v;
    }
    return o;
  });
}
