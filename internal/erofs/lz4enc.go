package erofs

import "math/bits"

// A high-compression LZ4 block encoder. The vendored pierrec/lz4 decoder (and
// the kernel) read its output like any other lz4 block; this encoder exists
// because pierrec's HC match finder compresses erofs artifact corpora ~4%
// worse than liblz4's, which is the difference between losing and beating
// mkfs.erofs on image size. It uses hash chains over 4-byte prefixes with
// bounded-attempt search, one-step-lazy match selection, and backward match
// extension — the same shape as liblz4's HC levels.

const (
	lz4MinMatch     = 4
	lz4LastLiterals = 5  // spec: the last 5 bytes are always literals
	lz4MFLimit      = 12 // spec: no match may start within the last 12 bytes
	lz4MaxDistance  = 65535
	lz4HashLog      = 16
	lz4HashShift    = 32 - lz4HashLog
	// lz4Attempts bounds the hash-chain walk per position. Recency-ordered
	// chains mean the first several dozen candidates carry nearly all the
	// ratio; measurements on artifact corpora show returns vanish long
	// before liblz4's level-9 depth of 256.
	lz4Attempts = 64
	// lz4GoodEnough ends a chain walk once a match this long is found;
	// past it, a longer match cannot change the parse much.
	lz4GoodEnough = 768
)

type lz4HCEncoder struct {
	// head maps a 4-byte-prefix hash to the most recent position + 1.
	head [1 << lz4HashLog]int32
	// chain maps a position to the backward delta to the previous position
	// with the same hash (0 = end of chain). Sized to the source.
	chain []uint16
	// optimal-parse scratch (allocated on first use)
	opt  []lz4OptEntry
	seqs [][3]int32 // backtracked {start, mlen, off} tuples, reused
}

func lz4Hash(u uint32) uint32 {
	return (u * 2654435761) >> lz4HashShift
}

func load32(b []byte, i int) uint32 {
	return uint32(b[i]) | uint32(b[i+1])<<8 | uint32(b[i+2])<<16 | uint32(b[i+3])<<24
}

// matchLen returns the length of the common prefix of a and b.
func matchLen(a, b []byte) int {
	n := 0
	for len(a) >= 8 && len(b) >= 8 {
		x := load64(a) ^ load64(b)
		if x != 0 {
			return n + trailingZeroBytes(x)
		}
		a, b = a[8:], b[8:]
		n += 8
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return n + i
		}
	}
	return n + min(len(a), len(b))
}

func load64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func trailingZeroBytes(x uint64) int {
	return bits.TrailingZeros64(x) / 8
}

// insert adds position i to the hash chains. Re-inserting a position is a
// no-op (a self-link would truncate the chain).
func (e *lz4HCEncoder) insert(src []byte, i int) {
	h := lz4Hash(load32(src, i))
	prev := int(e.head[h]) - 1
	if prev == i {
		return
	}
	if prev >= 0 && i-prev <= lz4MaxDistance {
		e.chain[i] = uint16(i - prev)
	} else {
		e.chain[i] = 0
	}
	e.head[h] = int32(i) + 1
}

// widerMatch walks the chain at position i and returns the best match,
// ranked by total length after extending each candidate backwards (no
// further back than minStart, the current literal anchor) as well as
// forwards. Returns the adjusted match start, its reference position, and
// the total length (0 = none). This mirrors liblz4's
// LZ4HC_InsertAndGetWiderMatch, which is where its ratio edge over simpler
// hash-chain matchers comes from.
func (e *lz4HCEncoder) widerMatch(src []byte, i, minStart int) (int, int, int) {
	bestStart, bestRef, bestLen := 0, 0, 0
	limit := i - lz4MaxDistance
	cand := int(e.head[lz4Hash(load32(src, i))]) - 1
	// A flushed optimal-parse batch leaves positions at or beyond i in the
	// chains; matches may only reference strictly earlier data.
	for cand >= i {
		d := int(e.chain[cand])
		if d == 0 {
			return 0, 0, 0
		}
		cand -= d
	}
	srcEnd := len(src) - lz4LastLiterals
	// The DP prices matches from their true start as well, so long backward
	// extensions mostly duplicate edges it already has; a small cap keeps
	// the win (shifting a match start a few bytes) at a fraction of the cost.
	maxBack := min(i-minStart, 64)
	first := load32(src, i)
	for attempts := lz4Attempts; attempts > 0 && cand >= 0 && cand >= limit; attempts-- {
		// Cheap rejects: the 16-bit hash collides often, so filter on the
		// real 4-byte prefix; and a candidate with no backward potential
		// must extend past the best match's endpoint to matter.
		viable := bestLen == 0 || (cand != 0 && src[cand-1] == src[i-1]) ||
			(i+bestLen < srcEnd && src[cand+bestLen] == src[i+bestLen])
		// Kept inline rather than extracted: this is the encoder's hottest
		// loop and the body has loops of its own, so a helper cannot inline
		// (measured ~3% on whole-image builds).
		if viable && load32(src, cand) == first {
			fwd := matchLen(src[cand:min(cand+srcEnd-i, srcEnd)], src[i:srcEnd])
			if fwd >= lz4MinMatch && fwd+maxBack > bestLen {
				back := 0
				for back < maxBack && cand-back > 0 && src[i-back-1] == src[cand-back-1] {
					back++
				}
				if fwd+back > bestLen {
					bestLen = fwd + back
					bestStart = i - back
					bestRef = cand - back
					if bestLen >= lz4GoodEnough {
						break // long matches saturate the token economics
					}
				}
			}
		}
		d := int(e.chain[cand])
		if d == 0 {
			break
		}
		cand -= d
	}
	if bestLen < lz4MinMatch {
		return 0, 0, 0
	}
	return bestStart, bestRef, bestLen
}

