# Memo 1: what the 128 MB window already absorbs, and dictionary training sources

Answering research questions 1 and 2 from store-optimization.md, from squish's
own architecture and prior art. No measurements yet; E1's v2 is the decisive read.

## Q1. How much cross-file redundancy does the current window absorb?

Reading the pipeline: ingest compresses each file independently (per-file zstd,
128 MB window), so cross-file redundancy is not absorbed at ingest. The offline
repack recompresses path-sorted groups with a shared dictionary, and that is where
cross-file redundancy within a group compresses away (the window plus the shared
dictionary both see the group's content). So the current store already absorbs
redundancy WITHIN repack groups, and misses:

1. cross-group redundancy: structurally similar files in different directories;
2. cross-image redundancy wherever repack groups differ between images;
3. partial-file redundancy: shared regions inside differing files, which a
   dictionary cannot express as locality.

Consequence for E1: the headline number (unique chunk bytes vs unique file bytes)
overstates CDC's incremental value, because part of that redundancy is already
absorbed inside groups. E1 v2 must split chunk overlap three ways: same repack
group, cross-group same image, cross-image. cdcbench v1 reports totals only;
the group tagging comes from the manifest's path records and is the next tool
change. If cross-group and cross-image overlap come back small, CDC is a weak
win and the roadmap's center of gravity moves to dictionaries (E2).

## Q2. Dictionary training sources

zstd's dictionary trainer (train_samples) wants hundreds to thousands of samples
and, by default, samples near 100 KiB; feeding it whole binaries dilutes the
signal. Expected ordering, to be settled by E2's competition, not this memo:

1. per-family samples (ELF, package metadata, text configs) trained separately,
   used as a portfolio selected per file: likely beats one whole-store dictionary;
2. whole-store sampling: one dictionary, simplest lifecycle, weakest per-file fit;
3. representative-region sampling (cut samples at content-defined boundaries
   rather than file starts): aligns training material with the real repeated
   structure and may lift both 1 and 2.

Scoring stays net: bytes saved across all users minus the dictionary's own
storage minus frame-index growth. Retirement rides on gc reference tracking;
no dictionary is dropped while any referencing object exists.

## Q3. Similarity index sizing (for E3)

At 10^5 to 10^6 files, a 128 x 64-bit MinHash signature is about 1 KiB per file:
0.1 to 1 GiB resident, fine on the laptop. Banded LSH at 16 bands x 8 rows
targets Jaccard roughly 0.5 to 0.9, which is the range where delta-encoding a
pair plausibly beats storing both. Build cost is linear in corpus bytes.

## Bottom line

Nothing here is adoptable without the E1 v2 split. If the cross-group and
cross-image slices are thin, the honest conclusion is that squish's group
repack already captures most of what the roadmap attributes to similarity
discovery, and effort shifts to E2 and E4.
