# Store-level optimization: research plan

Assessed Nov 6, 2026 against master `9764de0`. The proposal: stop treating compression
as a file-by-file problem and treat the whole Squish store as one optimization problem.
This document is the assessment and the research workplan. The companion experiment
plan lives on the `test/store-optimization` branch.

## Standing constraints

- Reads never get slower. Squish's materialize/mount path is the product; any
  representation that adds work at read time is rejected, unless the offline optimizer
  materializes the final representation before it goes active. The roadmap already
  states this; it is the first gate on every idea below.
- Gain rule: any storage gain that adds no read latency is worth adopting. The only
  remaining gates are implementation cost and correctness risk, judged per feature.
  There are no percentage kill lines; experiments report absolute bytes and the
  standing rule judges.
- Ingest stays deterministic and cheap. The store is content-addressed; representation
  choices made at ingest must stay stable and reproducible. Representation *search*
  is offline work; ingest keeps a fixed default strategy.

## Mapping the roadmap onto what Squish already has

| Roadmap idea | Squish today | Verdict |
|---|---|---|
| Whole-file dedup (2A) | per-file BLAKE2b content-addressed store, hardlinked at materialize | done, keep |
| Dictionary per directory group | path-sorted deep repack, shared dictionary per group, frame index | the current baseline the roadmap wants to beat |
| Regenerable-artifact elision | on by default, 0.6% measured on the corpus | a slice of "opportunity mining" already shipped |
| Cross-version deltas | `delta` command with min/max file bounds | exists; similarity-indexed base selection would improve it |
| Zero-read-latency invariant | zstd at 200+ MB/s vs 9 MB/s storage reads; offline repack | the invariant every new representation must re-earn by measurement |

## Verdicts per direction

### 1. Similarity-guided compression - PROTOTYPE (experiments E2/E3)
The grouping idea is right: Squish currently groups by path, and structurally similar
files that live in different directories get different dictionaries. Rolling-hash
region fingerprints plus a similarity index (MinHash or banded locality-sensitive
hashing) to find candidate pairs without all-pairs comparison is established technique
(LBFS lineage); the new part for Squish is doing it across the whole store, images
included, and feeding the results to dictionary training and delta-base selection.
Everything probabilistic stays a candidate-narrowing filter; every sharing decision is
verified by exact hash. Cross-image dictionaries are the highest-leverage single idea
in the whole proposal.

### 2. Multi-level dedup - split
- (A) whole-file: done.
- (B) content-defined chunking: the highest expected-value idea in the list. The
  per-file store misses identical regions inside differing files, which is exactly
  what version-bumped binaries and re-layered tarballs contain. FastCDC-style
  chunking is well understood. Design question: a chunk-deduped file must reassemble
  at read; keep the per-file frame index pointing at the chunk list so reads stay
  sequential over that file's own chunks, and measure the materialize/mount cost.
- (C) similarity-based region matching: fold into direction 3's representation
  search; delta-encoding near-identical regions is a candidate representation, not a
  separate subsystem.
- (D) cross-version dedup: already partly covered by `delta`; the similarity index
  improves base selection. Measure before building new machinery.
- (E) cross-image dedup: same mechanism as (B) with a store-wide chunk index.
  Note Docker's registry already dedupes layer blobs, so measure what is left.

### 3. Representation search - PROTOTYPE (experiment E4)
Sound architecture, and the two-stage search (cheap fingerprints and heuristics
eliminate candidates; expensive experiments measure the survivors) is how production
systems do it. Two amendments:
- Ingest-time choice stays a fixed, cheap default; search runs offline and its output
  materializes before it becomes active, per the invariant.
- "Learn from previous results" starts as a simple per-family strategy table measured
  in E4. No learned model until the table's accuracy is known; the corpus is too small
  to train on honestly.

### 4. Dictionary generation as optimization - PROTOTYPE (experiment E2)
Multi-image training, competition between candidate dictionaries, portfolios per
content family, and savings-based scoring (net of the dictionary's own storage and
the frame index) are all directly testable on the existing repack. Dictionary
retirement is the correctness trap: a retired dictionary must stay stored until no
referencing object remains, which is exactly what gc already tracks, so model
retirement on gc rather than a new lifecycle.

### Dictionary mosaics - GATED (experiment E6)
Per-region dictionaries for one file are attractive but segmentation overhead and the
frame index can erase the gain. Only worth testing if E6 (entropy-based region
decomposition) first shows regions with genuinely different statistics at sizes large
enough to amortize per-region metadata. Do not build mosaic storage before that read.

### 5. Storage co-optimization - SEQUENCE LAST
The global optimizer is the right end state and the highest correctness risk: it
rewrites manifests and shared objects, so it needs transactional manifest rewrites,
idempotent passes, and crash safety before anything else. Build the safe subset first:
periodic redundancy consolidation driven by the redundancy graph from E3, with
manifests rewritten transactionally and verified byte-exact afterward. Metadata
compression (E5) splits out as a cheap, low-risk, shippable item on its own.

### 6. Unusual ideas - triage
- (A) opportunity mining: partially shipped (elision). The pattern catalog is worth a
  read-only prototype after E3, not before; it is E3's index with a report on top.
- (B) semantic-independent binary decomposition: entropy-transition segmentation is
  cheap offline analysis and feeds mosaics and dictionary portfolios. Test E6.
- (C) probabilistic discovery: agreed framing, hashing filters narrow, hashes verify.
  This is infrastructure for E3, not a storage mechanism.
- (D) strategy evolution: deprioritized; per-family strategy table first (E4).
- (E) storage economics engine: this is the optimizer's objective function. Keep it
  as the spec section of the optimizer design, not a separate subsystem.

## Open research questions (this branch's work)

1. Chunking vs window: the 128 MB zstd window at ingest already lets cross-file
   redundancy compress if files land in the same recompression pass. Measure how much
   redundancy windowing already removes before crediting CDC for it (E1 must isolate
   this).
2. Dictionary training source: whole-store sampling vs per-family sampling vs
   representative-region sampling. zstd's trainer is the first implementation;
   competition between its output variants is cheap.
3. Similarity index scale: fingerprint/MinHash construction cost and memory on the
   35-image corpus, and what it costs at 10x that size.
4. Entropy segmentation: does it find stable, compressible regions in ELF, JS, and
   package metadata at useful granularity?
5. Optimizer crash safety: transactional manifest rewrite design, pass idempotency,
   and how consolidation interacts with gc and with an in-flight store/restore.

## Explicit exclusions

- Read-time reassembly or patching beyond what measurement shows is free.
- Nondeterministic ingest-time representation choice.
- Learned strategy models before the simple table is measured.

## Prior art to read before prototyping

LBFS (rolling-hash dedup), Venti, FastCDC (Xia et al.), restic and borg chunking,
zstd dictionary training notes, ostree's hardlink model. eStargz is rejected by the
invariant (cold-start latency), cite it as the counterexample.
