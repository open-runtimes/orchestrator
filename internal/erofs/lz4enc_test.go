package erofs

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/pierrec/lz4/v4"
)

// lz4encCorpora produces inputs spanning the encoder's edge cases: empty,
// tiny, incompressible, highly repetitive (overlap matches), long matches
// with extended lengths, and realistic mixed content.
func lz4encCorpora(t testing.TB) map[string][]byte {
	t.Helper()
	rnd := rand.New(rand.NewSource(11))
	random := make([]byte, 300000)
	rnd.Read(random)
	structured := make([]byte, 0, 1<<20)
	words := []string{"config", "handler", "return", "buffer", "stream", "value"}
	for len(structured) < 1<<20 {
		structured = append(structured, []byte(words[rnd.Intn(len(words))])...)
		structured = append(structured, byte(rnd.Intn(256)))
	}
	return map[string][]byte{
		"empty":         nil,
		"one":           []byte("x"),
		"short":         []byte("hello world"),
		"min-limit":     bytes.Repeat([]byte("ab"), 8),
		"zeros":         make([]byte, 100000),
		"overlap":       bytes.Repeat([]byte("a"), 70000),
		"period3":       bytes.Repeat([]byte("abc"), 30000),
		"random":        random,
		"structured":    structured,
		"long-literals": append(append([]byte{}, random[:100000]...), bytes.Repeat([]byte("z"), 50000)...),
		"far-matches":   append(append([]byte{}, structured[:70000]...), structured[:70000]...),
	}
}

// TestLZ4HCEncoderRoundTrip differentially validates both encoder modes
// against the reference decoder on every corpus.
func TestLZ4HCEncoderRoundTrip(t *testing.T) {
	var e lz4HCEncoder
	modes := map[string]func([]byte, []byte) (int, error){
		"lazy": e.CompressBlock,
		"opt":  e.CompressBlockOpt,
	}
	for mode, compress := range modes {
		for name, src := range lz4encCorpora(t) {
			t.Run(mode+"/"+name, func(t *testing.T) {
				dst := make([]byte, lz4.CompressBlockBound(len(src)))
				n, err := compress(src, dst)
				if err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					t.Skip("did not fit (incompressible)")
				}
				out := make([]byte, len(src))
				m, err := lz4.UncompressBlock(dst[:n], out)
				if err != nil {
					t.Fatalf("reference decoder rejected output: %v", err)
				}
				if m != len(src) || !bytes.Equal(out[:m], src) {
					t.Fatalf("round trip mismatch: got %d bytes, want %d", m, len(src))
				}
			})
		}
	}
}

// TestLZ4HCEncoderFuzzish hammers the encoder with random slices of random
// and structured data at many lengths.
func TestLZ4HCEncoderFuzzish(t *testing.T) {
	rnd := rand.New(rand.NewSource(42))
	base := make([]byte, 1<<20)
	rnd.Read(base)
	// overlay compressible runs
	for range 3000 {
		off := rnd.Intn(len(base) - 300)
		b := byte(rnd.Intn(256))
		runLen := 100 + rnd.Intn(200)
		for j := range runLen {
			base[off+j] = b
		}
	}
	var e lz4HCEncoder
	// Sized so the suite stays affordable under -race on small CI runners
	// (the whole erofs package must clear CI's 10-minute test timeout);
	// broader corpora run through the round-trip tests above.
	for i := range 50 {
		l := rnd.Intn(1 << 13)
		off := rnd.Intn(len(base) - l)
		src := base[off : off+l]
		dst := make([]byte, lz4.CompressBlockBound(len(src)))
		for mode, compress := range map[string]func([]byte, []byte) (int, error){"lazy": e.CompressBlock, "opt": e.CompressBlockOpt} {
			n, err := compress(src, dst)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				continue
			}
			out := make([]byte, len(src))
			m, err := lz4.UncompressBlock(dst[:n], out)
			if err != nil || m != len(src) || !bytes.Equal(out[:m], src) {
				t.Fatalf("case %d mode %s (len %d): decode err=%v m=%d", i, mode, l, err, m)
			}
		}
	}
}

