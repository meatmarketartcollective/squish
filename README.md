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

Requires Go 1.23+:

```
go install github.com/aloysiusvonfck/squish@latest
```

Release binaries for linux/amd64 and linux/arm64 are on the Releases page.

## Use

Store a docker save archive (the tarball Docker writes, no running daemon
needed):

```
docker save redis -o redis.tar
squish store --store ~/squish redis.tar
```

Store an extracted layer directory instead:

```
squish store --store ~/squish ~/layers/redis
```

See what you saved:

```
squish stats --store ~/squish redis
```

Rebuild the image tree byte-exactly (duplicates become hardlinks, so the
rebuild costs almost nothing on disk):

```
squish materialize --store ~/squish redis ./redis-tree
```

Check every stored byte still hashes back:

```
squish verify --store ~/squish redis
```

Run the deep offline pass on small files (idle time job):

```
squish repack --store ~/squish redis
```

The store root defaults to `$STACKFOLD_STORE`, then `./.squish`.
Pass `--no-elide` to keep regenerable files.

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

- v1 has no transparent mount view yet: reading a file goes through
  `materialize` (or a future FUSE layer). The store's read cost is decompress
  only, which measured free relative to storage.
- The version-delta pass (file pairs compressed against a shared base) and
  family dictionaries for small files are designed and measured but not in
  this binary yet; the repack frame dictionary is the first slice of both.
- All published numbers come from one ARM device and one 35-image corpus.

## License

MIT
