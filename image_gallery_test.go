package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/unxed/f4/imagedec"
	"github.com/unxed/f4/vfs"
	"github.com/unxed/vtinput"
	"github.com/unxed/vtui"
)

func TestImageGalleryLayoutAndScrolling(t *testing.T) {
	g := &imageGallery{}
	g.layout(80, 23)
	if g.cols != 4 || g.rows != 2 {
		t.Fatalf("an 80x23 window holds a 4x2 grid of tiles, got %dx%d", g.cols, g.rows)
	}

	// Twenty pictures are five rows of four, of which two fit on screen.
	g.scrollTo(19, 20)
	if g.top != 3 {
		t.Errorf("the last row has to come into view, got top %d", g.top)
	}
	g.scrollTo(0, 20)
	if g.top != 0 {
		t.Errorf("the first row has to come into view, got top %d", g.top)
	}

	// A grid that is not even full has nothing to scroll.
	g.scrollTo(2, 3)
	if g.top != 0 {
		t.Errorf("a grid with three pictures must not scroll, got top %d", g.top)
	}

	g.move(-100, 20)
	if g.cursor != 0 {
		t.Errorf("the cursor stops at the first picture, got %d", g.cursor)
	}
	g.move(100, 20)
	if g.cursor != 19 {
		t.Errorf("the cursor stops at the last picture, got %d", g.cursor)
	}
}

func TestImageViewGallerySelectionReachesThePanel(t *testing.T) {
	withStubPipeline(t, 8, 8)

	iv := newTestImageView(t, 40, 20)
	iv.path = "b.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 1)

	var reported []string
	iv.OnSelect = func(path string, on bool) {
		reported = append(reported, fmt.Sprintf("%s=%v", path, on))
	}

	press := func(vk uint16) {
		t.Helper()
		if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vk}) {
			t.Fatalf("key %d was not handled", vk)
		}
	}

	press(vtinput.VK_F12)
	if iv.gal == nil || iv.gal.cursor != 1 {
		t.Fatal("the grid opens on the picture that was on screen")
	}

	press(vtinput.VK_INSERT)
	if !iv.selected["b.png"] {
		t.Error("Ins did not pick the picture under the cursor")
	}
	if iv.gal.cursor != 2 {
		t.Errorf("Ins moves on, the cursor is at %d", iv.gal.cursor)
	}

	press(vtinput.VK_INSERT)
	press(vtinput.VK_DELETE)
	if iv.selected["c.png"] {
		t.Error("Del did not unpick the picture under the cursor")
	}

	want := []string{"b.png=true", "c.png=true", "c.png=false"}
	if strings.Join(reported, " ") != strings.Join(want, " ") {
		t.Errorf("the panel was told %v, expected %v", reported, want)
	}

	// Escape leaves the grid without leaving the viewer.
	press(vtinput.VK_ESCAPE)
	if iv.gal != nil {
		t.Error("Escape did not close the grid")
	}
	if iv.IsDone() {
		t.Error("Escape closed the whole viewer instead of the grid")
	}
}

func TestImageViewGalleryDrawsATileForEachPicture(t *testing.T) {
	withStubPipeline(t, 8, 8)

	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 40, 20)
	iv.path = "a.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 0)
	iv.ToggleGallery()

	// Two thumbnails have arrived; fetching the third is a background job
	// that has no business running on the drawing path, and this test is not
	// about it.
	iv.gal.thumbs["a.png"] = vtui.NewImageSurface(8, 8)
	iv.gal.thumbs["b.png"] = vtui.NewImageSurface(8, 8)
	iv.gal.asked["c.png"] = true

	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()

	if n := scr.Graphics().Len(); n != 2 {
		t.Errorf("two thumbnails are ready, %d were placed", n)
	}

	// Every tile is captioned, whether its thumbnail has arrived or not. The
	// grid starts at the very top of the screen, so the first caption is on
	// the tile's bottom row, one short of the tile height.
	row := ScreenRow(scr, imageTileRows-1, 0, 79)
	for _, name := range []string{"a.png", "b.png", "c.png"} {
		if !strings.Contains(row, name) {
			t.Errorf("the caption row is %q, without %s", row, name)
		}
	}
}

func TestImageViewGalleryEnterOpensTheCursor(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "a.png"
	iv.SetSiblings([]string{"a.png", "b.png"}, 0)
	if res := ImagePipe.LoadSync(nil, nil, "b.png"); res.Err != nil {
		t.Fatalf("b.png: %v", res.Err)
	}

	iv.ToggleGallery()
	iv.gal.move(1, 2)
	iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_RETURN})

	if iv.gal != nil {
		t.Error("Enter has to leave the grid")
	}
	if iv.path != "b.png" || iv.index != 1 {
		t.Errorf("Enter opened %q at %d", iv.path, iv.index)
	}
}

