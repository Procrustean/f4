package main

import (
	"io"
	"testing"

	"github.com/unxed/vtinput"
	"github.com/unxed/vtui"
)

// newBlockTestScreen is newBenchScreen wrapped for tests.
func newBlockTestScreen(t *testing.T) *vtui.ScreenBuf {
	t.Helper()
	// The placement tests assume the tab bar reserves the top row, so pin
	// the mode: earlier tests may have switched it globally.
	wasMode := vtui.FrameManager.WorkspaceTabMode
	vtui.FrameManager.WorkspaceTabMode = vtui.WorkspaceTabsAlways
	t.Cleanup(func() { vtui.FrameManager.WorkspaceTabMode = wasMode })
	return newBenchScreen()
}

// newBenchScreen is newBlockTestScreen without the testing.T, for benchmarks.
func newBenchScreen() *vtui.ScreenBuf {
	scr := vtui.NewScreenBuf()
	scr.Writer = io.Discard
	scr.AllocBuf(80, 25)
	scr.Graphics().SetProtocol(vtui.GraphicsNone)
	return scr
}

// solidSurface paints a w x h surface with one colour and full alpha.
func solidSurface(w, h int, rgb uint32) *vtui.ImageSurface {
	s := vtui.NewImageSurface(w, h)
	r := byte(rgb >> 16)
	g := byte(rgb >> 8)
	b := byte(rgb)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			s.SetPixel(x, y, r, g, b, 255)
		}
	}
	return s
}

func TestBlockPlacementFits(t *testing.T) {
	s, _ := fitPlacement(solidSurface(100, 100, 0xFFFFFF), 1, 2, 5, 7, 40, 10)
	// The pixel box is 40 x 20; a square fits to 20 x 20 pixels, i.e.
	// 20 cells wide and 10 tall, centred.
	if s.Cols != 20 || s.Rows != 10 {
		t.Errorf("size %dx%d cells", s.Cols, s.Rows)
	}
	if s.Col != 15 || s.Row != 7 {
		t.Errorf("origin %d,%d", s.Col, s.Row)
	}
	w, _ := fitPlacement(solidSurface(100, 50, 0xFFFFFF), 1, 2, 0, 0, 40, 10)
	if w.Cols != 40 || w.Rows != 10 {
		t.Errorf("wide picture size %dx%d", w.Cols, w.Rows)
	}
}

func TestBlockPlacementRejectsInvalid(t *testing.T) {
	if _, ok := fitPlacement(nil, 1, 2, 0, 0, 10, 10); ok {
		t.Error("nil surface must be rejected")
	}
	if _, ok := fitPlacement(solidSurface(2, 2, 0), 1, 2, 0, 0, 0, 5); ok {
		t.Error("empty box must be rejected")
	}
}

func TestBlockViewerPlacementFitsAndCentres(t *testing.T) {
	scr := newBlockTestScreen(t)
	iv := newTestImageView(t, 100, 100)
	p, ok := iv.placementForSize(scr, 1, 2)
	if !ok {
		t.Fatal("layout failed")
	}
	// 80x23 cells, pixel box 80x46 (two pixel rows per cell row):
	// the square fits to 46x46 pixels, i.e. 46x23 cells.
	if p.Cols != 46 || p.Rows != 23 {
		t.Errorf("wrong size %dx%d cells", p.Cols, p.Rows)
	}
	if p.Col != 17 || p.Row != 1 {
		t.Errorf("wrong origin %d,%d", p.Col, p.Row)
	}
	if p.SrcW != 0 || p.SrcH != 0 {
		t.Error("a fitting image must not be cropped")
	}
}

