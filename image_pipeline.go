package main

// The decoding pipeline: turns files into pixels, decodes each picture once,
// caches it, and prefetches what is likely to be asked for next.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/unxed/f4/imagedec"
	"github.com/unxed/f4/vfs"
	"github.com/unxed/vtui"
)

const (
	// imageCacheLimit bounds the decoded pixels kept; the cache makes going
	// back and forth instant, not a whole directory. The headroom over the
	// older budget holds the picture the reader stepped away from (kept
	// pinned), so a quick comparison does not re-decode it.
	imageCacheLimit = 256 << 20

	// imageWorkers: one lane stays free for the on-screen picture while
	// prefetch fills the rest.
	imageWorkers = 3

	// imageMaxPrefetch: at most two of the three lanes run background decodes;
	// the last is the urgent lane for the picture on screen.
	imageMaxPrefetch = 2

	// imageBytesCacheMaxFile: only files up to this size are remembered, for
	// a pinned re-decode, not a whole directory of originals.
	imageBytesCacheMaxFile = 32 << 20

	// imageBytesCacheLimit bounds the file bytes the pipeline keeps around.
	imageBytesCacheLimit = 64 << 20

	// imageIdentHeadSize is how much of a file the identification pass reads:
	// enough to reach every format's size fields, a fraction of a decode.
	imageIdentHeadSize = 64 << 10

	// imageIdentCacheLimit bounds the header identifications kept; each is a
	// few dozen bytes, so the count can be generous.
	imageIdentCacheLimit = 4096

	// imageHeadsCacheLimit bounds the raw head bytes the probe pass keeps;
	// each is at most the preview head, so the budget holds the ring and
	// window with room to spare.
	imageHeadsCacheLimit = 32 << 20
)

// ImageResult is what a picture request produces. A preview is provisional:
// the small copy the file carries, handed over while the picture decodes.
type ImageResult struct {
	Path    string
	Surface *vtui.ImageSurface
	Decoder string
	Preview bool
	Err     error

	// Sized records that the surface is a screen-sized decode, not the full
	// picture, so the viewer knows to fetch the rest on a deep zoom.
	Sized bool

	// DecodeDur is decode time incl. file read; the info panel reports it.
	DecodeDur time.Duration
}

// imageCacheKey identifies a picture. The same path in two different file
// systems is two different pictures.
type imageCacheKey struct {
	Source string
	Path   string
}

type imageEntry struct {
	res   ImageResult
	bytes int64
}

// imageWaiter is somebody waiting for a picture. UI requests are answered on
// the UI thread; a background caller is answered where the decode happened.
type imageWaiter struct {
	fn func(ImageResult)
	ui bool
}

type imageJob struct {
	key     imageCacheKey
	v       vfs.VFS
	path    string
	ctx     context.Context
	urgent  bool
	started bool
	// startedPrefetch is set when the job begins as a background decode (what
	// the prefetch lane cap counts).
	startedPrefetch bool
	waiters         []imageWaiter
}

// imageProbeJob is one header read shared by identification and the
// embedded-preview extraction: the read size is chosen by whether a preview
// is wanted, and both answers come out of the same bytes.
type imageProbeJob struct {
	cancel        context.CancelFunc
	previewWanted bool
	started       bool
}

type imageBytesJob struct {
	generation uint64
	done       chan struct{}
	data       []byte
	err        error
}

// imageLoader turns a path into pixels. The pipeline keeps it as a field so
// that tests do not need a file system.
type imageLoader func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error)

