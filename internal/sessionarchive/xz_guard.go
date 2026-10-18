package sessionarchive

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// The xz library treats ReaderConfig.DictCap as a minimum. Before invoking it,
// parse the LZMA2 framing without decompression and reject dictionaries larger
// than 64 MiB in every block. Export uses an 8 MiB dictionary and one filter.
func inspectXZ(ctx context.Context, f *os.File, size int64) error {
	invalid := func() error { return fmt.Errorf("unsupported or malformed archive compression") }
	if size < 32 {
		return invalid()
	}
	var header, footer [12]byte
	if _, err := f.ReadAt(header[:], 0); err != nil {
		return err
	}
	if _, err := f.ReadAt(footer[:], size-12); err != nil {
		return err
	}
	if !bytes.Equal(header[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0}) || !bytes.Equal(footer[10:], []byte{'Y', 'Z'}) || header[6] != 0 || header[6] != footer[8] || header[7] != footer[9] {
		return invalid()
	}
	checks := map[byte]int64{0: 0, 1: 4, 4: 8, 10: 32}
	checkSize, ok := checks[header[7]]
	if !ok {
		return invalid()
	}
	indexSize := (int64(binary.LittleEndian.Uint32(footer[4:8])) + 1) * 4
	indexStart := size - 12 - indexSize
	if indexStart < 12 {
		return invalid()
	}
	position := int64(12)
	readByte := func() (byte, error) { var b [1]byte; _, err := f.ReadAt(b[:], position); position++; return b[0], err }
	for position < indexStart {
		if err := ctx.Err(); err != nil {
			return err
		}
		blockStart := position
		first, err := readByte()
		if err != nil {
			return err
		}
		if first == 0 {
			return invalid()
		}
		blockSize := (int(first) + 1) * 4
		block := make([]byte, blockSize)
		block[0] = first
		if _, err := f.ReadAt(block[1:], position); err != nil {
			return err
		}
		position += int64(blockSize - 1)
		flags := block[1]
		if flags&0x3f != 0 {
			return invalid()
		}
		offset := 2
		vli := func() (uint64, error) {
			var value uint64
			for i := 0; i < 9; i++ {
				if offset >= len(block)-4 {
					return 0, invalid()
				}
				b := block[offset]
				offset++
				value |= uint64(b&0x7f) << uint(7*i)
				if b&0x80 == 0 {
					return value, nil
				}
			}
			return 0, invalid()
		}
		if flags&0x40 != 0 {
			if _, err := vli(); err != nil {
				return err
			}
		}
		if flags&0x80 != 0 {
			if _, err := vli(); err != nil {
				return err
			}
		}
		filter, err := vli()
		if err != nil || filter != 0x21 {
			return invalid()
		}
		properties, err := vli()
		if err != nil || properties != 1 || offset >= len(block)-4 {
			return invalid()
		}
		if block[offset] > 28 {
			return fmt.Errorf("archive compression dictionary exceeds 64 MiB")
		}
		for {
			control, err := readByte()
			if err != nil {
				return err
			}
			if control == 0 {
				break
			}
			if control == 1 || control == 2 {
				var b [2]byte
				if _, err := f.ReadAt(b[:], position); err != nil {
					return err
				}
				position += 2 + int64(binary.BigEndian.Uint16(b[:])) + 1
			} else if control >= 0x80 {
				var b [4]byte
				if _, err := f.ReadAt(b[:], position); err != nil {
					return err
				}
				position += 4 + int64(binary.BigEndian.Uint16(b[2:])) + 1
				if control >= 0xc0 {
					position++
				}
			} else {
				return invalid()
			}
			if position > indexStart {
				return invalid()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		// The compressed LZMA2 stream is followed by zero block padding and the
		// stream-selected integrity check. The decoder verifies their contents.
		position += (4 - (position-blockStart)%4) % 4
		position += checkSize
		if position > indexStart {
			return invalid()
		}
	}
	if position != indexStart {
		return invalid()
	}
	_, err := f.Seek(0, io.SeekStart)
	return err
}
