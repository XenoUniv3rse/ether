package listcache

import (
	"context"
	"errors"
	"runtime"
	"sync"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// WalkExportPages reads a bounded export with concurrent page preparation.
// Each page is delivered in source order, and all workers join on return.
func WalkExportPages[T any](ctx context.Context, exporter frontend.LibraryExporter, viewer *frontend.Viewer, device frontend.Device, view string, pageSize int, prepare func(context.Context, frontend.LibraryExportPage) (T, error), each func(frontend.LibraryExportPage, T) error) error {
	ctx, cancel := context.WithCancel(ctx)
	type result struct {
		page frontend.LibraryExportPage
		data T
		err  error
	}
	signers := max(2, min(4, runtime.GOMAXPROCS(0)))
	// ordered carries each page's slot in read order; work hands the same
	// slot to a signer, which fills it.
	type slot struct {
		res  result
		done chan struct{}
	}
	ordered, work := make(chan *slot, signers), make(chan *slot)
	var wg sync.WaitGroup
	for range signers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sl := range work {
				if sl.res.err == nil {
					sl.res.data, sl.res.err = prepare(ctx, sl.res.page)
				}
				close(sl.done)
			}
		}()
	}
	go func() {
		defer close(work)
		defer close(ordered)
		after := frontend.RatingKey(0)
		for {
			page, err := exporter.ExportLibraryPage(ctx, viewer, device, view, after, pageSize)
			switch {
			case err != nil:
			case len(page.Items) > pageSize:
				err = errors.New("unbounded export")
			case !page.Done && page.Next <= after:
				err = errors.New("invalid export cursor")
			}
			sl := &slot{res: result{page: page, err: err}, done: make(chan struct{})}
			select {
			case ordered <- sl:
			case <-ctx.Done():
				return
			}
			select {
			case work <- sl:
			case <-ctx.Done():
				sl.res.err = ctx.Err()
				close(sl.done)
				return
			}
			if err != nil || page.Done {
				return
			}
			after = page.Next
		}
	}()
	defer func() {
		// No stage outlives the request handler.
		cancel()
		for range ordered {
		}
		wg.Wait()
	}()
	for sl := range ordered {
		select {
		case <-sl.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if sl.res.err != nil {
			return sl.res.err
		}
		if err := each(sl.res.page, sl.res.data); err != nil {
			return err
		}
		if sl.res.page.Done {
			return nil
		}
	}
	// A stage stopped before the last page: the request was cancelled.
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("incomplete export")
}