func TestBlockViewerPlacementZoomCropsAndPans(t *testing.T) {
	scr := newBlockTestScreen(t)
	iv := newTestImageView(t, 100, 100)
	iv.SetZoom(4)
	p, ok := iv.placementForSize(scr, 1, 2)
	if !ok {
		t.Fatal("layout failed")
	}
	if p.SrcW <= 0 || p.SrcW >= 100 || p.SrcH <= 0 || p.SrcH >= 100 {
		t.Fatalf("a zoomed image must show only a part of the source, got %dx%d", p.SrcW, p.SrcH)
	}
	if p.SrcX != 0 || p.SrcY != 0 {
		t.Errorf("panning should start at the origin, got %d,%d", p.SrcX, p.SrcY)
	}
	if p.Cols <= 0 || p.Cols > 80 || p.Rows <= 0 || p.Rows > 23 {
		t.Errorf("placement must stay in the window, got %dx%d", p.Cols, p.Rows)
	}
	for i := 0; i < 50; i++ {
		iv.Pan(1, 1)
	}
	p, _ = iv.placementForSize(scr, 1, 2)
	if p.SrcX+p.SrcW > 100 || p.SrcY+p.SrcH > 100 {
		t.Errorf("panning ran past the picture: %d,%d + %dx%d", p.SrcX, p.SrcY, p.SrcW, p.SrcH)
	}
}

func TestBlockDrawWritesCells(t *testing.T) {
	scr := newBlockTestScreen(t)
	// 2x4 source: one cell is 1x2 pixels, so a 2x2 cell box shows the
	// whole 2x4 surface, one pixel column per cell, two rows per cell.
	surface := vtui.NewImageSurface(2, 4)
	for x := 0; x < 2; x++ {
		surface.SetPixel(x, 0, 255, 0, 0, 255)
		surface.SetPixel(x, 1, 0, 255, 0, 255)
		surface.SetPixel(x, 2, 0, 0, 255, 255)
		surface.SetPixel(x, 3, 255, 255, 0, 255)
	}
	p, ok := fitPlacement(surface, 1, 2, 1, 1, 2, 2)
	if !ok {
		t.Fatal("layout failed")
	}
	r := &blockRender{}
	r.draw(scr, p, blockImageBack)
	// Top cell: upper half red, lower half green.
	if c := scr.GetCell(1, 1); c.Char != blockHalfChar ||
		vtui.GetRGBFore(c.Attributes) != 0xFF0000 ||
		vtui.GetRGBBack(c.Attributes) != 0x00FF00 {
		t.Errorf("top cell: char %d, fg %06X, bg %06X",
			c.Char, vtui.GetRGBFore(c.Attributes), vtui.GetRGBBack(c.Attributes))
	}
	// Bottom cell: upper half blue, lower half yellow.
	if c := scr.GetCell(1, 2); vtui.GetRGBBack(c.Attributes) != 0xFFFF00 {
		t.Errorf("bottom cell: bg %06X, want %06X", vtui.GetRGBBack(c.Attributes), 0xFFFF00)
	}
}

func TestBlockDrawSourceRect(t *testing.T) {
	scr := newBlockTestScreen(t)
	// 4x4 source: the right half is blue, the placement shows only it.
	surface := vtui.NewImageSurface(4, 4)
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			if x >= 2 {
				surface.SetPixel(x, y, 0, 0, 255, 255)
			} else {
				surface.SetPixel(x, y, 255, 0, 0, 255)
			}
		}
	}
	r := &blockRender{}
	r.draw(scr, vtui.ImagePlacement{Surface: surface, Cols: 2, Rows: 2, SrcX: 2, SrcW: 2, SrcH: 4}, blockImageBack)
	if c := scr.GetCell(1, 1); vtui.GetRGBFore(c.Attributes) != 0x0000FF {
		t.Errorf("source rect ignored: fg %06X, want %06X", vtui.GetRGBFore(c.Attributes), 0x0000FF)
	}
}

func TestBlockDrawRedrawsAfterMove(t *testing.T) {
	scr := newBlockTestScreen(t)
	surface := solidSurface(4, 2, 0x804020)
	p, ok := fitPlacement(surface, 1, 2, 0, 0, 4, 1)
	if !ok {
		t.Fatal("layout failed")
	}
	r := &blockRender{}
	r.draw(scr, p, blockImageBack)
	if c := scr.GetCell(3, 0); c.Char != ' ' || vtui.GetRGBFore(c.Attributes) != 0x804020 {
		t.Fatalf("first draw: char %d, fg %06X", c.Char, vtui.GetRGBFore(c.Attributes))
	}
	p.Col, p.Row = 10, 5
	r.draw(scr, p, blockImageBack)
	if c := scr.GetCell(13, 5); c.Char != ' ' || vtui.GetRGBFore(c.Attributes) != 0x804020 {
		t.Fatalf("moved draw: char %d, fg %06X", c.Char, vtui.GetRGBFore(c.Attributes))
	}
}

