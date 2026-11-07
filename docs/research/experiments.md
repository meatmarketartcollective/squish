# Store-optimization experiments: plan

Companion to the research plan on `research/store-optimization`. Every idea in the
roadmap that is testable gets one experiment here. This branch carries the plan and
its harness; all experiment code runs in Termux on the phone per Casey's direction,
never in CI.

## Corpus and baseline

The 35-image, 6.19 GB official-image corpus every adopted feature was measured on.
Freeze it: pin the manifest list and the squish master SHA each run starts from
(currently `9764de0`), and record both in every result. No number ships without its
SHA. Runs are reproducible: same corpus, same SHA, same flags.

## Metric template (every experiment reports exactly this)

- total original bytes
- total physical bytes consumed by the store
- bytes eliminated (absolute, and as % for readability only)
- metadata and index overhead bytes (manifests, frame indexes, dictionaries, chunk indexes)
- ingest compute and wall time
- offline optimization compute and wall time
- materialize throughput, and mount random-read latency, vs the baseline at the same SHA
- byte-exact reconstruction and integrity verification result
- store format changes the experiment requires

## Gate

A technique is adoptable iff: store bytes drop beyond measurement noise, materialize
and mount read performance is within noise of the baseline, and reconstruction
verifies byte-exact. There are no percentage kill lines; the absolute byte report and
the standing no-read-latency rule decide. Implementation cost and correctness risk are
judged per feature after the measurement.

## Experiments

Order matters only where noted; E1, E2 and E3 each start from the baseline.

**E1. Content-defined chunk dedup (very high priority).**
Question: how many bytes does whole-file dedup leave on the table, and how much of
that does the 128 MB ingest window already remove? Method: FastCDC-style chunking
(2/8/16 KiB targets) as a read-only offline pass over the extracted corpus trees (never store objects: those are already compressed); report unique
chunk bytes vs unique-file bytes, before and after subtracting redundancy the window
already compresses (isolate by comparing against the same corpus recompressed with
windowing disabled in a scratch run). If the isolated remainder is real, prototype a
chunked object layout with a per-file frame index over chunk references and measure
materialize and mount cost against baseline. Chunk reuse across images (E) is the
same pass with a store-wide chunk index; report both.

**E2. Multi-image dictionary training (very high priority).**
Question: do dictionaries trained across the whole store beat the current
per-path-group dictionaries, net of dictionary storage and frame-index growth?
Method: extend the repack's dictionary trainer to sample candidates from the whole
store and per content family; run a small competition (whole-store sample,
per-family samples, representative-region samples) against the same file population;
score each dictionary by total bytes saved across all users, net of its own cost.
Also measure retirement safety: gc must retain any dictionary still referenced.

**E3. Cross-image similarity index (very high priority).**
Question: what redundancy does per-file hashing not see, and how cheaply can we
find it? Method: rolling-hash region fingerprints plus banded MinHash to produce
candidate similar-file pairs without all-pairs comparison; verify candidates exactly;
report candidate yield, precision at the k-th rank, index build time and memory, and
an estimate of potential savings from delta-encoding the near-identical pairs
(estimate only; representation search decides later). This index is also the
substrate for opportunity mining and for improving `delta` base selection.

**E4. Representation search (very high priority, after E1-E3).**
Question: for each file, which of independent compression, existing-dictionary
compression, new-dictionary compression, chunk dedup, or delta against a candidate
base wins, net of metadata? Method: two-stage search, stage 1 cheap fingerprints and
heuristics, stage 2 real experiments on survivors; report the chosen mix, total
store bytes, and the per-family strategy table that falls out. Ingest keeps its
fixed default; the search result materializes offline before it goes active.

**E5. Metadata compression (medium, cheap, shippable first).**
Measure manifests, frame indexes and reference records for compressible redundancy;
compress what pays. Lowest risk in the set; no read-path design questions.

**E6. Entropy-based region decomposition (high, read-only).**
Offline analysis only: segment representative binaries by entropy transitions and
repeated-sequence boundaries; report whether stable, statistically distinct regions
exist at sizes worth per-region treatment. Gates dictionary mosaics: mosaic storage
is built only if this read says the regions are real and large enough to amortize
segmentation overhead.

**E7. Global optimizer design (sequenced last).**
No experiment until E1-E4 report. Design work only on this branch: objective
function (minimize total physical bytes subject to exact reconstruction, no read
latency, integrity, recoverability, bounded metadata), transactional manifest
rewrite, idempotent passes, crash safety, gc interplay. The first build target is
the safe subset: periodic redundancy consolidation driven by E3's redundancy graph,
verified byte-exact after each pass.

## Reporting

Results go in a results memo per experiment on this branch, metrics template
verbatim, with the corpus freeze and squish SHA named. Blockers first in every
report; every number real or marked Unavailable.