// gallerySiblings builds a named sibling list for the request-order tests.
func gallerySiblings(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("pic%02d.png", i)
	}
	return out
}

func TestGalleryRingDistance(t *testing.T) {
	g := &imageGallery{cols: 4, cursor: 5} // row 1, column 1
	cases := []struct {
		idx  int
		dist int
	}{
		{5, 0},  // the cursor itself
		{1, 1},  // row 0, column 1
		{4, 1},  // row 1, column 0
		{6, 1},  // row 1, column 2
		{9, 1},  // row 2, column 1
		{0, 1},  // row 0, column 0: Chebyshev max(1,1)
		{2, 1},  // row 0, column 2: Chebyshev max(1,1)
		{3, 2},  // row 0, column 3: Chebyshev max(2,1)
		{14, 2}, // row 3, column 2: Chebyshev max(2,2)
	}
	for _, tc := range cases {
		if d := galleryRing(g, tc.idx); d != tc.dist {
			t.Errorf("slot %d is ring %d, got %d", tc.idx, tc.dist, d)
		}
	}
}

// TestGalleryThumbnailBudgetAndSpiral verifies G2: opening the grid must not
// fire every visible tile at once — a frame is allowed galleryThumbBudget
// decodes, nearest the cursor first.
func TestGalleryThumbnailBudgetAndSpiral(t *testing.T) {
	iv := newTestImageView(t, 100, 100)
	siblings := gallerySiblings(30)
	iv.SetSiblings(siblings, 7) // the cursor sits on pic07
	iv.ToggleGallery()

	g := iv.gal
	g.layout(80, 45) // 4 columns, 5 rows: slots 0..19 visible
	g.scrollTo(g.cursor, len(siblings))

	iv.requestVisibleThumbs()
	if n := len(g.asked); n > galleryThumbBudget {
		t.Fatalf("one frame asked for %d thumbnails, budget is %d", n, galleryThumbBudget)
	}

	// The cursor is slot 7: row 1, column 3 of a 4-wide grid. Its rings of
	// Chebyshev distance over the visible 20 slots are
	//   ring 0: {7}
	//   ring 1: {2,3,6,10,11}
	//   ring 2: {1,5,9,13,14,15}
	//   ring 3: {0,4,8,12,16,17,18,19}
	// A budget of 12 takes ring 0..2 whole and stops there.
	for _, name := range []string{
		"pic07.png",                                                     // ring 0
		"pic02.png", "pic03.png", "pic06.png", "pic10.png", "pic11.png", // ring 1
		"pic01.png", "pic05.png", "pic09.png", "pic13.png", "pic14.png", "pic15.png", // ring 2
	} {
		if !g.asked[name] {
			t.Errorf("ring 0/1/2 tile %s was not asked in the first frame", name)
		}
	}
	// pic00 and the rest of ring 3 wait for a later frame.
	for _, name := range []string{"pic00.png", "pic04.png", "pic16.png", "pic19.png"} {
		if g.asked[name] {
			t.Errorf("%s is ring 3 and must wait for a later frame", name)
		}
	}

	// The second frame picks up the eight ring-3 tiles and stops.
	before := len(g.asked)
	iv.requestVisibleThumbs()
	if len(g.asked) != 20 {
		t.Errorf("after two frames all 20 visible tiles should be asked, got %d", len(g.asked))
	}
	if len(g.asked) > before && !g.asked["pic00.png"] {
		t.Error("the second frame did not pick up the ring-3 tiles")
	}
}

// TestGalleryThumbnailBudgetNotExceededOnScreen verifies the same budget
// holds when the request goes through the drawing path with a real pipeline.
func TestGalleryThumbnailBudgetNotExceededOnScreen(t *testing.T) {
	withStubPipeline(t, 8, 8)

	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 40, 20)
	iv.path = "a.png"
	iv.SetSiblings(gallerySiblings(30), 0)
	iv.ToggleGallery()

	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()

	if n := len(iv.gal.asked); n > galleryThumbBudget {
		t.Errorf("a screenful of tiles asked for %d decodes in one frame, budget is %d", n, galleryThumbBudget)
	}
}