func TestBlockDrawLetterboxesWideImage(t *testing.T) {
	scr := newBlockTestScreen(t)
	// A 4:1 picture in a 4x4 cell box (0.5:1): it fills the full width and
	// one cell row, the rows above and below are background.
	surface := solidSurface(100, 25, 0x804020)
	r := &blockRender{}
	r.draw(scr, vtui.ImagePlacement{Surface: surface, Cols: 4, Rows: 4, SrcW: 100, SrcH: 25}, blockImageBack)
	if c := scr.GetCell(0, 0); vtui.GetRGBFore(c.Attributes) != 0x101010 {
		t.Errorf("padding row above: fg %06X, want 101010", vtui.GetRGBFore(c.Attributes))
	}
	if c := scr.GetCell(3, 3); vtui.GetRGBFore(c.Attributes) != 0x101010 {
		t.Errorf("padding row below: fg %06X, want 101010", vtui.GetRGBFore(c.Attributes))
	}
	if c := scr.GetCell(1, 1); c.Char != ' ' || vtui.GetRGBFore(c.Attributes) != 0x804020 {
		t.Errorf("image row: char %d, fg %06X", c.Char, vtui.GetRGBFore(c.Attributes))
	}
}

func TestBlockDrawLetterboxesTallImage(t *testing.T) {
	scr := newBlockTestScreen(t)
	// A 1:4 picture in an 8x2 cell box (2:1): it fills the full height and
	// two cell columns, the columns on either side are background.
	surface := solidSurface(25, 100, 0x804020)
	r := &blockRender{}
	r.draw(scr, vtui.ImagePlacement{Surface: surface, Cols: 8, Rows: 2, SrcW: 25, SrcH: 100}, blockImageBack)
	if c := scr.GetCell(0, 0); vtui.GetRGBFore(c.Attributes) != 0x101010 {
		t.Errorf("padding column left: fg %06X, want 101010", vtui.GetRGBFore(c.Attributes))
	}
	if c := scr.GetCell(5, 0); vtui.GetRGBFore(c.Attributes) != 0x101010 {
		t.Errorf("padding column right: fg %06X, want 101010", vtui.GetRGBFore(c.Attributes))
	}
	if c := scr.GetCell(3, 0); c.Char != ' ' || vtui.GetRGBFore(c.Attributes) != 0x804020 {
		t.Errorf("image cell: char %d, fg %06X", c.Char, vtui.GetRGBFore(c.Attributes))
	}
}

func TestBlockContainNeverCrops(t *testing.T) {
	scr := newBlockTestScreen(t)
	// Two equal halves: red left, green right. In a 2x1 cell box the old
	// cover renderer cropped the source square and lost the green half;
	// contain must show both pixels, one per cell.
	surface := vtui.NewImageSurface(2, 1)
	surface.SetPixel(0, 0, 255, 0, 0, 255)
	surface.SetPixel(1, 0, 0, 255, 0, 255)
	r := &blockRender{}
	r.draw(scr, vtui.ImagePlacement{Surface: surface, Cols: 2, Rows: 1, SrcW: 2, SrcH: 1}, blockImageBack)
	if c := scr.GetCell(0, 0); vtui.GetRGBFore(c.Attributes) != 0xFF0000 {
		t.Errorf("left half: fg %06X, want %06X", vtui.GetRGBFore(c.Attributes), 0xFF0000)
	}
	if c := scr.GetCell(1, 0); vtui.GetRGBFore(c.Attributes) != 0x00FF00 {
		t.Errorf("right half: fg %06X, want %06X", vtui.GetRGBFore(c.Attributes), 0x00FF00)
	}
}

