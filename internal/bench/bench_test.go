package bench

import (
	"errors"
	"hash/crc64"
	"testing"

	"yodadb/dberrors"
)

var (
	benchmarkPage     = make([]byte, 4096)
	benchmarkCRCTable = crc64.MakeTable(crc64.ECMA)
	benchmarkChecksum uint64
	benchmarkError    error
)

func BenchmarkCRC64Page(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchmarkPage)))
	for b.Loop() {
		benchmarkChecksum = crc64.Checksum(benchmarkPage, benchmarkCRCTable)
	}
}

func BenchmarkStructuredErrorWrap(b *testing.B) {
	cause := errors.New("disk failure")
	b.ReportAllocs()
	for b.Loop() {
		benchmarkError = dberrors.Wrap(cause, dberrors.CodeIO, "pager.read", "read page")
	}
}
