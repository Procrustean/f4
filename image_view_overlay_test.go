package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/unxed/vtui"
)

// newSixelTestScreen is newImageTestScreen with the sixel protocol, so the
// tests below exercise the no-z-index path where the image is emitted after
// the text.
func newSixelTestScreen(t *testing.T) *vtui.ScreenBuf {
	return newImageTestScreenMode(t, vtui.WorkspaceTabsNever, vtui.GraphicsSixel)
}

// setRendererMode pins the block renderer setting and restores it, so a test
// does not depend on the mode an earlier test left behind.
func setRendererMode(t *testing.T, mode int) {
	t.Helper()
	old := AppConfig.ImageBlockRenderer
	t.Cleanup(func() { AppConfig.ImageBlockRenderer = old })
	AppConfig.ImageBlockRenderer = mode
}

// placementTouches reports whether a placement covers any cell of r.
func placementTouches(p vtui.ImagePlacement, r vtui.Rect) bool {
	return p.Col <= r.X2 && p.Col+p.Cols-1 >= r.X1 &&
		p.Row <= r.Y2 && p.Row+p.Rows-1 >= r.Y1
}

func TestOverlayTextRectsAreTheGlyphCells(t *testing.T) {
	iv := newTestImageView(t, 4000, 2000)
	iv.overlay = true
	iv.path = "photo.png"
	iv.decoder = "png"

	pane, ok := iv.overlayPane()
	if !ok {
		t.Fatal("overlay pane missing")
	}
	rects := iv.overlayTextRects(pane)
	if len(rects) == 0 {
		t.Fatal("overlay must produce glyph rects")
	}
	for i, rc := range rects {
		if rc.X1 != pane.X1+1 {
			t.Errorf("rect %d must skip the leading space: %+v", i, rc)
		}
		if rc.Y1 != pane.Y1+i || rc.Y2 != pane.Y1+i {
			t.Errorf("rect %d must sit on its own line: %+v", i, rc)
		}
		if rc.X2 < rc.X1 {
			t.Errorf("rect %d is empty: %+v", i, rc)
		}
	}
}

func TestToastTextRectTracksTheSlide(t *testing.T) {
	iv := newTestImageView(t, 4000, 2000)
	iv.toast("scale: 120%")

	strip, ok := iv.toastGuardRect()
	if !ok {
		t.Fatal("toast guard missing")
	}
	rc, ok := iv.toastTextRect(strip)
	if !ok || rc.X1 != strip.X1 || rc.X2 != strip.X2 {
		t.Errorf("at rest the toast rect must equal the guard: %+v (guard %+v)", rc, strip)
	}

	// Mid-slide the toast has moved left, still inside the frame.
	iv.tempSlideStart = time.Now().Add(-50 * time.Millisecond)
	if !iv.tempSliding() {
		t.Fatal("the toast must be sliding")
	}
	rc, ok = iv.toastTextRect(strip)
	if !ok {
		t.Fatal("a sliding toast must still have a rect")
	}
	if rc.X1 < strip.X1 || rc.X2 > strip.X2 {
		t.Errorf("sliding toast must stay inside the frame: %+v (guard %+v)", rc, strip)
	}
	if rc.X2 == strip.X2 && rc.X1 == strip.X1 {
		t.Errorf("the sliding rect must actually move: %+v", rc)
	}
}

func TestSixelKeepsOverlayAndToastOnPicture(t *testing.T) {
	setRendererMode(t, 1) // block only when there is no graphics; sixel here
	scr := newSixelTestScreen(t)

	// A 2:1 picture zoomed to crop: the placement fills the whole frame, so
	// the OSD pane and the toast sit on the image. Sixel paints pixels over
	// text written first, so the glyphs are re-asserted after the image
	// instead of the picture being carved around them.
	iv := newTestImageView(t, 4000, 2000)
	iv.surface = solidSurface(4000, 2000, 0xCC8855)
	iv.zoom = 2
	iv.overlay = true
	iv.path = "photo.png"
	iv.decoder = "png"
	iv.toast("scale: 120%")

	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()

	pane, okPane := iv.overlayPane()
	strip, okStrip := iv.toastGuardRect()
	if !okPane || !okStrip {
		t.Fatal("the overlay and the toast must produce guard rectangles")
	}
	if !iv.textOnImage {
		t.Error("the overlay and toast sit on the picture, textOnImage must be set")
	}

	// The buffer keeps the info: the toast and the overlay are drawn, and on
	// the picture they use the bare no-slab attribute so the image shows
	// around the glyphs.
	if row := ScreenRow(scr, strip.Y1, strip.X1, strip.X2); !strings.Contains(row, "scale: 120%") {
		t.Errorf("toast missing from the buffer: %q", row)
	}
	if attr := scr.GetCell(strip.X1, strip.Y1).Attributes; attr != imageBareTextAttr {
		t.Errorf("toast on the picture must use the bare attribute, got %#x want %#x", attr, imageBareTextAttr)
	}
	if row := ScreenRow(scr, pane.Y1, pane.X1, pane.X2); !strings.Contains(row, "photo.png") {
		t.Errorf("overlay missing from the buffer: %q", row)
	}
	if attr := scr.GetCell(pane.X1+1, pane.Y1).Attributes; attr != imageBareTextAttr {
		t.Errorf("overlay on the picture must use the bare attribute, got %#x want %#x", attr, imageBareTextAttr)
	}

	// The whole picture stays one placement; the slabs are re-asserted as
	// text after the DCS rather than carved out of the picture.
	snap := make([]vtui.ImagePlacement, 0, 4)
	snap, _ = scr.Graphics().Snapshot(snap)
	if len(snap) != 1 {
		t.Fatalf("sixel must keep a single placement, got %d", len(snap))
	}
	if !placementOverlaps(snap[0], vtui.Rect{X1: 0, Y1: 0, X2: 79, Y2: 23}) {
		t.Errorf("placement must fill the frame, got %+v", snap[0])
	}
}