// CompressBlock compresses src into dst as one raw LZ4 block. It returns 0
// (not an error) when the output would not fit in dst, mirroring the
// pierrec CompressBlock contract used by the packer.
func (e *lz4HCEncoder) CompressBlock(src, dst []byte) (int, error) {
	if len(src) < lz4MFLimit+lz4MinMatch {
		return e.emitTail(src, dst, 0, 0)
	}
	if cap(e.chain) < len(src) {
		e.chain = make([]uint16, len(src))
	} else {
		e.chain = e.chain[:len(src)]
	}
	clear(e.head[:])

	mfLimit := len(src) - lz4MFLimit
	d := 0 // dst write position
	anchor := 0
	i := 0
	e.insert(src, 0)
	for i < mfLimit {
		start, ref, l := e.widerMatch(src, i, anchor)
		if l == 0 {
			i++
			if i < mfLimit {
				e.insert(src, i)
			}
			continue
		}
		// Overlap-resolving lazy loop: probe for a wider match starting
		// inside the current one (from two bytes before its end, the last
		// point a 4-byte match could begin and still overlap). If one is
		// found, truncate the current match up to where the better one
		// starts — or drop it entirely when too little remains — and repeat.
		for start+l-2 > start && start+l-2 < mfLimit {
			probeAt := start + l - 2
			for j := max(i+1, probeAt-2); j <= probeAt && j < mfLimit; j++ {
				e.insert(src, j)
			}
			s2, r2, l2 := e.widerMatch(src, probeAt, start)
			if l2 <= l || s2+l2 <= start+l {
				break
			}
			if s2 <= start {
				// The wider match covers this one entirely; replace it.
				start, ref, l = s2, r2, l2
				continue
			}
			keep := s2 - start
			if keep >= lz4MinMatch {
				var ok bool
				if d, ok = emitSequence(dst, d, src[anchor:start], start-ref, keep); !ok {
					return 0, nil
				}
				anchor = start + keep
			}
			// If keep < MinMatch the covered bytes simply stay literals.
			start, ref, l = s2, r2, l2
		}

		var ok bool
		if d, ok = emitSequence(dst, d, src[anchor:start], start-ref, l); !ok {
			return 0, nil
		}
		// Index the positions the match skipped (bounded: long matches only
		// need their tail indexed for future 64K-window lookups).
		next := start + l
		from := max(i+1, next-lz4MaxDistance)
		for j := from; j < next && j < mfLimit; j++ {
			e.insert(src, j)
		}
		i = next
		anchor = next
	}
	return e.emitTail(src, dst, d, anchor)
}

// emitTail writes the closing literals-only sequence.
func (e *lz4HCEncoder) emitTail(src, dst []byte, d, anchor int) (int, error) {
	lit := len(src) - anchor
	need := 1 + lit + (lit+240)/255
	if d+need > len(dst) {
		return 0, nil
	}
	var token byte
	if lit >= 15 {
		token = 15 << 4
	} else {
		token = byte(lit) << 4
	}
	dst[d] = token
	d++
	if lit >= 15 {
		d = putLenExt(dst, d, lit-15)
	}
	d += copy(dst[d:], src[anchor:])
	return d, nil
}

