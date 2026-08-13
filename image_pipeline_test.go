package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/unxed/f4/imagedecoders"
	"github.com/unxed/f4/vfs"
	"github.com/unxed/vtui"
)

func imageTestSurface(w, h int) *vtui.ImageSurface {
	return vtui.NewImageSurfaceFromPix(w, h, w*4, make([]byte, w*h*4))
}

// newTestPipeline answers on the calling thread and decodes with a stub, so
// that a test needs neither a file system nor a running interface.
func newTestPipeline(load imageLoader) *ImagePipeline {
	p := NewImagePipeline()
	p.load = load
	p.dispatch = func(fn func()) { fn() }
	return p
}

func TestImagePipelineDecodesOnce(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})

	p := newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return imageTestSurface(4, 4), "stub", nil
	})

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		p.Load(nil, "a.png", func(res ImageResult) {
			if res.Err != nil {
				t.Errorf("decoding failed: %v", res.Err)
			}
			wg.Done()
		})
	}
	close(release)
	wg.Wait()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("two requests for one picture must share a decode, got %d", got)
	}

	// A picture already in hand is handed over on the spot.
	answered := false
	p.Load(nil, "a.png", func(res ImageResult) { answered = res.Surface.Valid() })
	if !answered {
		t.Error("a cached picture must be delivered before Load returns")
	}

	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("a cached picture must not be decoded again, got %d decodes", got)
	}
}

func TestImagePipelineEvictsTheOldest(t *testing.T) {
	p := newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		return imageTestSurface(100, 100), "stub", nil
	})
	// Room for two pictures of forty thousand bytes each.
	p.limit = 90000

	for _, name := range []string{"a", "b", "c"} {
		if res := p.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	count, bytes := p.CacheStats()
	if count != 2 || bytes != 80000 {
		t.Errorf("cache holds %d pictures, %d bytes", count, bytes)
	}
	if _, ok := p.Cached(nil, "a"); ok {
		t.Error("the least recently used picture must have been thrown out")
	}
	if _, ok := p.Cached(nil, "c"); !ok {
		t.Error("the newest picture must stay")
	}
}

func TestImagePipelineKeepsTheNewestPictureHoweverLarge(t *testing.T) {
	p := newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		return imageTestSurface(100, 100), "stub", nil
	})
	p.limit = 1

	if res := p.LoadSync(context.Background(), nil, "a"); res.Err != nil {
		t.Fatalf("decoding failed: %v", res.Err)
	}
	if count, _ := p.CacheStats(); count != 1 {
		t.Errorf("the picture on screen must survive its own size, cache holds %d", count)
	}
}

func TestImagePipelinePrefetchFollowsTheView(t *testing.T) {
	started := make(chan string, 8)
	finished := make(chan string, 8)
	gate := make(chan struct{})

	p := newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		started <- path
		<-gate
		finished <- path
		return imageTestSurface(2, 2), "stub", nil
	})
	p.workers = 1

	p.Prefetch(nil, []string{"a", "b", "c"})
	if first := <-started; first != "a" {
		t.Fatalf("the nearest neighbour goes first, got %q", first)
	}

	// The view moved: b and c are not neighbours any more, d is.
	p.Prefetch(nil, []string{"a", "d"})
	close(gate)

	if second := <-started; second != "d" {
		t.Errorf("a neighbour left behind must give up its place, got %q", second)
	}
	seenFinished := map[string]bool{}
	for len(seenFinished) < 2 {
		seenFinished[<-finished] = true
	}
	select {
	case extra := <-started:
		t.Errorf("nothing else should have been decoded, got %q", extra)
	default:
	}
}

func TestImagePipelineUrgentStartsWhilePrefetchIsInFlight(t *testing.T) {
	started := make(chan string, 8)
	release := make(chan struct{})

	p := newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		started <- path
		<-release
		return imageTestSurface(2, 2), "stub", nil
	})
	p.workers = 3

	// Three prefetch jobs: two take the prefetch lanes, the third waits.
	p.Prefetch(nil, []string{"a", "b", "c"})
	first := map[string]bool{<-started: true, <-started: true}
	if !first["a"] || !first["b"] {
		t.Fatalf("the first two prefetches must run, got %v", first)
	}

	// An on-screen request lands on the lane the cap kept free.
	urgent := make(chan ImageResult, 1)
	go func() { urgent <- p.LoadSync(context.Background(), nil, "urgent") }()
	select {
	case path := <-started:
		if path != "urgent" {
			t.Errorf("the urgent request must start on the free lane, got %q", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the urgent request never started")
	}
	close(release)
	if res := <-urgent; res.Err != nil {
		t.Errorf("the urgent request failed: %v", res.Err)
	}
}

func TestImagePipelinePreviewPrefetch(t *testing.T) {
	var mu sync.Mutex
	previews := 0
	loads := 0
	p := newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		mu.Lock()
		loads++
		mu.Unlock()
		return imageTestSurface(8, 8), "stub", nil
	})
	p.preview = func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		mu.Lock()
		previews++
		mu.Unlock()
		return imageTestSurface(4, 4), imagePreviewDecoder, nil
	}

	// The ring beyond the decoded neighbours: only thumbnails, no decodes.
	p.PreviewPrefetch(nil, []string{"far1", "far2"})

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := previews
		mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the previews never arrived, %d extracted", n)
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	gotLoads := loads
	mu.Unlock()
	if gotLoads != 0 {
		t.Errorf("preview prefetch must not decode whole files, got %d decodes", gotLoads)
	}
	p.mu.Lock()
	running := len(p.previewJobs)
	p.mu.Unlock()
	if running != 0 {
		t.Errorf("%d preview jobs are still running", running)
	}
	if _, ok := p.previews[imageCacheKey{Path: "far1"}]; !ok {
		t.Error("the extracted thumbnail must be remembered")
	}
}

