# memo-2: E3-E7 measurements on the real corpus

Continues memo-1 (E1 CDC, E2 dictionaries). Corpus: 65,499 files / 3.83 GB extracted
layer trees in `~/diskreclaim2/layers` (phone, Termux), `files.csv` with md5+size per
file. Experiment code: `tools/experiments/{e3sim,e5meta,e6entropy}.py` on
`test/store-optimization`. All measurements offline; the zero-read-latency invariant
holds by construction.

## E3 cross-layer similarity clusters — NOT worthwhile

Method: file-md5 set Jaccard between every layer pair (126 layers covered by the CSV,
7,875 pairs), union-find clustering at J >= 0.5.

- 20 pairs J >= 0.9, 61 pairs J >= 0.5, 125 pairs J >= 0.2, 7,669 pairs (97%) below 0.2.
- Largest clusters: 8 layers / 3,388 files; 7 / 1,208; 7 / 567; one 2-layer cluster with
  4,231 files; everything else 2-16 files.
- Whole-file duplicate bytes: 1,189.4 MB (95%) CROSS-cluster vs 62.8 MB within-cluster.

Verdict: images are mostly disjoint; the content they share is universal base files
present across unrelated images, not cluster-specific material. A similarity index has
no clusters worth exploiting, and dictionary samples keyed on clusters do not help (see
E4). CDC chunking (E1) stays the right mechanism for cross-image sharing: it needs no
cluster structure. E3 as an index feature: parked, not worth building.

## E4 representation search (cluster-aware dictionaries) — confirms the E2 park

Method per top cluster: train one zstd dictionary on 150 sampled files (2 KB-500 KB),
eval held-out 25-60 files at zstd -19, net of dictionary bytes.

- cluster c0 (8 layers): dict 9,084 B; held-out 77,670 -> 61,002; net +7,584 (+129/file)
- cluster c1 (7 layers): dict 727 B; net -23,802
- cluster c2 (7 layers): dict 277 B; net -9,350

Verdict: even with similarity-clustered training samples, multi-image dictionaries stay
net-negative or negligible. E3 explains why: cluster-shared content is a small slice of
duplicated bytes. Parked, now with a structural reason, not just a weak first result.

## E5 metadata compression — worthwhile, adopt

Measured on the live `sqfinal/st` store (2 manifests, 48 frame idx JSON files):

- manifests: 1,118,383 B raw -> 174,590 B at zstd -19 (84.4% saved)
- frames: 1,619,295 B raw -> 806,227 B at zstd -19 (50.2% saved)
- total metadata 2.74 MB -> 0.98 MB (64.2% saved, ~1.76 MB on a 2-image store; scales
  with image count)

Read latency: manifests and frame indexes are read at store open and per-image ops, not
per-blob; zstd decompression runs at GB/s, so reads cannot get slower. Adopt: compress
at write, decompress transparently at read. Frame indexes compress only 50% because they
are mostly hex digests; binary framing is a later format change, not now.

## E6 entropy segmentation — no skip-compression win; parked

Method: stratified 2,400-file sample (4 KB-2 MB) from files.csv, zstd -3 ratio per
family and size bucket; corpus weights from all 68,733 rows.

- binary_other: 55,380 files / 6.00 GB, ratio 0.43 (small) / 0.28 (big)
- text: 12,610 files / 143 MB, ratio 0.31 / 0.24
- already-compressed (archives, fonts, images): 743 files / 41.7 MB, ratio ~1.0 / 0.88
- corpus-wide estimate at zstd -3: 6.19 GB -> 1.81 GB (70.8% saved)
- near-incompressible files: 2.6 MB total (0.04% of the corpus)

Verdict: compression is uniformly valuable on this corpus; skipping it would save
almost nothing. Residual value is dictionary-sample hygiene only (train dicts on the
compressible classes). Parked as a feature.

## E7 global optimizer — unchanged, sequenced last

No new measurement. Crash-safety prerequisite already shipped (v0.3.10 durable writes).
Design stays as in store-optimization.md.

## Net position after E1-E7

Worthwhile: E1 CDC chunk dedup (gate remaining: materialize reassembly check so reads
stay exactly as fast) and E5 metadata compression (no gate). Everything else parked with
numbers, not vibes.
