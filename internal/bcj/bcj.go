// Package bcj implements the xz ARM64 branch filter in pure Go so cold
// frames need no external xz binary.
//
// Port of xz's liblzma simple/arm64.c (SPDX 0BSD, authors Lasse Collin,
// Jia Tan, Igor Pavlov). It converts the relative immediates of ARM64 BL
// and ADRP instructions to absolute values, which makes branch targets
// repeat across similar call sites and compress better. The transform is
// its own inverse: encode then decode returns the original bytes.
package bcj

import "encoding/binary"

// Arm64 applies or reverses the ARM64 BCJ filter in place.
func Arm64(b []byte, encode bool) {
	size := len(b) &^ 3
	for i := 0; i < size; i += 4 {
		pc := uint32(i)
		instr := binary.LittleEndian.Uint32(b[i:])
		if instr>>26 == 0x25 {
			// BL: the full 26-bit immediate is converted.
			src := instr
			instr = 0x94000000
			pc >>= 2
			if !encode {
				pc = 0 - pc
			}
			instr |= (src + pc) & 0x03FFFFFF
			binary.LittleEndian.PutUint32(b[i:], instr)
		} else if instr&0x9F000000 == 0x90000000 {
			// ADRP: values in the +/-512 MiB range are converted.
			src := ((instr >> 29) & 3) | ((instr >> 3) & 0x001FFFFC)
			if (src+0x00020000)&0x001C0000 != 0 {
				continue
			}
			instr &= 0x9000001F
			pc >>= 12
			if !encode {
				pc = 0 - pc
			}
			dest := src + pc
			instr |= (dest & 3) << 29
			instr |= (dest & 0x0003FFFC) << 3
			instr |= (0 - (dest & 0x00020000)) & 0x00E00000
			binary.LittleEndian.PutUint32(b[i:], instr)
		}
	}
}

// Encode returns a copy of b with branch immediates made absolute.
func Encode(b []byte) []byte {
	out := append([]byte(nil), b...)
	Arm64(out, true)
	return out
}

// Decode reverses Encode exactly.
func Decode(b []byte) []byte {
	out := append([]byte(nil), b...)
	Arm64(out, false)
	return out
}