func TestBlockContainEvenHeight(t *testing.T) {
	scr := newBlockTestScreen(t)
	// Aspect that would map to 3 source rows per half-row pair: dstH must
	// be snapped down to an even 2, so the extra row becomes background.
	surface := solidSurface(40, 30, 0xA0B0C0)
	r := &blockRender{}
	r.draw(scr, vtui.ImagePlacement{Surface: surface, Cols: 4, Rows: 3, SrcW: 40, SrcH: 30}, blockImageBack)
	if c := scr.GetCell(0, 0); vtui.GetRGBFore(c.Attributes) != 0x101010 {
		t.Errorf("odd remainder row above: fg %06X, want 101010", vtui.GetRGBFore(c.Attributes))
	}
	if c := scr.GetCell(1, 1); c.Char != ' ' || vtui.GetRGBFore(c.Attributes) != 0xA0B0C0 {
		t.Errorf("image cell: char %d, fg %06X", c.Char, vtui.GetRGBFore(c.Attributes))
	}
	if c := scr.GetCell(0, 2); vtui.GetRGBFore(c.Attributes) != 0x101010 {
		t.Errorf("odd remainder row below: fg %06X, want 101010", vtui.GetRGBFore(c.Attributes))
	}
}

func TestBlockGammaHalfAlphaBlend(t *testing.T) {
	scr := newBlockTestScreen(t)
	// A half-transparent grey over black is blended in linear RGB, which
	// is visibly lighter than the naive sRGB average (64).
	surface := vtui.NewImageSurface(1, 1)
	surface.SetPixel(0, 0, 128, 128, 128, 128)
	r := &blockRender{}
	r.draw(scr, vtui.ImagePlacement{Surface: surface, Cols: 1, Rows: 1, SrcW: 1, SrcH: 1}, 0)
	lin := (blockSrgbToLinear[128]*128 + blockSrgbToLinear[0]*127) / 255
	want := uint32(blockLinearToSrgb[lin])<<16 | uint32(blockLinearToSrgb[lin])<<8 | uint32(blockLinearToSrgb[lin])
	if c := scr.GetCell(0, 0); vtui.GetRGBFore(c.Attributes) != want {
		t.Errorf("gamma blend: fg %06X, want %06X (sRGB average would be %06X)",
			vtui.GetRGBFore(c.Attributes), want, uint32(64<<16|64<<8|64))
	}
}

func TestImageBlockMode(t *testing.T) {
	scr := newBlockTestScreen(t)
	old := AppConfig.ImageBlockRenderer
	defer func() { AppConfig.ImageBlockRenderer = old }()
	AppConfig.ImageBlockRenderer = 1
	if !imageBlockMode(scr) {
		t.Error("fallback mode must kick in without a graphics protocol")
	}
	scr.Graphics().SetProtocol(vtui.GraphicsKitty)
	if imageBlockMode(scr) {
		t.Error("kitty must not be displaced by the fallback mode")
	}
	AppConfig.ImageBlockRenderer = 2
	if !imageBlockMode(scr) {
		t.Error("always mode must win over kitty")
	}
	AppConfig.ImageBlockRenderer = 0
	if imageBlockMode(scr) {
		t.Error("off mode must never use the block renderer")
	}
}

// sendKey feeds one key press into the viewer and reports whether
// ProcessKey consumed it.
func sendKey(iv *ImageView, code uint16, mods ...vtinput.ControlKeyState) bool {
	e := &vtinput.InputEvent{
		Type:            vtinput.KeyEventType,
		KeyDown:         true,
		VirtualKeyCode:  code,
		ControlKeyState: 0,
	}
	for _, m := range mods {
		e.ControlKeyState |= m
	}
	return iv.ProcessKey(e)
}

// withViewerScreen runs fn with the given screen as the FrameManager's
// current screen, restoring whatever was there before.
func withViewerScreen(scr *vtui.ScreenBuf, fn func()) {
	oldFM := *vtui.FrameManager
	defer func() { *vtui.FrameManager = oldFM }()
	vtui.FrameManager.Init(scr)
	fn()
}