// TestImageViewGalleryShowsIdentifiedDimensions locks in the header-pass
// consumer: a tile whose identification arrived before its thumbnail shows
// the dimensions on the row above the caption.
func TestImageViewGalleryShowsIdentifiedDimensions(t *testing.T) {
	withStubPipeline(t, 8, 8)

	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 40, 20)
	iv.path = "a.png"
	// c.png is beyond the decode prefetch (index 0 prefetches only b.png), so
	// it stays identified-only: exactly the case the header pass serves.
	// d.png is a rotated picture (orientation 6): the label shows the swapped
	// size, like its decoded surface.
	iv.SetSiblings([]string{"a.png", "b.png", "c.png", "d.png"}, 0)

	ImagePipe.mu.Lock()
	ImagePipe.idents.put(imageCacheKey{Path: "c.png"}, imagedec.ImageHead{Width: 1600, Height: 900})
	ImagePipe.idents.put(imageCacheKey{Path: "d.png"}, imagedec.ImageHead{Width: 1600, Height: 900, Orientation: 6})
	ImagePipe.mu.Unlock()

	iv.ToggleGallery()
	// Mark every tile as already asked, so no thumbnail decode races the
	// draw: the dimensions row must come from the identification alone.
	for _, p := range []string{"a.png", "b.png", "c.png", "d.png"} {
		iv.gal.asked[p] = true
	}
	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()

	// Slot 2 sits at column 36; its dimensions row is one above the caption.
	row := ScreenRow(scr, imageTileRows-2, imageTileCols*2, imageTileCols*3-1)
	if !strings.Contains(row, "1600x900") {
		t.Errorf("the dimensions row is %q, without 1600x900", row)
	}
	// Slot 3 is the rotated picture: 1600x900 with orientation 6 shows as
	// 900x1600.
	row = ScreenRow(scr, imageTileRows-2, imageTileCols*3, imageTileCols*4-1)
	if !strings.Contains(row, "900x1600") {
		t.Errorf("the rotated dimensions row is %q, without 900x1600", row)
	}
}

// TestGalleryVideoTileSkipped locks in the video-tile guard: a gallery tile
// for a video is skipped outright (the shell frame fetch can block for a long
// time), so the grid shows only the caption and never reads the container.
func TestGalleryVideoTileSkipped(t *testing.T) {
	// A size decoder that would eagerly claim mp4 if the guard slipped.
	imagedec.RegisterImageDecoder(imagedec.ImageDecoder{
		Name:       "test-video-size",
		Priority:   5000,
		Extensions: []string{"mp4"},
		Decode: func(data []byte) (*vtui.ImageSurface, error) {
			return imageTestSurface(1, 1), nil
		},
		DecodeSize: func(ctx context.Context, path string, data []byte, w, h int) (*vtui.ImageSurface, error) {
			return imageTestSurface(w, h), nil
		},
	})
	defer imagedec.UnregisterImageDecoder("test-video-size")
	registerVideoPathDecoder(t, func(context.Context, string, []byte) (*vtui.ImageSurface, error) {
		return imageTestSurface(2, 2), nil
	})

	v := &byteCacheVFS{data: make([]byte, 4096)}
	p := NewImagePipeline()
	p.dispatch = func(fn func()) { fn() } // answer on the calling thread
	oldPipe := ImagePipe
	ImagePipe = p
	defer func() { ImagePipe = oldPipe }()

	res := loadGalleryTile(context.Background(), v, "clip.mp4", 32, 32)
	if res.Err == nil {
		t.Fatal("a video tile must be skipped, not decoded")
	}
	if v.opens != 0 || v.readAts != 0 {
		t.Errorf("a video tile must never be read whole: %d opens, %d read-ats", v.opens, v.readAts)
	}
}

// TestGalleryTileFallbackDownscales locks in the fallback-tile downscale: a
// full decode for a tile is shrunk to tile size once, so the grid's draws
// filter tiny pixels instead of the whole picture every frame.
func TestGalleryTileFallbackDownscales(t *testing.T) {
	v := &byteCacheVFS{data: make([]byte, 4096)}
	p := newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		return imageTestSurface(64, 64), "stub", nil
	})
	oldPipe := ImagePipe
	ImagePipe = p
	defer func() { ImagePipe = oldPipe }()

	res := loadGalleryTile(context.Background(), v, "a.png", 32, 32)
	if res.Err != nil {
		t.Fatalf("the tile failed: %v", res.Err)
	}
	if res.Surface == nil || res.Surface.Width != 32 || res.Surface.Height != 32 {
		t.Fatalf("the tile must be shrunk to 32x32, got %dx%d", res.Surface.Width, res.Surface.Height)
	}
	if res.Decoder != "stub" {
		t.Errorf("the decoder name must survive the downscale, got %q", res.Decoder)
	}
}
func TestPanelSelectionByName(t *testing.T) {
	fp := &FileSystemPanel{
		entries: []*fileEntry{
			{VFSItem: vfs.VFSItem{Name: "..", IsDir: true}},
			{VFSItem: vfs.VFSItem{Name: "a.png"}},
		},
		selectedItems: map[string]bool{},
	}

	if !fp.SetSelectedByName("a.png", true) {
		t.Fatal("the panel does show that entry")
	}
	if !fp.IsNameSelected("a.png") {
		t.Error("the entry did not get picked")
	}
	if fp.SetSelectedByName("gone.png", true) {
		t.Error("a name the panel does not show cannot be picked")
	}
	fp.SetSelectedByName("a.png", false)
	if fp.IsNameSelected("a.png") {
		t.Error("the entry did not get unpicked")
	}
}