// byteCacheVFS counts the transfers behind a fixed file, so the byte cache
// can be measured the way the plan's OpenPictureReads bench did.
type byteCacheVFS struct {
	vfs.VFS
	data    []byte
	opens   int
	readAts int
}

func (v *byteCacheVFS) Open(ctx context.Context, path string) (vfs.ReadAtCloser, error) {
	v.opens++
	return &byteCacheFile{v: v}, nil
}

type byteCacheFile struct{ v *byteCacheVFS }

func (f *byteCacheFile) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	f.v.readAts++
	n := copy(p, f.v.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *byteCacheFile) Read(ctx context.Context, p []byte) (int, error) { return 0, io.EOF }
func (f *byteCacheFile) Close() error                                    { return nil }
func (f *byteCacheFile) Size() int64                                     { return int64(len(f.v.data)) }

func TestImagePipelineFileBytesAreReadOnce(t *testing.T) {
	v := &byteCacheVFS{data: make([]byte, 4096)}
	for i := range v.data {
		v.data[i] = byte(i)
	}

	p := newTestPipeline(nil)
	data, err := p.FileBytes(context.Background(), v, "a.jpg")
	if err != nil {
		t.Fatalf("the first read failed: %v", err)
	}
	if v.opens != 1 || v.readAts != 1 {
		t.Errorf("the first read: %d opens, %d read-ats", v.opens, v.readAts)
	}

	// A second read of the same picture comes out of the cache.
	again, err := p.FileBytes(context.Background(), v, "a.jpg")
	if err != nil {
		t.Fatalf("the second read failed: %v", err)
	}
	if v.opens != 1 || v.readAts != 1 {
		t.Errorf("the cached read must not transfer again: %d opens, %d read-ats", v.opens, v.readAts)
	}
	if !bytes.Equal(data, again) {
		t.Error("the cached bytes differ from the file")
	}

	// The preview head is cut from the same bytes.
	if head, ok := p.CachedBytes(v, "a.jpg"); !ok || len(head) != 4096 {
		t.Errorf("CachedBytes = %d bytes, ok=%v", len(head), ok)
	}

	// Invalidating the picture forgets its bytes as well.
	p.Invalidate(v, "a.jpg")
	if _, ok := p.CachedBytes(v, "a.jpg"); ok {
		t.Error("invalidating the picture must drop its file bytes")
	}
}

func TestImagePipelineDoesNotCacheFailures(t *testing.T) {
	var mu sync.Mutex
	calls := 0

	p := newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return nil, "", errors.New("broken file")
		}
		return imageTestSurface(4, 4), "stub", nil
	})

	if res := p.LoadSync(context.Background(), nil, "a"); res.Err == nil {
		t.Fatal("the failure must be reported")
	}
	if res := p.LoadSync(context.Background(), nil, "a"); res.Err != nil {
		t.Fatalf("the second attempt must be made: %v", res.Err)
	}

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Errorf("a failure must not be remembered as a result, got %d decodes", got)
	}
}

func TestImagePipelineInvalidateAndClear(t *testing.T) {
	p := newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		return imageTestSurface(4, 4), "stub", nil
	})

	p.LoadSync(context.Background(), nil, "a")
	p.LoadSync(context.Background(), nil, "b")

	p.Invalidate(nil, "a")
	if _, ok := p.Cached(nil, "a"); ok {
		t.Error("an invalidated picture must be gone")
	}
	if _, ok := p.Cached(nil, "b"); !ok {
		t.Error("only the named picture may be dropped")
	}

	p.Clear()
	if count, bytes := p.CacheStats(); count != 0 || bytes != 0 {
		t.Errorf("after Clear the cache holds %d pictures, %d bytes", count, bytes)
	}
}

func TestImagePipelineCancellationAfterDecodeDoesNotSucceed(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	p := newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		close(started)
		<-cancelled
		return imageTestSurface(4, 4), "stub", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan ImageResult, 1)
	go func() { result <- p.LoadSync(ctx, nil, "after-decode") }()
	<-started
	close(cancelled)
	cancel()
	res := <-result
	if !errors.Is(res.Err, context.Canceled) || res.Surface != nil {
		t.Fatalf("cancelled decode result = surface %v, err %v", res.Surface, res.Err)
	}
}

func TestImagePipelineLoadSyncHonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	p := newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		<-release
		return imageTestSurface(4, 4), "stub", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := p.LoadSync(ctx, nil, "a"); res.Err == nil {
		t.Error("a cancelled request must not wait for the decoder")
	}
}

func TestImageNeighbourhood(t *testing.T) {
	paths := []string{"0", "1", "2", "3", "4"}

	if got := ImageNeighbourhood(paths, 2, 2); !reflect.DeepEqual(got, []string{"3", "1", "4", "0"}) {
		t.Errorf("neighbours of the middle: %v", got)
	}
	if got := ImageNeighbourhood(paths, 0, 1); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("neighbours of the first: %v", got)
	}
	if got := ImageNeighbourhood(paths, 4, 1); !reflect.DeepEqual(got, []string{"3"}) {
		t.Errorf("neighbours of the last: %v", got)
	}
	if got := ImageNeighbourhood(paths, -1, 2); got != nil {
		t.Errorf("a position outside the list has no neighbours: %v", got)
	}
	if got := ImageNeighbourhood(paths, 2, 0); got != nil {
		t.Errorf("a radius of zero has no neighbours: %v", got)
	}
}