// TestLZ4HCEncoderRatio pins that the encoder compresses at least as well as
// pierrec's HC on structured content (the reason it exists).
func TestLZ4HCEncoderRatio(t *testing.T) {
	src := lz4encCorpora(t)["structured"]
	var e lz4HCEncoder
	dst := make([]byte, lz4.CompressBlockBound(len(src)))
	ours, err := e.CompressBlock(src, dst)
	if err != nil || ours == 0 {
		t.Fatalf("compress: n=%d err=%v", ours, err)
	}
	hc := lz4.CompressorHC{Level: lz4.Level9}
	theirs, err := hc.CompressBlock(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ours=%d pierrec=%d (%.2f%%)", ours, theirs, 100*float64(ours)/float64(theirs))
	if ours > theirs {
		t.Fatalf("encoder ratio regressed below pierrec HC: %d > %d", ours, theirs)
	}
}

func TestLZ4HCEncoderOptLimit(t *testing.T) {
	src := lz4encCorpora(t)["structured"]
	dst := make([]byte, lz4.CompressBlockBound(len(src)))
	var full lz4HCEncoder
	want, err := full.CompressBlockOpt(src, dst)
	if err != nil || want == 0 {
		t.Fatalf("full compression: n=%d err=%v", want, err)
	}

	var fits lz4HCEncoder
	n, within, err := fits.CompressBlockOptLimit(src, dst, want)
	if err != nil || !within || n != want {
		t.Fatalf("exact limit: n=%d within=%v err=%v, want %d", n, within, err, want)
	}
	var stops lz4HCEncoder
	if n, within, err := stops.CompressBlockOptLimit(src, dst, want-1); err != nil || within {
		t.Fatalf("undersized limit: n=%d within=%v err=%v", n, within, err)
	}
	var noOutput lz4HCEncoder
	if n, within, err := noOutput.CompressBlockOptLimit(src[:8], nil, 100); err != nil || n != 0 || within {
		t.Fatalf("no output buffer: n=%d within=%v err=%v", n, within, err)
	}
}

// TestLZ4HCEncoderOptBatchFlush regresses a corruption where an optimal-parse
// batch flush left hash-chain entries at positions beyond the parse point;
// the finders then returned a "match" referencing the current or a future
// position, emitting an invalid sequence (offset 0). The corpus mixes
// word-salad with runs long enough to trip the greedy fast path and batch
// bounds, mirroring the packed-inode content that first exposed it.
func TestLZ4HCEncoderOptBatchFlush(t *testing.T) {
	rnd := rand.New(rand.NewSource(3))
	words := []string{"config", "handler", "router", "parse", "validate", "schema"}
	var b bytes.Buffer
	for b.Len() < 400_000 {
		for range 200 {
			b.WriteString(words[rnd.Intn(len(words))])
			b.WriteByte(byte('0' + rnd.Intn(10)))
		}
		// Long verbatim repeats of earlier content: matches beyond the
		// greedy cutoff, forcing fast-path emits and batch flushes.
		start := rnd.Intn(b.Len() / 2)
		end := min(start+8_000+rnd.Intn(30_000), b.Len())
		b.Write(bytes.Clone(b.Bytes()[start:end]))
	}
	src := b.Bytes()
	var e lz4HCEncoder
	dst := make([]byte, lz4.CompressBlockBound(len(src)))
	n, err := e.CompressBlockOpt(src, dst)
	if err != nil || n == 0 {
		t.Fatalf("compress: n=%d err=%v", n, err)
	}
	out := make([]byte, len(src))
	m, err := lz4.UncompressBlock(dst[:n], out)
	if err != nil || m != len(src) || !bytes.Equal(out[:m], src) {
		t.Fatalf("round trip: decoded=%d err=%v", m, err)
	}
}

func BenchmarkLZ4HCEncoder(b *testing.B) {
	src := lz4encCorpora(b)["structured"]
	dst := make([]byte, lz4.CompressBlockBound(len(src)))
	var encoder lz4HCEncoder
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := encoder.CompressBlockOpt(src, dst); err != nil {
			b.Fatal(err)
		}
	}
}