// ImagePipeline decodes pictures in the background and caches the results.
type ImagePipeline struct {
	mu      sync.Mutex
	jobs    map[imageCacheKey]*imageJob
	queue   []*imageJob
	busy    int
	workers int

	// busyPrefetch counts the running background decodes; pump never lets it
	// pass imageMaxPrefetch.
	busyPrefetch int

	// surfaceCache holds decoded pixels, bounded by bytes.
	surfaceCache *imageLRU[imageCacheKey, *imageEntry]
	// previews holds embedded thumbnails, bounded by count.
	previews *imageLRU[imageCacheKey, ImageResult]

	// idents holds header identifications (dimensions, orientation), the
	// cheap pass that runs over far more pictures than the decode prefetch.
	idents *imageLRU[imageCacheKey, imagedec.ImageHead]

	// heads holds the raw head bytes a probe read, so the preview and the
	// identification share one transfer instead of two.
	heads *imageLRU[imageCacheKey, []byte]
	// probeJobs are cancellable header reads shared by both passes.
	probeJobs map[imageCacheKey]*imageProbeJob

	// bytesCache shares one transfer between preview and full decode.
	bytesCache *imageLRU[imageCacheKey, []byte]
	bytesJobs  map[imageCacheKey]*imageBytesJob
	bytesGen   uint64
	clearGen   uint64

	// prevPin is the picture the reader stepped away from, kept in the
	// surface cache so flipping back for a comparison does not re-decode it.
	prevPin *imageCacheKey

	load     imageLoader
	preview  imageLoader
	dispatch func(func())
}

// ImagePipe is the pipeline the application uses.
var ImagePipe = NewImagePipeline()

func NewImagePipeline() *ImagePipeline {
	p := &ImagePipeline{
		jobs:         make(map[imageCacheKey]*imageJob),
		workers:      imageWorkers,
		surfaceCache: newImageLRU[imageCacheKey, *imageEntry](imageCacheLimit, func(e *imageEntry) int64 { return e.bytes }),
		previews:     newImageLRU[imageCacheKey, ImageResult](imagePreviewCacheLimit, nil),
		idents:       newImageLRU[imageCacheKey, imagedec.ImageHead](imageIdentCacheLimit, nil),
		heads:        newImageLRU[imageCacheKey, []byte](imageHeadsCacheLimit, func(d []byte) int64 { return int64(len(d)) }),
		bytesCache:   newImageLRU[imageCacheKey, []byte](imageBytesCacheLimit, func(d []byte) int64 { return int64(len(d)) }),
		bytesJobs:    make(map[imageCacheKey]*imageBytesJob),
		probeJobs:    make(map[imageCacheKey]*imageProbeJob),
		dispatch:     func(fn func()) { vtui.FrameManager.PostTask(fn) },
	}
	p.load = p.loadWithCache
	p.preview = p.previewWithCache
	return p
}

// previewWithCache cuts the preview out of bytes already in hand instead of
// reopening the file: the whole picture, then the probe's head, then a fresh
// read.
func (p *ImagePipeline) previewWithCache(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
	if data, ok := p.CachedBytes(v, path); ok && len(data) > 0 {
		return imagePreviewFromHead(data)
	}
	// The probe pass read the head already; the thumbnail is cut from the
	// same bytes, no second transfer. A thumbnail beyond the head (rare)
	// falls through to a fresh read.
	if head, ok := p.cachedHead(v, path); ok {
		if surf, name, err := imagePreviewFromHead(head); err == nil {
			return surf, name, nil
		}
	}
	return imageQuickPreview(ctx, v, path)
}

// cachedHead returns the raw head bytes the probe pass read for a picture,
// so the preview needs no new transfer.
func (p *ImagePipeline) cachedHead(v vfs.VFS, path string) ([]byte, bool) {
	key := imageCacheKey{Source: imageSource(v), Path: path}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.heads.get(key)
}

// loadWithCache decodes through the byte cache, so a picture decoded once is
// not transferred again.
func (p *ImagePipeline) loadWithCache(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
	// A path-rendering decoder (the shell) needs no bytes; reading a video
	// into memory to hand over its path would trip the byte cap. The shell
	// resolves a real local file; anything else falls through to the bytes.
	if d, ok := imagedec.PathDecoderFor(path); ok {
		if surf, name, err := imagedec.DecodeImageFromPath(ctx, path, d); err == nil {
			return surf, name, nil
		}
		// The shell failed (a virtual FS has no real path). Don't read a
		// video whole for a frame — that's how the "too large" refusal
		// surfaced — just skip its preview quietly.
		if imagedec.IsVideoFile(path) {
			return nil, "", imagedec.ErrVideoPreviewUnavailable
		}
	}
	data, err := p.FileBytes(ctx, v, path)
	if err != nil {
		return nil, "", err
	}
	return imagedec.LoadImageForBytes(ctx, path, data, "")
}

