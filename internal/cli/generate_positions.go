package cli

import (
	"compress/flate"
	"context"
	"fmt"

	"github.com/readium/go-toolkit/pkg/fetcher"
)

// The EPUB toolkit uses ceil(compressed entry size / 1024) for reflowable
// positions. Directory resources have no archive size and otherwise silently
// collapse to one position per document. Measure a ZIP-compatible DEFLATE stream
// without writing an archive or reading/compressing the publication's media.
// This estimates the original compressed size; ZIP compression settings can
// differ, so archive inputs always retain the toolkit's original strategy.
type directoryPositionStrategy struct {
	ctx context.Context
	err error
}

func (s *directoryPositionStrategy) PositionCount(resource fetcher.Resource) uint {
	if s.err != nil {
		return 1
	}
	var size positionByteCounter
	writer, err := flate.NewWriter(&size, flate.DefaultCompression)
	if err != nil {
		s.err = err
		return 1
	}
	_, readErr := resource.Stream(s.ctx, writer, 0, 0)
	closeErr := writer.Close()
	if readErr != nil {
		s.err = fmt.Errorf("read %s: %w", resource.Link().Href.String(), readErr)
		return 1
	}
	if closeErr != nil {
		s.err = fmt.Errorf("compress %s: %w", resource.Link().Href.String(), closeErr)
		return 1
	}
	return uint(max((size+1023)/1024, 1))
}

type positionByteCounter uint64

func (c *positionByteCounter) Write(b []byte) (int, error) {
	*c += positionByteCounter(len(b))
	return len(b), nil
}