// emitSequence writes one literals+match sequence; ok is false when dst is
// too small.
func emitSequence(dst []byte, d int, literals []byte, offset, mlen int) (int, bool) {
	lit := len(literals)
	need := 1 + lit + (lit+240)/255 + 2 + (mlen+240)/255 + 1
	if d+need > len(dst) {
		return 0, false
	}
	var token byte
	if lit >= 15 {
		token = 15 << 4
	} else {
		token = byte(lit) << 4
	}
	m := mlen - lz4MinMatch
	if m >= 15 {
		token |= 15
	} else {
		token |= byte(m)
	}
	dst[d] = token
	d++
	if lit >= 15 {
		d = putLenExt(dst, d, lit-15)
	}
	d += copy(dst[d:], literals)
	dst[d] = byte(offset)
	dst[d+1] = byte(offset >> 8)
	d += 2
	if m >= 15 {
		d = putLenExt(dst, d, m-15)
	}
	return d, true
}

// putLenExt writes an LZ4 extended-length field (255-terminated).
func putLenExt(dst []byte, d, v int) int {
	for v >= 255 {
		dst[d] = 255
		d++
		v -= 255
	}
	dst[d] = byte(v)
	return d + 1
}

// LZ4HCEncoderForBench exposes the encoder to the benchmark harness in tmp/.
type LZ4HCEncoderForBench = lz4HCEncoder

// --- optimal parse ---

const (
	// lz4OptWindow bounds one dynamic-programming batch. Matches whose end
	// would cross the bound trigger the batch to flush, so the arrays stay
	// small regardless of input size.
	lz4OptWindow = 4096
	// lz4SufficientLen short-circuits the parse: a match this long is taken
	// greedily (optimizing around it cannot pay for the DP cost).
	lz4SufficientLen = 4095
)

type lz4OptEntry struct {
	price  int32
	off    int32 // match offset; 0 for a literal step
	mlen   int32 // match length; 1 for a literal step
	litlen int32 // pending literal run ending here (literal steps only)
}

// litRunPrice is the encoded size of a literal run, excluding the token.
func litRunPrice(litlen int32) int32 {
	if litlen >= 15 {
		return litlen + 1 + (litlen-15)/255
	}
	return litlen
}

// seqPrice is the encoded size of a full sequence: token, offset, literal
// run, and match-length extension.
func seqPrice(litlen, mlen int32) int32 {
	price := int32(1+2) + litRunPrice(litlen)
	if mlen >= 15+lz4MinMatch {
		price += 1 + (mlen-(15+lz4MinMatch))/255
	}
	return price
}

// forwardMatch finds the best match starting exactly at i (no backward
// extension; the DP considers earlier starts through other positions).
func (e *lz4HCEncoder) forwardMatch(src []byte, i int) (int, int) {
	bestLen, bestRef := 0, 0
	limit := i - lz4MaxDistance
	cand := int(e.head[lz4Hash(load32(src, i))]) - 1
	// See widerMatch: skip stale future positions from flushed batches.
	for cand >= i {
		d := int(e.chain[cand])
		if d == 0 {
			return 0, 0
		}
		cand -= d
	}
	srcEnd := len(src) - lz4LastLiterals
	maxLen := srcEnd - i
	for attempts := lz4Attempts; attempts > 0 && cand >= 0 && cand >= limit; attempts-- {
		if bestLen == 0 || src[cand+bestLen] == src[i+bestLen] {
			if l := matchLen(src[cand:cand+maxLen], src[i:i+maxLen]); l > bestLen {
				bestLen, bestRef = l, cand
				if bestLen >= maxLen {
					break
				}
			}
		}
		d := int(e.chain[cand])
		if d == 0 {
			break
		}
		cand -= d
	}
	if bestLen < lz4MinMatch {
		return 0, 0
	}
	return bestRef, bestLen
}

