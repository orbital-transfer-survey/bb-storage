package buffer

import (
	"io"
	"sync"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-storage/pkg/digest"
)

type casClonedBuffer struct {
	base           Buffer
	digest         digest.Digest
	repairStrategy RepairStrategy

	lock               sync.Mutex
	consumersRemaining uint
	consumersWaiting   []chan ChunkReader
	needsValidation    bool
}

// newCASClonedBuffer creates a decorator for CAS-backed buffer objects
// that permits concurrent access to the same buffer. All consumers will
// be synchronized, meaning that they will get access to the buffer's
// contents at the same pace.
func newCASClonedBuffer(base Buffer, digest digest.Digest, repairStrategy RepairStrategy) Buffer {
	return &casClonedBuffer{
		base:           base,
		digest:         digest,
		repairStrategy: repairStrategy,

		consumersRemaining: 1,
	}
}

func (b *casClonedBuffer) GetSizeBytes() (int64, error) {
	return b.digest.GetSizeBytes(), nil
}

func (b *casClonedBuffer) toChunkReader(needsValidation bool, chunkPolicy ChunkPolicy) ChunkReader {
	b.lock.Lock()
	if b.consumersRemaining == 0 {
		panic("Attempted to obtain a chunk reader for a buffer that is already fully consumed")
	}
	b.consumersRemaining--

	// Provide constraints that this consumer desires.
	b.needsValidation = b.needsValidation || needsValidation

	// Create the underlying ChunkReader in case all consumers have
	// supplied their constraints.
	if b.consumersRemaining == 0 {
		// If there is at least one consumer that needs checksum
		// validation, we use checksum validation for everyone.
		var r ChunkReader
		if b.needsValidation {
			r = b.base.ToChunkReader(0, chunkSizeDontCare)
		} else {
			r = b.base.toUnvalidatedChunkReader(0, chunkSizeDontCare)
		}

		// Give all consumers their own ChunkReader.
		rMultiplexed := newMultiplexedChunkReader(r, len(b.consumersWaiting))
		for _, c := range b.consumersWaiting {
			c <- rMultiplexed
		}
		b.lock.Unlock()
		return newNormalizingChunkReader(rMultiplexed, chunkPolicy)
	}

	// There are other consumers that still have to supply their
	// constraints. Let the last consumer create the ChunkReader and
	// hand it out.
	c := make(chan ChunkReader, 1)
	b.consumersWaiting = append(b.consumersWaiting, c)
	b.lock.Unlock()
	return newNormalizingChunkReader(<-c, chunkPolicy)
}

func (b *casClonedBuffer) IntoWriter(w io.Writer) error {
	return intoWriterViaChunkReader(b.toChunkReader(true, chunkSizeDontCare), w)
}

func (b *casClonedBuffer) ReadAt(p []byte, off int64) (int, error) {
	return readAtViaChunkReader(b.toChunkReader(true, chunkSizeDontCare), p, off)
}

func (b *casClonedBuffer) ToActionResult(maximumSizeBytes int) (*remoteexecution.ActionResult, error) {
	return toActionResultViaByteSlice(b, maximumSizeBytes)
}

func (b *casClonedBuffer) ToByteSlice(maximumSizeBytes int) ([]byte, error) {
	return toByteSliceViaChunkReader(b.toChunkReader(true, chunkSizeDontCare), b.digest, maximumSizeBytes)
}

func (b *casClonedBuffer) ToChunkReader(off int64, chunkPolicy ChunkPolicy) ChunkReader {
	return newOffsetChunkReader(b.toChunkReader(true, chunkPolicy), off)
}

func (b *casClonedBuffer) ToReader() io.ReadCloser {
	return newChunkReaderBackedReader(b.toChunkReader(true, chunkSizeDontCare))
}

func (b *casClonedBuffer) CloneCopy(maximumSizeBytes int) (Buffer, Buffer) {
	return cloneCopyViaByteSlice(b, maximumSizeBytes)
}

func (b *casClonedBuffer) CloneStream() (Buffer, Buffer) {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.consumersRemaining == 0 {
		panic("Attempted to clone stream for a buffer that is already fully consumed")
	}
	b.consumersRemaining++
	return b, b
}

func (b *casClonedBuffer) Discard() {
	b.toChunkReader(false, chunkSizeDontCare).Close()
}

func (b *casClonedBuffer) applyErrorHandler(errorHandler ErrorHandler) (replacement Buffer, shouldRetry bool) {
	// For stream-backed buffers, it is not yet known whether they
	// may be read successfully. Wrap the buffer into one that
	// handles I/O errors upon access.
	return newCASErrorHandlingBuffer(b, errorHandler, b.digest, b.repairStrategy), false
}

func (b *casClonedBuffer) toUnvalidatedChunkReader(off int64, chunkPolicy ChunkPolicy) ChunkReader {
	return newOffsetChunkReader(b.toChunkReader(false, chunkPolicy), off)
}

func (b *casClonedBuffer) toUnvalidatedReader(off int64) io.ReadCloser {
	return newChunkReaderBackedReader(b.toUnvalidatedChunkReader(off, chunkSizeDontCare))
}
