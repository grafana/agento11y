package local

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"sync"
)

// rawCodecDeflate marks a generation.raw value compressed with
// compress/flate. The empty codec is plain JSON.
const rawCodecDeflate = "deflate"

var (
	flateWriters = sync.Pool{New: func() any {
		w, _ := flate.NewWriter(nil, flate.BestSpeed)
		return w
	}}
	flateReaders sync.Pool
)

func deflateRaw(raw []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(raw) / 3)
	w := flateWriters.Get().(*flate.Writer)
	defer flateWriters.Put(w)
	w.Reset(&buf)
	// Writes to a bytes.Buffer cannot fail.
	_, _ = w.Write(raw)
	_ = w.Close()
	return buf.Bytes()
}

func decodeSQLRaw(codec string, raw []byte) ([]byte, error) {
	switch codec {
	case "":
		return raw, nil
	case rawCodecDeflate:
		src := bytes.NewReader(raw)
		r, ok := flateReaders.Get().(io.ReadCloser)
		if ok {
			if err := r.(flate.Resetter).Reset(src, nil); err != nil {
				return nil, err
			}
		} else {
			r = flate.NewReader(src)
		}
		defer flateReaders.Put(r)
		out, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("local sqlite: inflate generation: %w", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("local sqlite: unknown raw codec %q", codec)
	}
}

func (row sqlGeneration) storedGeneration() (storedGeneration, error) {
	raw, err := decodeSQLRaw(row.RawCodec, row.Raw)
	if err != nil {
		return storedGeneration{}, err
	}
	return storedGenerationFromRaw(raw, row.ConvID)
}