// CompressBlockOpt compresses src with a chunked optimal parse: within each
// batch it prices every literal/match-length choice and backtracks the
// cheapest encoding, the same strategy as liblz4's highest levels. Output is
// a standard LZ4 block; 0 means it does not fit dst.
func (e *lz4HCEncoder) CompressBlockOpt(src, dst []byte) (int, error) {
	if len(src) < lz4MFLimit+lz4MinMatch {
		return e.emitTail(src, dst, 0, 0)
	}
	if cap(e.chain) < len(src) {
		e.chain = make([]uint16, len(src))
	} else {
		e.chain = e.chain[:len(src)]
	}
	clear(e.head[:])
	if e.opt == nil {
		e.opt = make([]lz4OptEntry, lz4OptWindow+lz4SufficientLen+1)
	}
	opt := e.opt

	mfLimit := len(src) - lz4MFLimit
	d := 0
	anchor := 0
	ip := 0
	e.insert(src, 0)
	for ip < mfLimit {
		ref, ml := e.forwardMatch(src, ip)
		if ml == 0 {
			ip++
			if ip < mfLimit {
				e.insert(src, ip)
			}
			continue
		}
		if ml >= lz4SufficientLen {
			var ok bool
			if d, ok = emitSequence(dst, d, src[anchor:ip], ip-ref, ml); !ok {
				return 0, nil
			}
			ip = e.optAdvance(src, ip, ml, mfLimit)
			anchor = ip
			continue
		}

		// Seed the batch with the first match's lengths.
		const inf = int32(1) << 30
		last := ml // furthest position with a priced parse
		opt[0] = lz4OptEntry{price: 0, mlen: 1}
		for l := 1; l < lz4MinMatch && l <= last; l++ {
			opt[l] = lz4OptEntry{price: inf, mlen: 1}
		}
		for l := lz4MinMatch; l <= ml; l++ {
			opt[l] = lz4OptEntry{price: seqPrice(0, int32(l)), off: int32(ip - ref), mlen: int32(l)}
		}
		for cur := 1; cur <= last; cur++ {
			// Literal step from cur-1.
			prev := opt[cur-1]
			if prev.price < inf {
				var litlen int32 = 1
				base := prev.price
				if prev.mlen == 1 {
					litlen = prev.litlen + 1
					base -= litRunPrice(prev.litlen)
				}
				price := base + litRunPrice(litlen)
				if cur > last || price < opt[cur].price {
					opt[cur] = lz4OptEntry{price: price, mlen: 1, litlen: litlen}
				}
			}
			if cur == last || ip+cur >= mfLimit {
				continue
			}
			e.insert(src, ip+cur)
			// Wider candidates extend backwards over already-priced
			// positions; the DP treats them as edges from their true start.
			s2, ref2, ml2 := e.widerMatch(src, ip+cur, ip)
			if ml2 == 0 {
				continue
			}
			relStart := s2 - ip
			if ml2 >= lz4SufficientLen || relStart+ml2 >= lz4OptWindow {
				// Huge match or batch bound: flush the parse up to cur, then
				// the outer loop re-finds this match.
				last = cur
				break
			}
			if opt[relStart].price >= inf {
				continue
			}
			// Price every usable length of the new match.
			var litlen int32
			base := opt[relStart].price
			if opt[relStart].mlen == 1 {
				litlen = opt[relStart].litlen
				base = opt[relStart-int(litlen)].price
			}
			off := int32(s2 - ref2)
			for l := lz4MinMatch; l <= ml2; l++ {
				price := base + seqPrice(litlen, int32(l))
				pos := relStart + l
				if pos <= cur {
					continue // already-settled positions stay as parsed
				}
				if pos > last {
					for j := last + 1; j < pos; j++ {
						opt[j] = lz4OptEntry{price: inf, mlen: 1}
					}
					last = pos
					opt[pos] = lz4OptEntry{price: price, off: off, mlen: int32(l)}
				} else if price < opt[pos].price {
					opt[pos] = lz4OptEntry{price: price, off: off, mlen: int32(l)}
				}
			}
		}

		// Backtrack the cheapest parse and emit its sequences in order.
		// Trailing literals are left for the next batch (or the tail).
		endPos := last
		for endPos > 0 && opt[endPos].mlen == 1 {
			endPos--
		}
		seqs := e.seqs[:0]
		for pos := endPos; pos > 0; {
			ent := opt[pos]
			if ent.mlen == 1 {
				pos -= int(ent.litlen)
				continue
			}
			seqs = append(seqs, [3]int32{int32(pos) - ent.mlen, ent.mlen, ent.off})
			pos -= int(ent.mlen)
		}
		e.seqs = seqs
		for si := len(seqs) - 1; si >= 0; si-- {
			start := ip + int(seqs[si][0])
			mlen := int(seqs[si][1])
			off := int(seqs[si][2])
			var ok bool
			if d, ok = emitSequence(dst, d, src[anchor:start], off, mlen); !ok {
				return 0, nil
			}
			anchor = start + mlen
		}
		if endPos == 0 {
			// Nothing but literals priced (can happen at batch bounds);
			// step forward to guarantee progress.
			endPos = 1
		}
		ip = e.optAdvance(src, ip, endPos, mfLimit)
	}
	return e.emitTail(src, dst, d, anchor)
}

// optAdvance indexes the positions a committed batch consumed and returns
// the new parse position.
func (e *lz4HCEncoder) optAdvance(src []byte, ip, consumed, mfLimit int) int {
	next := ip + consumed
	from := max(ip+1, next-lz4MaxDistance)
	for j := from; j < next && j < mfLimit; j++ {
		e.insert(src, j)
	}
	return next
}
