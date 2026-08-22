package aggdata

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestReadBBOShardValidatesContainer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggbbo.sqrec.zst")
	writeCompressedTestShard(t, path, 1, false)
	assertReadBBOShardOK(t, path)
}

func TestReadBBOShardAcceptsContainerV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggbbo.sqrec.zst")
	writeCompressedTestShard(t, path, 2, false)
	assertReadBBOShardOK(t, path)
}

func TestReadBBOShardRejectsUnsupportedContainerMajor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggbbo.sqrec.zst")
	writeCompressedTestShard(t, path, 3, false)
	if _, err := ReadBBOShard(path, nil); err == nil ||
		!strings.Contains(err.Error(), "invalid SQREC container header") {
		t.Fatalf("expected unsupported container header, got %v", err)
	}
}

func TestReadBBOShardRejectsCRC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggbbo.sqrec.zst")
	writeCompressedTestShard(t, path, 1, true)
	if _, err := ReadBBOShard(path, nil); err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Fatalf("expected CRC error, got %v", err)
	}
}

func assertReadBBOShardOK(t *testing.T, path string) {
	t.Helper()
	var seen []RecordedBBO
	count, err := ReadBBOShard(path, func(record RecordedBBO) error {
		seen = append(seen, record)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadBBOShard: %v", err)
	}
	if count != 1 || len(seen) != 1 || seen[0].GatedBPS != 0 ||
		seen[0].GatedMin != -7 || seen[0].GatedMax != 2 {
		t.Fatalf("unexpected records: %+v", seen)
	}
}

func writeCompressedTestShard(t *testing.T, path string, major uint16, corrupt bool) {
	t.Helper()
	payload := recordedBBOPayload()
	container := appendU32(nil, containerMagic)
	container = appendU16(container, major)
	container = appendU16(container, 0)
	container = appendU32(container, 0x01020304)
	container = appendU32(container, crc32.ChecksumIEEE(container))
	container = appendU32(container, recordMagic)
	container = appendU32(container, uint32(len(payload)))
	checksum := crc32.ChecksumIEEE(payload)
	if corrupt {
		checksum++
	}
	container = appendU32(container, checksum)
	container = append(container, payload...)
	trailerStart := len(container)
	container = appendU32(container, trailerMagic)
	container = appendU64(container, 1)
	container = appendU32(container, crc32.ChecksumIEEE(container[trailerStart:]))
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(container, nil)
	encoder.Close()
	if err := os.WriteFile(path, compressed, 0o600); err != nil {
		t.Fatal(err)
	}
}

func recordedBBOPayload() []byte {
	output := []byte{byte(KindBBO), 0}
	output = appendU16(output, 0)
	output = appendU64(output, 1_000_000_000)
	output = appendU64(output, 2)
	output = appendU64(output, 3)
	output = appendU64(output, 4)
	output = appendU64(output, 5)
	header := make([]byte, sqmdRecordHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], sqmdMagic)
	binary.LittleEndian.PutUint16(header[4:6], sqmdSchemaMajor)
	binary.LittleEndian.PutUint16(header[6:8], sqmdSchemaMinor)
	binary.LittleEndian.PutUint16(header[8:10], sqmdAggBboMessage)
	binary.LittleEndian.PutUint16(header[10:12], sqmdAggBboRecordSize)
	output = append(output, header...)
	output = append(output, make([]byte, 99*2+21*2)...)
	base := make([]byte, 16)
	quote := make([]byte, 16)
	copy(base, "BTC")
	copy(quote, "USDT")
	output = append(output, base...)
	output = append(output, quote...)
	output = append(output, 1, 0, 0, 0, 0, 0, 0, 0)
	output = append(output, 2, 3, 1)
	output = appendU32(output, 1)
	output = appendU32(output, 1)
	output = appendI32(output, 5)
	output = appendI32(output, 4)
	output = append(output, make([]byte, 15)...)
	output = appendU64(output, 10)
	output = appendU64(output, 20)
	output = appendI32(output, -8)
	output = appendI32(output, 1)
	output = appendI32(output, -7)
	output = appendI32(output, 2)
	output = appendU64(output, 3)
	return output
}

func appendU16(output []byte, value uint16) []byte {
	return binary.LittleEndian.AppendUint16(output, value)
}

func appendU32(output []byte, value uint32) []byte {
	return binary.LittleEndian.AppendUint32(output, value)
}

func appendU64(output []byte, value uint64) []byte {
	return binary.LittleEndian.AppendUint64(output, value)
}

func appendI32(output []byte, value int32) []byte {
	return binary.LittleEndian.AppendUint32(output, uint32(value))
}