// imageSource names the file system a path belongs to.
func imageSource(v vfs.VFS) string {
	if v == nil {
		return ""
	}
	if t, ok := v.(vfs.TitleProvider); ok {
		if title := t.GetTitle(); title != "" {
			return fmt.Sprintf("%T:%s", v, title)
		}
	}
	return fmt.Sprintf("%T", v)
}

// Cached returns a picture that has already been decoded.
func (p *ImagePipeline) Cached(v vfs.VFS, path string) (ImageResult, bool) {
	key := imageCacheKey{Source: imageSource(v), Path: path}

	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.surfaceCache.get(key)
	if !ok {
		return ImageResult{}, false
	}
	return entry.res, true
}

// PreviewSync returns the best picture that can be had without decoding the
// whole file: one that is already decoded, a thumbnail seen earlier, or the
// thumbnail the file carries inside itself. The second value says whether
// there is anything at all to show.
func (p *ImagePipeline) PreviewSync(ctx context.Context, v vfs.VFS, path string) (ImageResult, bool) {
	ctx = nonNilImageContext(ctx)
	if res, ok := p.Cached(v, path); ok {
		return res, true
	}
	key := imageCacheKey{Source: imageSource(v), Path: path}

	p.mu.Lock()
	res, ok := p.previews.get(key)
	p.mu.Unlock()
	if ok {
		return res, true
	}

	surf, decoder, err := p.preview(ctx, v, path)
	if err != nil || surf == nil || !surf.Valid() {
		return ImageResult{}, false
	}
	res = ImageResult{Path: path, Surface: surf, Decoder: decoder, Preview: true}

	p.mu.Lock()
	p.previews.put(key, res)
	p.mu.Unlock()
	return res, true
}

// PreviewPrefetch asks the probe pass to extract the embedded thumbnails of
// the given pictures in the background: one header read covers both the
// preview and the identification of each. The list replaces the previous
// one, like Prefetch replaces the decode queue.
func (p *ImagePipeline) PreviewPrefetch(v vfs.VFS, paths []string) {
	source := imageSource(v)
	wanted := make(map[imageCacheKey]bool, len(paths))
	for _, path := range paths {
		wanted[imageCacheKey{Source: source, Path: path}] = true
	}

	p.mu.Lock()
	// Only the thumbnail requests are this pass's to cancel; a probe doing
	// identification alone belongs to the wider window.
	for key, job := range p.probeJobs {
		if !wanted[key] && job.previewWanted {
			job.cancel()
			delete(p.probeJobs, key)
		}
	}
	p.mu.Unlock()

	for _, path := range paths {
		if imagedec.IsVideoFile(path) || !canEmbeddedPreview(path) {
			continue
		}
		p.requestProbe(v, path, imageCacheKey{Source: source, Path: path}, true)
	}
}

// IdentifyPrefetch asks the probe pass to read the headers of the given
// pictures in the background and remember their dimensions and orientation.
// It is the cheap pass that covers far more pictures than the decode
// prefetch, so the gallery and the viewer know a picture's size before it
// decodes. The list replaces the previous one, like Prefetch replaces the
// decode queue.
func (p *ImagePipeline) IdentifyPrefetch(v vfs.VFS, paths []string) {
	source := imageSource(v)
	wanted := make(map[imageCacheKey]bool, len(paths))
	for _, path := range paths {
		wanted[imageCacheKey{Source: source, Path: path}] = true
	}

	p.mu.Lock()
	// A probe already reading a thumbnail also answers the identification;
	// only the identification-only probes are this pass's to cancel.
	for key, job := range p.probeJobs {
		if !wanted[key] && !job.previewWanted {
			job.cancel()
			delete(p.probeJobs, key)
		}
	}
	p.mu.Unlock()

	for _, path := range paths {
		if imagedec.IsVideoFile(path) {
			continue
		}
		p.requestProbe(v, path, imageCacheKey{Source: source, Path: path}, false)
	}
}

