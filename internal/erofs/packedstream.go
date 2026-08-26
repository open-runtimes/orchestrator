package erofs

import (
	"crypto/sha256"
	"io"
	"sync"
)

// zStreamPacker accepts a stream incrementally and packs fixed-size segments
// in parallel. It exists for the shared packed inode: fragment collection can
// feed compression directly instead of writing all raw bytes to a temporary
// file and reading them back in a second pass.
type zStreamPacker struct {
	z       *zState
	profile zProfile
	jobs    chan zStreamJob
	queue   []chan zStreamResult
	tail    []byte
	zi      zInfo
	wg      sync.WaitGroup
	closed  bool
	workers int
}

type zStreamJob struct {
	buf   []byte
	final bool
	res   chan zStreamResult
}

type zStreamResult struct {
	packedSegment
	buf []byte
}

func newZStreamPacker(z *zState, profile zProfile) *zStreamPacker {
	p := &zStreamPacker{
		z:       z,
		profile: profile,
		jobs:    make(chan zStreamJob),
		workers: z.workerCount(profile),
	}
	p.tail = p.getBuffer()
	for range p.workers {
		p.wg.Go(func() {
			for job := range p.jobs {
				worker := z.getPackWorker(profile)
				spans, err := z.packStreamSegment(job.buf, profile, worker.compress, worker.finalize, &worker.probe, &worker.kept, job.final)
				z.putPackWorker(profile, worker)
				job.res <- zStreamResult{packedSegment: packedSegment{spans: spans, err: err}, buf: job.buf}
			}
		})
	}
	return p
}

func (p *zStreamPacker) Write(buf []byte) (int, error) {
	written := len(buf)
	for len(buf) > 0 {
		n := min(len(buf), zSegmentSize-len(p.tail))
		p.tail = append(p.tail, buf[:n]...)
		buf = buf[n:]
		if len(p.tail) == zSegmentSize {
			if err := p.submit(false); err != nil {
				return written - len(buf), err
			}
		}
	}
	return written, nil
}

// ReadFull appends exactly size bytes from r directly into the segment tail.
// Unlike io.Copy through Write, it avoids an intermediate whole-file buffer.
func (p *zStreamPacker) ReadFull(r io.Reader, size int) error {
	for size > 0 {
		n := min(size, zSegmentSize-len(p.tail))
		off := len(p.tail)
		p.tail = p.tail[:off+n]
		if _, err := io.ReadFull(r, p.tail[off:]); err != nil {
			p.tail = p.tail[:off]
			return err
		}
		size -= n
		if len(p.tail) == zSegmentSize {
			if err := p.submit(false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *zStreamPacker) submit(final bool) error {
	if len(p.queue) >= p.workers {
		if err := p.collectOne(); err != nil {
			return err
		}
	}
	res := make(chan zStreamResult, 1)
	p.jobs <- zStreamJob{buf: p.tail, final: final, res: res}
	p.queue = append(p.queue, res)
	if final {
		p.tail = nil
	} else {
		p.tail = p.getBuffer()
	}
	return nil
}

func (p *zStreamPacker) collectOne() error {
	res := <-p.queue[0]
	p.queue = p.queue[1:]
	defer p.putBuffer(res.buf)
	defer p.z.releasePackedSpans(p.profile, res.spans)
	if res.err != nil {
		return res.err
	}
	for _, span := range res.spans {
		ext, err := p.z.storeSpanKeyed(span.raw, span.comp, span.key)
		if err != nil {
			return err
		}
		p.zi.extents = append(p.zi.extents, ext)
		p.zi.totalBlocks += uint32(ext.blocks)
	}
	return nil
}

func (p *zStreamPacker) getBuffer() []byte {
	return p.z.getSegmentBuffer()
}

func (p *zStreamPacker) putBuffer(buf []byte) {
	p.z.putSegmentBuffer(buf)
}

func (p *zStreamPacker) Finish() (*zInfo, error) {
	if len(p.tail) > 0 {
		if err := p.submit(true); err != nil {
			p.Close()
			return nil, err
		}
	} else if p.tail != nil {
		p.putBuffer(p.tail)
		p.tail = nil
	}
	p.closeWorkers()
	for len(p.queue) > 0 {
		if err := p.collectOne(); err != nil {
			p.discardQueued()
			return nil, err
		}
	}
	return &p.zi, nil
}

func (p *zStreamPacker) Close() {
	p.closeWorkers()
	if p.tail != nil {
		p.putBuffer(p.tail)
		p.tail = nil
	}
	p.discardQueued()
}

func (p *zStreamPacker) closeWorkers() {
	if p.closed {
		return
	}
	p.closed = true
	close(p.jobs)
	p.wg.Wait()
}

func (p *zStreamPacker) discardQueued() {
	for len(p.queue) > 0 {
		res := <-p.queue[0]
		p.queue = p.queue[1:]
		p.z.releasePackedSpans(p.profile, res.spans)
		p.putBuffer(res.buf)
	}
}

// packStreamSegment is the shared segment packing loop used by an incremental
// stream. Segment boundaries deliberately match compressParallel, preserving
// the existing extent and image-size behavior.
func (z *zState) packStreamSegment(buf []byte, profile zProfile, compress zCompressor, finalize zFinalizer, probe, kept *[]byte, final bool) ([]packedSpan, error) {
	ratio := 0.5
	var spans []packedSpan
	window := buf
	for len(window) > 0 {
		candidate := window[:min(len(window), profile.maxExtentSize)]
		span, comp, err := z.packSpan(compress, candidate, profile, probe, kept, final && len(candidate) == len(window), &ratio)
		if err != nil {
			return spans, err
		}
		comp, err = refineSpan(finalize, window[:span], comp, *probe, z.blockSize)
		if err != nil {
			return spans, err
		}
		packed := packedSpan{raw: window[:span], comp: z.stageCompressed(profile, comp)}
		if z.dedupe != nil {
			packed.key = sha256.Sum256(packed.raw)
		}
		spans = append(spans, packed)
		window = window[span:]
	}
	return spans, nil
}