func TestSixelStreamKeepsToastOverImage(t *testing.T) {
	setRendererMode(t, 1)
	scr := newSixelTestScreen(t)

	iv := newTestImageView(t, 4000, 2000)
	iv.surface = solidSurface(4000, 2000, 0xCC8855)
	iv.zoom = 2
	iv.overlay = true
	iv.path = "photo.png"
	iv.toast("scale: 120%")

	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()

	var out bytes.Buffer
	scr.SetOutput(&out)
	scr.Flush()
	raw := out.String()
	if !strings.Contains(raw, "scale: 120%") {
		t.Errorf("toast text missing from the emitted stream")
	}
	if dcs := strings.Count(raw, "\x1bP0;1;8q"); dcs < 1 {
		t.Errorf("the frame must emit a sixel image, got %d DCS", dcs)
	}
	// The DCS pixels cover text written before them, so the slabs must be
	// re-emitted as text after the last image: the terminal then paints the
	// overlay and the toast on top of the picture in every renderer.
	if lastDCS := strings.LastIndex(raw, "\x1bP0;1;8q"); lastDCS < 0 {
		t.Errorf("no sixel DCS in the stream")
	} else {
		if i := strings.LastIndex(raw, "scale: 120%"); i < lastDCS {
			t.Errorf("toast must be re-emitted after the images (DCS at %d, toast at %d)", lastDCS, i)
		}
		if i := strings.LastIndex(raw, "photo.png"); i < lastDCS {
			t.Errorf("overlay must be re-emitted after the images (DCS at %d, overlay at %d)", lastDCS, i)
		}
	}
}

func TestBlockToastAndOverlayEmitted(t *testing.T) {
	setRendererMode(t, 1)
	scr := newBlockTestScreen(t)
	iv := newTestImageView(t, 100, 100)
	iv.block = &blockRender{}
	iv.overlay = true
	iv.path = "photo.png"
	iv.decoder = "png"
	iv.toast("scale: 120%")

	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()

	if row := ScreenRow(scr, 23, 0, 30); !strings.Contains(row, "scale: 120%") {
		t.Errorf("toast missing in block mode: %q", row)
	}
	if row := ScreenRow(scr, 2, 0, 20); !strings.Contains(row, "photo.png") {
		t.Errorf("overlay missing in block mode: %q", row)
	}
}

func TestPlainToastAndOverlayEmitted(t *testing.T) {
	setRendererMode(t, 3) // forced plain spaces
	scr := newBlockTestScreen(t)
	iv := newTestImageView(t, 100, 100)
	iv.block = &blockRender{}
	iv.overlay = true
	iv.path = "photo.png"
	iv.toast("scale: 120%")

	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()

	if row := ScreenRow(scr, 23, 0, 30); !strings.Contains(row, "scale: 120%") {
		t.Errorf("toast missing in plain mode: %q", row)
	}
	if row := ScreenRow(scr, 2, 0, 20); !strings.Contains(row, "photo.png") {
		t.Errorf("overlay missing in plain mode: %q", row)
	}
}

func TestKittyKeepsOnePlacement(t *testing.T) {
	setRendererMode(t, 1)
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 4000, 2000)
	iv.zoom = 2
	iv.overlay = true
	iv.path = "photo.png"
	iv.toast("scale: 120%")

	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()

	// Kitty layers the image below the glyphs, so the slabs need no carving.
	if n := scr.Graphics().Len(); n != 1 {
		t.Errorf("kitty must keep a single placement, got %d", n)
	}
}