func TestShiftF4CyclesRenderers(t *testing.T) {
	old := AppConfig.ImageBlockRenderer
	defer func() { AppConfig.ImageBlockRenderer = old }()
	iv := &ImageView{BaseFrame: vtui.BaseFrame{}}
	scr := newBlockTestScreen(t)
	withViewerScreen(scr, func() {
		// Without a graphics protocol the half-block cells are the only
		// stop of the round robin: the setting has to stay put.
		AppConfig.ImageBlockRenderer = 1
		for i := 0; i < 2; i++ {
			if !sendKey(iv, vtinput.VK_F4, vtinput.ShiftPressed) {
				t.Fatal("Shift+F4 must be consumed")
			}
		}
		if AppConfig.ImageBlockRenderer != 1 {
			t.Fatalf("no-graphics cycle moved the setting to %d, want 1",
				AppConfig.ImageBlockRenderer)
		}
		// With kitty available the two renderers alternate.
		scr.Graphics().SetProtocol(vtui.GraphicsKitty)
		sendKey(iv, vtinput.VK_F4, vtinput.ShiftPressed)
		if AppConfig.ImageBlockRenderer != 2 {
			t.Fatalf("graphics -> cells cycle set %d, want 2",
				AppConfig.ImageBlockRenderer)
		}
		sendKey(iv, vtinput.VK_F4, vtinput.ShiftPressed)
		if AppConfig.ImageBlockRenderer != 1 {
			t.Fatalf("cells -> graphics cycle set %d, want 1",
				AppConfig.ImageBlockRenderer)
		}
		sendKey(iv, vtinput.VK_F4, vtinput.ShiftPressed)
		if AppConfig.ImageBlockRenderer != 2 {
			t.Fatalf("second cells -> cycle set %d, want 2",
				AppConfig.ImageBlockRenderer)
		}
	})
}

func TestShiftF4IgnoredInGallery(t *testing.T) {
	old := AppConfig.ImageBlockRenderer
	defer func() { AppConfig.ImageBlockRenderer = old }()
	iv := &ImageView{BaseFrame: vtui.BaseFrame{}, gal: &imageGallery{}}
	AppConfig.ImageBlockRenderer = 1
	if !sendKey(iv, vtinput.VK_F4, vtinput.ShiftPressed) {
		t.Fatal("Shift+F4 must be consumed")
	}
	if AppConfig.ImageBlockRenderer != 1 {
		t.Fatalf("gallery Shift+F4 changed the setting to %d, want 1",
			AppConfig.ImageBlockRenderer)
	}
}

// TestBlockRenderGrowsBuffers draws twice, the second time into a larger grid:
// the internal filter buffers must grow without panics or stale data.
func TestBlockRenderGrowsBuffers(t *testing.T) {
	scr := newBlockTestScreen(t)
	surface := solidSurface(4, 2, 0x804020)
	r := &blockRender{}
	r.draw(scr, vtui.ImagePlacement{Surface: surface, Cols: 4, Rows: 1, SrcW: 4, SrcH: 2}, blockImageBack)
	r.draw(scr, vtui.ImagePlacement{Surface: surface, Cols: 8, Rows: 4, SrcW: 4, SrcH: 2}, blockImageBack)
	// The 2:1 source fits the 8x4 box by width and is centred vertically,
	// so the image occupies cell rows 1-2; (7,3) is padding.
	if c := scr.GetCell(7, 2); c.Char != ' ' || vtui.GetRGBFore(c.Attributes) != 0x804020 {
		t.Fatalf("cell after buffer growth: char %d, fg %06X", c.Char, vtui.GetRGBFore(c.Attributes))
	}
	if c := scr.GetCell(7, 3); vtui.GetRGBFore(c.Attributes) != 0x101010 {
		t.Fatalf("padding below image: fg %06X, want 101010", vtui.GetRGBFore(c.Attributes))
	}
}

// newBenchSurface paints a 4K RGBA gradient so the filter cannot be
// optimised away.
func newBenchSurface(w, h int) *vtui.ImageSurface {
	surf := vtui.NewImageSurface(w, h)
	for i := 0; i < len(surf.Pix); i += 4 {
		surf.Pix[i] = byte(i % 255)
		surf.Pix[i+1] = byte((i / 2) % 255)
		surf.Pix[i+2] = byte((i / 3) % 255)
		surf.Pix[i+3] = 255
	}
	surf.Opaque = true
	return surf
}

