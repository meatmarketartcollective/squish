# Squish

Squish shrinks what your Docker containers take on disk, without making reads slower. It stores the extracted contents of every image
once, per file, in a content-addressed store, and can rebuild any image tree
byte-exactly from that store.

## Why

Docker already shares identical layer blobs between images. What it does not
do is compress the extracted contents harder, deduplicate files that differ
between blobs but repeat between them, or skip bytes that can simply be
regenerated. Squish does all three, and every mechanism in it is one that
survived measurement on a 35-image, 6.19 GB corpus of official images:

- **Per-file content-addressed store.** Every unique file's content is stored
  once under its BLAKE2b-256 hash. Identical files across images and layers
  are stored a single time; `materialize` rebuilds the tree and hardlinks the
  duplicates.
- **zstd with a 128 MB window at the install path.** Fast settings matched
  raw-read throughput in the speed benchmark (reads 9 MB/s from the store,
  9 MB/s raw; zstd decompresses at 200+ MB/s, far faster than storage), so
  compression costs nothing on the read path.
- **Path-sorted deep repack (offline).** Small files in the same directory are
  recompressed at deep settings with a shared dictionary built from the
  group's own contents, keeping per-file random access through a frame index.
  This is the offline pass: scheduled idle work, never on the read path.
- **Regenerable-artifact elision (on by default).** Python bytecode caches,
  package-manager caches, docs, translation catalogs and man pages are never
  stored; the manifest records what was skipped and why, and the tree
  materializes without them. Measured at 0.6% on a slim-alpine corpus, more on
  fat images.

Rejected on evidence, not on vibes: lazy pulling and page-level faulting
(first-touch latency breaks the "reads don't slow down" bar), neural codecs
(decode speed), and package-provenance reconstruction (measured at 43.5%
coverage on the corpus; statically bundled toolchains and runtime binaries
dominate the residual, so it stays out of v1).

## Install

    curl -fsSL https://raw.githubusercontent.com/aloysiusvonfck/squish/master/install.sh | sh

That puts the right binary for your machine in ~/.local/bin. Prefer manual: grab a binary from [releases](https://github.com/aloysiusvonfck/squish/releases) (linux amd64/arm64, darwin arm64) and put it on your PATH.

## Quick start

    squish reclaim                 # store every docker image, free the space
    squish ls                      # what is stored, what it saves
    squish restore alpine_latest   # bring an image back into docker
    squish gc                      # after restores and cleanup

`reclaim` saves each tagged image, verifies the stored copy byte-for-byte, and only then removes the original (pass --keep to leave images in place). `restore` rebuilds the image as a single squashed layer and loads it with docker. Names are the image ref with / : @ replaced by _.

## Use

  squish store    [--store DIR] [--name NAME] [--no-elide] SOURCE...
  squish materialize [--store DIR] MANIFEST OUTDIR [--no-hardlinks] [--verify]
  squish verify   [--store DIR] MANIFEST
  squish stats    [--store DIR] MANIFEST
  squish repack   [--store DIR] MANIFEST [--max-file N] [--min-group N] [--force]
  squish delta    [--store DIR] [--min-file N] [--max-file N]
  squish cold     [--store DIR] [--min-size N] MANIFEST
  squish gc       [--store DIR]
  squish mount    [--store DIR] MANIFEST MNT
  squish reclaim  [--store DIR] [--image A,B] [--keep] [--min-size-mb N]
  squish restore  [--store DIR] [--out DIR] NAME...
  squish ls       [--store DIR]

Store commands take a docker save archive (.tar or .tar.gz) or a directory.
Manifests are named by their basename unless --name is given.

## Store layout

```
<root>/blobs/<h2>/<hash>.zst     unique file contents, one zstd frame each
<root>/frames/<gid>.zst          repack batches: concatenated dict-compressed frames
<root>/frames/<gid>.dict         the batch dictionary
<root>/frames/<gid>.idx.json     per-file offsets into the frame
<root>/manifests/<name>.json     the image's file table
```

Manifests record path, size, mode and content hash for every file, so
`verify` and `materialize` are byte-exact by construction. File mtimes are
not preserved in v1; modes are.

## Honest caveats

`restore` rebuilds the image as one squashed layer from the flattened store, verified structurally against docker's load format; the first real docker load still deserves a watch. Regenerable files elided at store time (caches, docs, locales, man pages) are not part of a restored image.

- v1 has no transparent mount view yet: reading a file goes through
  `materialize` (or a future FUSE layer). The store's read cost is decompress
  only, which measured free relative to storage.
- The version-delta pass (file pairs compressed against a shared base) and
  family dictionaries for small files are designed and measured but not in
  this binary yet; the repack frame dictionary is the first slice of both.
- All published numbers come from one ARM device and one 35-image corpus.

## License

MIT

## The extra passes

`squish delta` runs across every manifest in the store. Files that live at
the same path in several images but with different content (library
versions, config drift) get rewritten as delta frames against the most
common version, and blobs nothing references anymore are dropped by the
gc pass that follows. Reads stay byte-exact: a delta decodes against its
base blob at materialize time.

`squish cold MANIFEST` moves large ELF binaries (1 MiB and up by default)
into xz frames with the BCJ filter matching the binary's own architecture
(arm64 or x86). These are binaries your containers read once per image,
never per run, so the slower decode buys real space. Needs an `xz` with
BCJ support (`SQUISH_XZ` can point at one; Termux's xz has it).

`squish gc` removes every blob, delta frame and cold frame no manifest
references. Delta bases are protected.

## Browsing without materializing

`squish mount MANIFEST MNT` presents the manifest as a read-only
filesystem: the whole image layout browsable and readable straight out
of the store, nothing materialized to disk. Needs FUSE on the host
(`fusermount`/`fusermount3`). Elided files return an error on read with
the reason they were elided.
