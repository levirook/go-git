package dotgit

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
)

const (
	reftablePath     = "reftable"
	reftableListFile = "tables.list"

	reftableMagic      = "REFT"
	reftableHeaderSize = 24
	reftableFooterSize = 68

	// reftable block types
	reftableBlockRef = 'r'

	// reftable ref record value types
	reftableValueDeletion = 0 // ref is deleted
	reftableValueSHA1     = 1 // single hash (SHA1 or SHA256)
	reftableValueTwoSHA   = 2 // two hashes (annotated tag: tag object + tagged object)
	reftableValueSymref   = 3 // symbolic reference
)

var (
	// ErrReftableNotSupported is returned when a write operation is attempted
	// on a repository using reftable storage. Write operations require a full
	// reftable implementation to avoid data corruption.
	ErrReftableNotSupported = errors.New("reftable ref storage is not writable; write operations are not supported")
)

// isReftable reports whether the repository uses reftable ref storage by
// checking for the presence of the reftable tables.list file.
func (d *DotGit) isReftable() bool {
	_, err := d.fs.Stat(d.fs.Join(reftablePath, reftableListFile))
	return err == nil
}

// reftableRefs returns all references stored in the reftable storage.
func (d *DotGit) reftableRefs() ([]*plumbing.Reference, error) {
	merged, err := d.mergeReftables()
	if err != nil {
		return nil, err
	}

	refs := make([]*plumbing.Reference, 0, len(merged))
	for _, ref := range merged {
		if ref != nil {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// reftableLookupRef returns the reference with the given name from the reftable storage.
func (d *DotGit) reftableLookupRef(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	merged, err := d.mergeReftables()
	if err != nil {
		return nil, err
	}

	ref, ok := merged[name]
	if !ok || ref == nil {
		return nil, plumbing.ErrReferenceNotFound
	}
	return ref, nil
}

// mergeReftables reads the tables.list file and merges all reftable files,
// returning a map from reference name to reference. Newer tables override
// older ones. Deletion records remove entries (nil value).
func (d *DotGit) mergeReftables() (map[plumbing.ReferenceName]*plumbing.Reference, error) {
	listPath := d.fs.Join(reftablePath, reftableListFile)
	f, err := d.fs.Open(listPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var tableNames []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if name != "" {
			tableNames = append(tableNames, name)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	merged := make(map[plumbing.ReferenceName]*plumbing.Reference)
	for _, name := range tableNames {
		tablePath := d.fs.Join(reftablePath, name)
		tf, err := d.fs.Open(tablePath)
		if err != nil {
			return nil, err
		}

		data, err := func() ([]byte, error) {
			defer tf.Close()
			fi, err := d.fs.Stat(tablePath)
			if err != nil {
				return nil, err
			}
			buf := make([]byte, fi.Size())
			_, err = io.ReadFull(tf, buf)
			if err != nil {
				return nil, err
			}
			return buf, nil
		}()
		if err != nil {
			return nil, err
		}

		if err := parseReftableInto(data, d.options.ObjectFormat, merged); err != nil {
			return nil, err
		}
	}

	return merged, nil
}

// parseReftableInto parses a reftable binary file and merges the references
// into dst. Deletion records set the map entry to nil.
func parseReftableInto(data []byte, objectFormat formatcfg.ObjectFormat, dst map[plumbing.ReferenceName]*plumbing.Reference) error {
	if len(data) < reftableHeaderSize+reftableFooterSize {
		return errors.New("reftable file too small")
	}

	if string(data[0:4]) != reftableMagic {
		return errors.New("invalid reftable magic")
	}

	// Determine hash size based on object format.
	hashSize := formatcfg.SHA1Size
	if objectFormat == formatcfg.SHA256 {
		hashSize = formatcfg.SHA256Size
	}

	// Read log_position from the footer to know where ref blocks end.
	footerStart := len(data) - reftableFooterSize
	if string(data[footerStart:footerStart+4]) != reftableMagic {
		return errors.New("invalid reftable footer magic")
	}
	logPosition := binary.BigEndian.Uint64(data[footerStart+48 : footerStart+56])

	offset := 0
	for {
		if offset >= len(data)-reftableFooterSize {
			break
		}
		if logPosition > 0 && uint64(offset) >= logPosition {
			break
		}

		// The first block is special: the 24-byte file header is embedded at the
		// start. The block type and length are at offsets 24 and 25–27.
		var blockTypeOff, blockLenStart, recordsStart int
		var blockEnd int

		if offset == 0 {
			blockTypeOff = reftableHeaderSize
			blockLenStart = reftableHeaderSize + 1
			recordsStart = reftableHeaderSize + 4 // after file header + block header
		} else {
			blockTypeOff = offset
			blockLenStart = offset + 1
			recordsStart = offset + 4
		}

		if blockTypeOff >= len(data) {
			break
		}

		blockType := data[blockTypeOff]
		blockLen := int(uint32(data[blockLenStart])<<16 | uint32(data[blockLenStart+1])<<8 | uint32(data[blockLenStart+2]))

		if offset == 0 {
			// For the first block, block_len counts from file position 0 (includes
			// the 24-byte file header).
			blockEnd = blockLen
		} else {
			blockEnd = offset + blockLen
		}

		if blockEnd > len(data) {
			return errors.New("reftable block length exceeds file size")
		}

		if blockType != reftableBlockRef {
			offset = blockEnd
			continue
		}

		if err := parseRefBlockInto(data[recordsStart:blockEnd], hashSize, dst); err != nil {
			return err
		}

		offset = blockEnd
	}

	return nil
}

// parseRefBlockInto parses the records in a ref block and merges them into dst.
func parseRefBlockInto(blockData []byte, hashSize int, dst map[plumbing.ReferenceName]*plumbing.Reference) error {
	if len(blockData) < 2 {
		return nil
	}

	// The last 2 bytes of the block are the restart_count.
	restartCount := int(binary.BigEndian.Uint16(blockData[len(blockData)-2:]))
	restartArraySize := restartCount * 3
	recordsEnd := len(blockData) - 2 - restartArraySize

	if recordsEnd < 0 || recordsEnd > len(blockData) {
		return errors.New("invalid restart array in reftable block")
	}

	prevName := ""
	pos := 0

	for pos < recordsEnd {
		prefixLen, newPos := reftableReadVarint(blockData, pos)
		if newPos < 0 {
			return errors.New("malformed varint in reftable record")
		}
		pos = newPos

		encoded, newPos := reftableReadVarint(blockData, pos)
		if newPos < 0 {
			return errors.New("malformed varint in reftable record")
		}
		pos = newPos

		suffixLen := int(encoded >> 3)
		valueType := int(encoded & 7)

		if int(prefixLen) > len(prevName) {
			return errors.New("reftable record prefix_len exceeds previous name length")
		}
		if pos+suffixLen > recordsEnd {
			return errors.New("reftable record suffix extends beyond block")
		}

		name := prevName[:prefixLen] + string(blockData[pos:pos+suffixLen])
		pos += suffixLen
		prevName = name

		// Consume update_index_delta (not needed for basic ref reading).
		_, newPos = reftableReadVarint(blockData, pos)
		if newPos < 0 {
			return errors.New("malformed update_index_delta in reftable record")
		}
		pos = newPos

		refName := plumbing.ReferenceName(name)

		switch valueType {
		case reftableValueDeletion:
			dst[refName] = nil

		case reftableValueSHA1:
			if pos+hashSize > recordsEnd {
				return errors.New("reftable ref record truncated (hash)")
			}
			oid, ok := plumbing.FromBytes(blockData[pos : pos+hashSize])
			if !ok {
				return errors.New("invalid hash in reftable ref record")
			}
			pos += hashSize
			dst[refName] = plumbing.NewHashReference(refName, oid)

		case reftableValueTwoSHA:
			// Two hashes: the first is the tag object, the second is the tagged object.
			// We store the tag object hash (what git show-ref reports).
			if pos+hashSize*2 > recordsEnd {
				return errors.New("reftable ref record truncated (two hashes)")
			}
			oid, ok := plumbing.FromBytes(blockData[pos : pos+hashSize])
			if !ok {
				return errors.New("invalid hash in reftable ref record")
			}
			pos += hashSize * 2 // skip both hashes (second is the peeled target)
			dst[refName] = plumbing.NewHashReference(refName, oid)

		case reftableValueSymref:
			targetLen, newPos := reftableReadVarint(blockData, pos)
			if newPos < 0 {
				return errors.New("malformed symref target length in reftable record")
			}
			pos = newPos
			if pos+int(targetLen) > recordsEnd {
				return errors.New("reftable symref target extends beyond block")
			}
			target := plumbing.ReferenceName(blockData[pos : pos+int(targetLen)])
			pos += int(targetLen)
			dst[refName] = plumbing.NewSymbolicReference(refName, target)

		default:
			return errors.New("unknown value_type in reftable ref record")
		}
	}

	return nil
}

// reftableReadVarint reads a git-style modified base-128 variable-length integer
// from data starting at pos. Returns the value and the new position.
// Returns -1 as position on error (out of bounds).
//
// This encoding is used throughout git for pack files and reftable files.
// It differs from standard LEB128: each continuation byte implicitly adds 1
// to the accumulated value, making the encoding non-redundant.
//
// Decoding:
//
//	val = byte & 0x7f
//	while byte has high bit set:
//	    val += 1
//	    byte = next byte
//	    val = (val << 7) | (byte & 0x7f)
func reftableReadVarint(data []byte, pos int) (uint64, int) {
	if pos >= len(data) {
		return 0, -1
	}
	b := data[pos]
	pos++
	val := uint64(b & 0x7f)
	for b&0x80 != 0 {
		val++
		if pos >= len(data) {
			return 0, -1
		}
		b = data[pos]
		pos++
		val = (val << 7) | uint64(b&0x7f)
	}
	return val, pos
}