// BenchmarkBoxFilter_4KtoTerminal downsamples a 4K gradient to an 80x25 grid.
// The placement shifts every call so the static-geometry memo never hits:
// this measures the cold resample path.
func BenchmarkBoxFilter_4KtoTerminal(b *testing.B) {
	surf := newBenchSurface(3840, 2160)
	p := vtui.ImagePlacement{Surface: surf, Cols: 80, Rows: 25, SrcW: 3840, SrcH: 2160}
	r := &blockRender{}
	scr := newBenchScreen()
	b.ResetTimer()
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		p.Col = i & 1
		i++
		r.draw(scr, p, blockImageBack)
	}
}

// BenchmarkBlockRedrawStatic times the repeat draw of an unchanged
// placement: the memo must skip both filter passes and only re-stamp
// the cached cells. The first call warms the memo.
func BenchmarkBlockRedrawStatic(b *testing.B) {
	surf := newBenchSurface(3840, 2160)
	p := vtui.ImagePlacement{Surface: surf, Cols: 80, Rows: 25, SrcW: 3840, SrcH: 2160}
	r := &blockRender{}
	scr := newBenchScreen()
	r.draw(scr, p, blockImageBack)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		r.draw(scr, p, blockImageBack)
	}
}

// TestBlockVerticalPanMatchesFreshRender locks in the hMemo path: a
// vertical-only pan must reuse the horizontal pass without corrupting the
// vertical pass output, so the cells equal what a fresh renderer produces.
// The placement is zoomed past blockWorkMaxZoom so the working copy stands
// aside and this fallback path is what runs.
func TestBlockVerticalPanMatchesFreshRender(t *testing.T) {
	surf := newBenchSurface(96, 96)
	scr := newBlockTestScreen(t)
	r := &blockRender{}
	for sy := 0; sy < 48; sy += 7 {
		p := vtui.ImagePlacement{Surface: surf, Cols: 24, Rows: 8,
			SrcX: 0, SrcY: sy, SrcW: 20, SrcH: 10}
		r.draw(scr, p, blockImageBack)
		assertFreshDraw(t, r, scr, p)
	}
}

// assertFreshDraw compares one draw with a fresh renderer's, cell by cell.
func assertFreshDraw(t *testing.T, r *blockRender, scr *vtui.ScreenBuf, p vtui.ImagePlacement) {
	t.Helper()
	fresh := &blockRender{}
	freshScr := newBlockTestScreen(t)
	fresh.draw(freshScr, p, blockImageBack)
	for y := 0; y < p.Rows; y++ {
		for x := 0; x < p.Cols; x++ {
			if got, want := scr.GetCell(x, y), freshScr.GetCell(x, y); got != want {
				t.Fatalf("cell %d,%d: %+v != fresh %+v (SrcX=%d SrcY=%d)", x, y, got, want, p.SrcX, p.SrcY)
			}
		}
	}
}

// TestBlockWorkCopyMatchesFreshWhenAligned: when the pan lands on the working
// copy's grid (every second source pixel here, one destination column being
// two source pixels), the cut equals a fresh render cell by cell.
func TestBlockWorkCopyMatchesFreshWhenAligned(t *testing.T) {
	surf := newBenchSurface(96, 96)
	r := &blockRender{}
	scr := newBlockTestScreen(t)
	for sx := 0; sx <= 40; sx += 2 {
		p := vtui.ImagePlacement{Surface: surf, Cols: 24, Rows: 8,
			SrcX: sx, SrcY: 0, SrcW: 48, SrcH: 24}
		r.draw(scr, p, blockImageBack)
		assertFreshDraw(t, r, scr, p)
	}
}

// TestBlockWorkCopySnapsPansToGrid locks in the working copy's approximation:
// a pan that stays inside one work cell (half a destination column) leaves
// the output untouched, because the cut snaps to the copy's grid.
func TestBlockWorkCopySnapsPansToGrid(t *testing.T) {
	surf := newBenchSurface(96, 96)
	r := &blockRender{}
	scr := newBlockTestScreen(t)
	drawAt := func(sx int) []vtui.CharInfo {
		r.draw(scr, vtui.ImagePlacement{Surface: surf, Cols: 24, Rows: 8,
			SrcX: sx, SrcY: 0, SrcW: 48, SrcH: 24}, blockImageBack)
		out := make([]vtui.CharInfo, 24*8)
		for y := 0; y < 8; y++ {
			for x := 0; x < 24; x++ {
				out[y*24+x] = scr.GetCell(x, y)
			}
		}
		return out
	}
	a, b := drawAt(1), drawAt(2) // both round to the same work column
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("pan within a work cell changed cell %d: %+v != %+v", i, a[i], b[i])
		}
	}
}

