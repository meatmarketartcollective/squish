package store

// Gear-based content-defined chunking (FastCDC, Xia et al. 2016).
// Chunk boundaries are content-determined, so identical regions cut at
// the same points across files, layers and images. Deterministic across
// builds: the Gear table is generated from a fixed seed at init time.

// Chunking parameters for ingest, in bytes.
const (
	MinChunkSize = 2048
	AvgChunkSize = 8192
	MaxChunkSize = 32768
	// ChunkMinFile: files at least this large are stored chunked;
	// smaller files stay whole-file blobs.
	ChunkMinFile = 4096
)

var gearTable [256]uint64

func init() {
	var x uint64 = 0x9E3779B97F4A7C15
	for i := 0; i < 256; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		gearTable[i] = x
	}
}

// ChunkData splits data into content-defined chunks. Requires
// min <= avg <= max. The returned slices alias data.
func ChunkData(data []byte, min, avg, max int) [][]byte {
	if len(data) == 0 {
		return nil
	}
	bits := 0
	for 1<<bits < avg {
		bits++
	}
	maskS := (uint64(1) << (bits - 1)) - 1
	maskL := (uint64(1) << (bits + 1)) - 1
	var chunks [][]byte
	h := uint64(0)
	start := 0
	for i := 0; i < len(data); i++ {
		h = (h << 8) + gearTable[data[i]&0xFF]
		n := i + 1 - start
		if n >= min &&
			(n >= max ||
				(n < avg && h&maskS == 0) ||
				(n >= avg && h&maskL == 0)) {
			chunks = append(chunks, data[start:i+1])
			start = i + 1
		}
	}
	if start < len(data) {
		chunks = append(chunks, data[start:])
	}
	return chunks
}