// Identified returns the cached header identification of a picture, or the
// dimensions of one already decoded.
func (p *ImagePipeline) Identified(v vfs.VFS, path string) (imagedec.ImageHead, bool) {
	key := imageCacheKey{Source: imageSource(v), Path: path}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.surfaceCache.peek(key); ok {
		return imagedec.ImageHead{Width: e.res.Surface.Width, Height: e.res.Surface.Height}, true
	}
	h, ok := p.idents.get(key)
	return h, ok
}

// IdentifiedHead is the header identification alone, without preferring a
// decoded surface: the EXIF orientation survives the full decode this way,
// so the viewer can keep reporting it without a jump.
func (p *ImagePipeline) IdentifiedHead(v vfs.VFS, path string) (imagedec.ImageHead, bool) {
	key := imageCacheKey{Source: imageSource(v), Path: path}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idents.get(key)
}

// requestProbe starts one header read for a picture, shared by both passes:
// the identification always comes out of it, the embedded preview too when
// a preview is wanted. An existing probe covers the request; a thumbnail
// wanted by a probe that has not started yet upgrades it.
func (p *ImagePipeline) requestProbe(v vfs.VFS, path string, key imageCacheKey, wantPreview bool) {
	p.mu.Lock()
	if wantPreview {
		if _, ok := p.surfaceCache.peek(key); ok {
			p.mu.Unlock()
			return
		}
		if _, ok := p.previews.peek(key); ok {
			p.mu.Unlock()
			return
		}
	} else if _, ok := p.idents.peek(key); ok {
		p.mu.Unlock()
		return
	}
	if job := p.probeJobs[key]; job != nil {
		if wantPreview && !job.started {
			job.previewWanted = true
		}
		p.mu.Unlock()
		return
	}
	// The identification pass may already have read this file's head (the
	// gallery identifies the whole folder before the viewer opens); the
	// thumbnail is cut from those bytes, no second transfer. A thumbnail
	// beyond the head falls through to a deeper probe.
	if wantPreview {
		if head, ok := p.heads.peek(key); ok {
			p.mu.Unlock()
			if surf, name, err := imagePreviewFromHead(head); err == nil && surf != nil && surf.Valid() {
				p.mu.Lock()
				p.previews.put(key, ImageResult{Path: path, Surface: surf, Decoder: name, Preview: true})
				p.mu.Unlock()
				return
			}
			p.mu.Lock()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &imageProbeJob{cancel: cancel, previewWanted: wantPreview}
	p.probeJobs[key] = job
	p.mu.Unlock()

	go func(job *imageProbeJob, ctx context.Context, key imageCacheKey, path string) {
		p.probeOne(ctx, v, path, key, job)
		p.mu.Lock()
		if p.probeJobs[key] == job {
			delete(p.probeJobs, key)
		}
		p.mu.Unlock()
	}(job, ctx, key, path)
}

// probeOne reads a file's head once, and from those bytes answers both the
// identification and (for JPEGs) the embedded preview: one transfer covers
// the cheap pass and the thumbnail. A cancelled or unreadable file is
// skipped quietly: identification is never worth an error message.
func (p *ImagePipeline) probeOne(ctx context.Context, v vfs.VFS, path string, key imageCacheKey, job *imageProbeJob) {
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	previewWanted := job.previewWanted
	job.started = true
	p.mu.Unlock()

	limit := imageIdentHeadSize
	if previewWanted {
		// A thumbnail can sit deep in the header; read far enough for it.
		limit = imagePreviewHeadSize
	}
	head, err := imageReadHead(ctx, v, path, limit)
	if err != nil {
		return
	}
	h, ok := imagedec.ProbeImageHead(head)
	if !ok {
		return
	}
	// Carry the file stat along with the header: the OSD's size and time
	// lines then arrive with the identification, ahead of the decode, with
	// no separate round trip. A failed stat leaves the fields zero, and the
	// viewer falls back to its own async stat.
	if item, err := v.Stat(ctx, path); err == nil {
		h.Size, h.MTime = item.Size, item.MTime
	}

	p.mu.Lock()
	p.heads.put(key, head)
	p.idents.put(key, h)
	p.mu.Unlock()

	if !previewWanted {
		return
	}
	surf, name, err := imagePreviewFromHead(head)
	if err != nil || surf == nil || !surf.Valid() {
		return
	}
	p.mu.Lock()
	p.previews.put(key, ImageResult{Path: path, Surface: surf, Decoder: name, Preview: true})
	p.mu.Unlock()
}

// Load asks for a picture. A cached one is handed over on the calling thread;
// otherwise the callback runs on the UI thread when ready. Shared requests
// share one decoding job.
func (p *ImagePipeline) Load(v vfs.VFS, path string, done func(ImageResult)) {
	if res, ok := p.Cached(v, path); ok {
		if done != nil {
			done(res)
		}
		return
	}
	p.request(v, path, true, imageWaiter{fn: done, ui: true}, context.Background())
}

// LoadSync decodes a picture and waits; callers already run in the background.
func (p *ImagePipeline) LoadSync(ctx context.Context, v vfs.VFS, path string) ImageResult {
	return p.loadSync(nonNilImageContext(ctx), v, path, true)
}

// LoadTileSync is LoadSync for a gallery tile: never preempts the picture on
// screen, which matters when a grid full of big files is opening.
func (p *ImagePipeline) LoadTileSync(ctx context.Context, v vfs.VFS, path string) ImageResult {
	return p.loadSync(nonNilImageContext(ctx), v, path, false)
}

func nonNilImageContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (p *ImagePipeline) loadSync(ctx context.Context, v vfs.VFS, path string, urgent bool) ImageResult {
	ctx = nonNilImageContext(ctx)
	if res, ok := p.Cached(v, path); ok {
		return res
	}
	ch := make(chan ImageResult, 1)
	p.request(v, path, urgent, imageWaiter{fn: func(res ImageResult) { ch <- res }}, ctx)

	select {
	case res := <-ch:
		return res
	case <-ctx.Done():
		return ImageResult{Path: path, Err: ctx.Err()}
	}
}

// Prefetch decodes pictures nobody asked for yet; the list replaces the
// previous one, so a far neighbour gives up its queue slot.
func (p *ImagePipeline) Prefetch(v vfs.VFS, paths []string) {
	source := imageSource(v)
	wanted := make(map[imageCacheKey]bool, len(paths))
	for _, path := range paths {
		if !imagedec.IsVideoFile(path) {
			wanted[imageCacheKey{Source: source, Path: path}] = true
		}
	}

	p.mu.Lock()
	kept := p.queue[:0]
	for _, job := range p.queue {
		// A job somebody is waiting for is not a prefetch any more.
		if len(job.waiters) == 0 && !wanted[job.key] {
			delete(p.jobs, job.key)
			continue
		}
		kept = append(kept, job)
	}
	p.queue = kept
	p.mu.Unlock()

	for _, path := range paths {
		if imagedec.IsVideoFile(path) {
			continue
		}
		key := imageCacheKey{Source: source, Path: path}
		p.mu.Lock()
		_, cached := p.surfaceCache.peek(key)
		p.mu.Unlock()
		if cached {
			continue
		}
		p.request(v, path, false, imageWaiter{}, context.Background())
	}
}

// PinPrevious keeps the picture the reader stepped away from in the surface
// cache: only the last one is pinned, the next navigation replaces it.
func (p *ImagePipeline) PinPrevious(v vfs.VFS, path string) {
	key := imageCacheKey{Source: imageSource(v), Path: path}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prevPin != nil && *p.prevPin != key {
		p.surfaceCache.unpin(*p.prevPin)
	}
	p.prevPin = &key
	p.surfaceCache.pin(key)
}

// Invalidate forgets one picture, so that the next request decodes it again.
func (p *ImagePipeline) Invalidate(v vfs.VFS, path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bytesGen++
	key := imageCacheKey{Source: imageSource(v), Path: path}
	p.surfaceCache.delete(key)
	p.previews.delete(key)
	p.idents.delete(key)
	p.heads.delete(key)
	p.bytesCache.delete(key)
	delete(p.bytesJobs, key)
	if job := p.probeJobs[key]; job != nil {
		job.cancel()
		delete(p.probeJobs, key)
	}
}

// Clear forgets every picture.
func (p *ImagePipeline) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bytesGen++
	p.clearGen++
	for key := range p.bytesJobs {
		delete(p.bytesJobs, key)
	}
	for key, job := range p.probeJobs {
		job.cancel()
		delete(p.probeJobs, key)
	}
	p.surfaceCache.clear()
	p.previews.clear()
	p.idents.clear()
	p.heads.clear()
	p.bytesCache.clear()
}

// CacheStats reports how much the pipeline is holding on to.
func (p *ImagePipeline) CacheStats() (int, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.surfaceCache.len(), p.surfaceCache.total()
}

func (p *ImagePipeline) request(v vfs.VFS, path string, urgent bool, w imageWaiter, ctx context.Context) {
	ctx = nonNilImageContext(ctx)
	key := imageCacheKey{Source: imageSource(v), Path: path}

	p.mu.Lock()
	defer p.mu.Unlock()

	if job, ok := p.jobs[key]; ok {
		if w.fn != nil {
			job.waiters = append(job.waiters, w)
		}
		if urgent {
			job.urgent = true
			// An on-screen request may take over a queued prefetch before it
			// starts; after that the worker context is fixed (changing it
			// would race the worker's snapshot).
			if !job.started {
				job.ctx = ctx
			}
		}
		return
	}

	job := &imageJob{key: key, v: v, path: path, ctx: ctx, urgent: urgent}
	if w.fn != nil {
		job.waiters = append(job.waiters, w)
	}
	p.jobs[key] = job
	p.queue = append(p.queue, job)
	p.pump()
}

// pump starts as many queued jobs as there are free workers. The caller
// holds the lock.
func (p *ImagePipeline) pump() {
	for p.busy < p.workers {
		job := p.nextJob()
		if job == nil {
			return
		}
		p.busy++
		job.startedPrefetch = !job.urgent
		if job.startedPrefetch {
			p.busyPrefetch++
		}
		go p.run(job)
	}
}

// nextJob takes the most deserving job from the queue: an urgent one now, a
// prefetch later. A prefetch only starts while its lanes have room, so an
// on-screen request always finds a free worker.
func (p *ImagePipeline) nextJob() *imageJob {
	best := -1
	for i, job := range p.queue {
		if !job.urgent && p.busyPrefetch >= imageMaxPrefetch {
			continue
		}
		if best < 0 || (job.urgent && !p.queue[best].urgent) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	job := p.queue[best]
	p.queue = append(p.queue[:best], p.queue[best+1:]...)
	return job
}

func (p *ImagePipeline) run(job *imageJob) {
	// Mark started and snapshot the owner context; a later urgent waiter may
	// join but cannot replace cancellation semantics.
	p.mu.Lock()
	job.started = true
	ctx := job.ctx
	gen := p.clearGen
	p.mu.Unlock()

	// A decode the reader stepped away from is wasted: it would push a live
	// picture out of the cache.
	if err := ctx.Err(); err != nil {
		p.done(job, ImageResult{Path: job.path, Err: err})
		return
	}

	start := time.Now()
	surf, decoder, err := p.load(ctx, job.v, job.path)
	res := ImageResult{Path: job.path, Surface: surf, Decoder: decoder, Err: err, DecodeDur: time.Since(start)}
	if err == nil && (surf == nil || !surf.Valid()) {
		res.Err = fmt.Errorf("image pipeline: %w", imagedec.ErrEmptyDecoderResult)
		res.Surface = nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		res.Err = ctxErr
		res.Surface = nil
		p.done(job, res)
		return
	}

	p.mu.Lock()
	delete(p.jobs, job.key)
	p.busy--
	if job.startedPrefetch {
		p.busyPrefetch--
	}
	if err == nil && surf != nil && surf.Valid() && p.clearGen == gen {
		p.store(job.key, res)
	}
	waiters := job.waiters
	job.waiters = nil
	p.pump()
	p.mu.Unlock()

	p.dispatchWaiters(waiters, res)
}

// done finishes a job whose decode was abandoned or never started, waking
// waiters still listening. The caller holds no lock.
func (p *ImagePipeline) done(job *imageJob, res ImageResult) {
	p.mu.Lock()
	delete(p.jobs, job.key)
	p.busy--
	if job.startedPrefetch {
		p.busyPrefetch--
	}
	waiters := job.waiters
	job.waiters = nil
	p.pump()
	p.mu.Unlock()
	p.dispatchWaiters(waiters, res)
}

// dispatchWaiters runs a finished job's waiters, UI ones on the UI thread.
// The caller holds no lock.
func (p *ImagePipeline) dispatchWaiters(waiters []imageWaiter, res ImageResult) {
	var onUI []imageWaiter
	for _, w := range waiters {
		if w.ui {
			onUI = append(onUI, w)
			continue
		}
		w.fn(res)
	}
	if len(onUI) > 0 {
		p.dispatch(func() {
			for _, w := range onUI {
				w.fn(res)
			}
		})
	}
}

// store puts a picture into the cache and throws out the ones nobody has
// looked at for the longest. The caller holds the lock.
func (p *ImagePipeline) store(key imageCacheKey, res ImageResult) {
	bytes := int64(res.Surface.Width) * int64(res.Surface.Height) * 4
	p.surfaceCache.put(key, &imageEntry{res: res, bytes: bytes})
}

// FileBytes shares recent reads and joins concurrent misses.
func (p *ImagePipeline) FileBytes(ctx context.Context, v vfs.VFS, path string) ([]byte, error) {
	ctx = nonNilImageContext(ctx)
	key := imageCacheKey{Source: imageSource(v), Path: path}

	p.mu.Lock()
	if data, ok := p.bytesCache.get(key); ok {
		p.mu.Unlock()
		return data, nil
	}
	if job := p.bytesJobs[key]; job != nil {
		p.mu.Unlock()
		select {
		case <-job.done:
			return job.data, job.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	job := &imageBytesJob{generation: p.bytesGen, done: make(chan struct{})}
	p.bytesJobs[key] = job
	p.mu.Unlock()

	data, err := imagedec.ImageReadFileBytes(ctx, v, path)

	p.mu.Lock()
	if err == nil && job.generation == p.bytesGen && len(data) <= imageBytesCacheMaxFile {
		p.bytesCache.put(key, data)
	}
	job.data, job.err = data, err
	if p.bytesJobs[key] == job {
		delete(p.bytesJobs, key)
	}
	close(job.done)
	p.mu.Unlock()
	return data, err
}

// CachedBytes hands out file bytes already in hand, without reading anything.
func (p *ImagePipeline) CachedBytes(v vfs.VFS, path string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := imageCacheKey{Source: imageSource(v), Path: path}
	data, ok := p.bytesCache.get(key)
	if !ok {
		return nil, false
	}
	return data, true
}

// ImageNeighbourhood returns the pictures nearest the current position on
// both sides, forward first.
func ImageNeighbourhood(paths []string, index, radius int) []string {
	if index < 0 || index >= len(paths) || radius <= 0 {
		return nil
	}
	out := make([]string, 0, 2*radius)
	for step := 1; step <= radius; step++ {
		if i := index + step; i < len(paths) {
			out = append(out, paths[i])
		}
		if i := index - step; i >= 0 {
			out = append(out, paths[i])
		}
	}
	return out
}
