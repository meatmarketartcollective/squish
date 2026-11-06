package store

// Advanced store features: version-delta blobs, BCJ cold frames, and
// manifest listing for whole-store operations.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	KindDelta = "delta"
	KindCold  = "cold"
)

// DeltaPath is where a target's delta-against-base frame lives.
func (s *Store) DeltaPath(target, base string) string {
	return filepath.Join(s.Root, "deltas", target+".from."+base+".zst")
}

// ColdPath is where an xz/BCJ cold frame lives.
func (s *Store) ColdPath(hash string) string {
	return filepath.Join(s.Root, "cold", hash+".xz")
}

// PutDelta compresses data against the base file's content and stores the
// delta frame. Returns the compressed size.
func (s *Store) PutDelta(target, base string, data []byte) (int64, error) {
	baseData, err := s.ReadBlob(base)
	if err != nil {
		return 0, fmt.Errorf("delta base %s: %w", base[:12], err)
	}
	comp, err := CompressDelta(baseData, data)
	if err != nil {
		return 0, err
	}
	p := s.DeltaPath(target, base)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, err
	}
	tmp := p + ".tmp" + RandomID(4)
	if err := os.WriteFile(tmp, comp, 0o644); err != nil {
		return 0, err
	}
	return int64(len(comp)), os.Rename(tmp, p)
}

// CompressDelta delta-encodes data against baseData with zstd dictionaries.
func CompressDelta(baseData, data []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderDictRaw(1, baseData),
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithWindowSize(1<<27),
	)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil), nil
}

// ReadDelta decodes a delta frame against its base content.
func (s *Store) ReadDelta(target, base string) ([]byte, error) {
	comp, err := os.ReadFile(s.DeltaPath(target, base))
	if err != nil {
		return nil, err
	}
	baseData, err := s.ReadBlob(base)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderMaxWindow(1<<27),
		zstd.WithDecoderDictRaw(1, baseData),
	)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(comp, nil)
}

// DeltaSize returns the stored size of a delta frame.
func (s *Store) DeltaSize(target, base string) (int64, error) {
	fi, err := os.Stat(s.DeltaPath(target, base))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// xzBinary locates an xz executable: $SQUISH_XZ, then PATH.
func xzBinary() (string, error) {
	if p := os.Getenv("SQUISH_XZ"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	p, err := exec.LookPath("xz")
	if err != nil {
		return "", fmt.Errorf("no xz binary found (set SQUISH_XZ or install xz); BCJ cold frames need it")
	}
	return p, nil
}

// CompressCold compresses data with xz -6 plus the named BCJ filter
// ("arm64", "x86", or "" for none). The stream records its own filter.
func CompressCold(filter string, data []byte) ([]byte, error) {
	xz, err := xzBinary()
	if err != nil {
		return nil, err
	}
	args := []string{"-6", "-c", "--check=crc32"}
	switch filter {
	case "arm64":
		args = append(args, "--arm64")
	case "x86":
		args = append(args, "--x86")
	case "":
		// no filter
	default:
		return nil, fmt.Errorf("unknown BCJ filter %q", filter)
	}
	cmd := exec.Command(xz, args...)
	cmd.Stdin = bytes.NewReader(data)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if strings.Contains(msg, "Unsupported options in filter chain") && filter != "" {
			return nil, fmt.Errorf("this xz lacks the %s BCJ filter; cold frames need an xz built with BCJ support (set SQUISH_XZ to one that has it)", filter)
		}
		return nil, fmt.Errorf("xz: %w: %s", err, msg)
	}
	return out.Bytes(), nil
}

// WriteCold stores already-compressed cold bytes under the content hash.
func (s *Store) WriteCold(hash string, comp []byte) error {
	p := s.ColdPath(hash)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp" + RandomID(4)
	if err := os.WriteFile(tmp, comp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ReadCold decodes an xz cold frame.
func (s *Store) ReadCold(hash string) ([]byte, error) {
	xz, err := xzBinary()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(xz, "-dc", s.ColdPath(hash))
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("xz decode: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// ColdSize returns the stored size of a cold frame.
func (s *Store) ColdSize(hash string) (int64, error) {
	fi, err := os.Stat(s.ColdPath(hash))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// ListManifests returns every manifest name in the store, sorted.
func (s *Store) ListManifests() ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(s.Root, "manifests"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(names)
	return names, nil
}