// countingWriter counts payload bytes written through the ANSI renderer.
type countingWriter struct{ n int }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	return len(p), nil
}

// BenchmarkBlockFrameEmit measures the cold end-to-end path: draw a fresh
// 4K placement, Flush to ANSI, and count the emitted bytes. This is the
// "closer to instant" proof: resample + diff + emit in one number.
func BenchmarkBlockFrameEmit(b *testing.B) {
	surf := newBenchSurface(3840, 2160)
	p := vtui.ImagePlacement{Surface: surf, Cols: 80, Rows: 25, SrcW: 3840, SrcH: 2160}
	r := &blockRender{}
	scr := vtui.NewScreenBuf()
	cw := &countingWriter{}
	scr.Writer = cw
	scr.AllocBuf(80, 25)
	scr.Graphics().SetProtocol(vtui.GraphicsNone)
	i := 0
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		scr.FillRect(0, 0, 79, 24, ' ', imageViewBackAttr)
		p.Col = i & 1
		i++
		r.draw(scr, p, blockImageBack)
		scr.Flush()
	}
	b.ReportMetric(float64(cw.n)/float64(b.N), "bytes/frame")
}

// BenchmarkNearestNeighbor_4KtoTerminal is the naive baseline: one source
// sample per half-row. The separable box filter should stay close to it.
func BenchmarkNearestNeighbor_4KtoTerminal(b *testing.B) {
	surf := newBenchSurface(3840, 2160)
	buf := make([]vtui.CharInfo, 80*25)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		for cy := 0; cy < 50; cy++ {
			row := surf.Pix[(cy*2160/50)*surf.Stride:]
			for cx := 0; cx < 80; cx++ {
				sx := (cx * 3840 / 80) * 4
				c := &buf[cy/2*80+cx]
				c.Char = blockHalfChar
				c.Attributes = vtui.SetRGBBoth(0,
					uint32(row[sx])<<16|uint32(row[sx+1])<<8|uint32(row[sx+2]), 0)
			}
		}
	}
}

// FuzzBlockRenderDraw drives the full draw path with hostile sizes and pan
// windows: it must never panic, no matter how the placement and source rect
// combine.
func FuzzBlockRenderDraw(f *testing.F) {
	f.Add(8, 4, 4, 2, 0, 0, 8, 4, 10, 20)
	f.Add(3840, 2160, 80, 25, 100, 100, 3640, 1960, 10, 20)
	f.Add(1, 1, 1, 1, 0, 0, 1, 1, 1, 1)
	f.Add(2048, 2048, 80, 25, 0, 0, 2048, 2048, 8, 16)
	f.Fuzz(func(t *testing.T, w, h, cols, rows, sx, sy, sw, sh, cw, ch int) {
		if w <= 0 || h <= 0 || cols <= 0 || rows <= 0 || cw <= 0 || ch <= 0 {
			t.Skip()
		}
		if w > 2048 || h > 2048 || cols > 80 || rows > 25 || cw > 64 || ch > 64 {
			t.Skip()
		}
		// clamp the pan window to the surface
		sx, sy = sx%w, sy%h
		if sw <= 0 || sh <= 0 {
			t.Skip()
		}
		if sw > w-sx {
			sw = w - sx
		}
		if sh > h-sy {
			sh = h - sy
		}
		surf := vtui.NewImageSurface(w, h)
		for i := 0; i < len(surf.Pix); i += 4 {
			surf.Pix[i] = byte(i * 7)
			surf.Pix[i+1] = byte(i * 13)
			surf.Pix[i+2] = byte(i * 29)
			surf.Pix[i+3] = 255
		}
		p := vtui.ImagePlacement{Surface: surf, Cols: cols, Rows: rows, SrcX: sx, SrcY: sy, SrcW: sw, SrcH: sh}
		r := &blockRender{}
		scr := newBlockTestScreen(t)
		r.draw(scr, p, blockImageBack)
	})
}
