package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/unxed/f4/imagedec"
	"github.com/unxed/f4/vfs"
	"github.com/unxed/vtinput"
	"github.com/unxed/vtui"
)

// newImageTestScreenMode builds a silent screen with the given protocol and
// pins the workspace tab mode, so layout tests see a stable top row.
func newImageTestScreenMode(t *testing.T, tab vtui.WorkspaceTabMode, proto vtui.GraphicsProtocol) *vtui.ScreenBuf {
	t.Helper()
	wasMode := vtui.FrameManager.WorkspaceTabMode
	vtui.FrameManager.WorkspaceTabMode = tab
	t.Cleanup(func() { vtui.FrameManager.WorkspaceTabMode = wasMode })

	scr := vtui.NewScreenBuf()
	scr.Writer = io.Discard
	scr.AllocBuf(80, 25)
	scr.Graphics().SetProtocol(proto)
	scr.Graphics().SetCellSize(8, 16)
	return scr
}

func newImageTestScreen(t *testing.T) *vtui.ScreenBuf {
	return newImageTestScreenMode(t, vtui.WorkspaceTabsNever, vtui.GraphicsKitty)
}

func newTestImageView(t *testing.T, w, h int) *ImageView {
	t.Helper()
	iv := &ImageView{
		path:    "test.png",
		surface: vtui.NewImageSurface(w, h),
		decoder: "test",
		zoom:    1,
		gfxKey:  "test-key",
	}
	iv.ResizeConsole(80, 25)
	return iv
}

// restoreBars keeps the whole screen flag from leaking between tests.
func restoreBars(t *testing.T) {
	t.Helper()
	was := vtui.FrameManager.HideBars
	t.Cleanup(func() { vtui.FrameManager.HideBars = was })
}

func TestImageViewFitsAndCentres(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)

	p, ok := iv.placementFor(scr)
	if !ok {
		t.Fatal("layout failed")
	}
	// The window has 24 available rows (h=25, minus the key bar).
	// Available cells: 80x24, cell size 8x16 -> 640x384 pixels.
	// A square image fits to 384x384, which is 48x24 cells, centred horizontally.
	if p.Cols != 48 || p.Rows != 24 {
		t.Errorf("wrong size %dx%d cells", p.Cols, p.Rows)
	}
	if p.Col != 16 || p.Row != 0 {
		t.Errorf("wrong origin %d,%d", p.Col, p.Row)
	}
	if p.SrcW != 0 || p.SrcH != 0 {
		t.Error("a fitting image must not be cropped")
	}
}

func TestImageViewZoomCropsAndPans(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)
	iv.SetZoom(4)

	p, ok := iv.placementFor(scr)
	if !ok {
		t.Fatal("layout failed")
	}
	if p.SrcW <= 0 || p.SrcW >= 100 || p.SrcH <= 0 || p.SrcH >= 100 {
		t.Fatalf("a zoomed image must show only a part of the source, got %dx%d", p.SrcW, p.SrcH)
	}
	if p.SrcX != 0 || p.SrcY != 0 {
		t.Errorf("panning should start at the origin, got %d,%d", p.SrcX, p.SrcY)
	}

	for i := 0; i < 50; i++ {
		iv.Pan(1, 1)
	}
	p, _ = iv.placementFor(scr)
	if p.SrcX+p.SrcW > 100 || p.SrcY+p.SrcH > 100 {
		t.Errorf("panning ran off the image: %d+%d, %d+%d", p.SrcX, p.SrcW, p.SrcY, p.SrcH)
	}
	if p.SrcX == 0 {
		t.Error("panning had no effect")
	}

	for i := 0; i < 50; i++ {
		iv.Pan(-1, -1)
	}
	p, _ = iv.placementFor(scr)
	if p.SrcX != 0 || p.SrcY != 0 {
		t.Errorf("panning back must reach the origin, got %d,%d", p.SrcX, p.SrcY)
	}
}

func TestImageViewZoomOutClearsThePan(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)
	iv.SetZoom(4)
	iv.Pan(1, 1)
	iv.placementFor(scr)

	iv.SetZoom(1)
	p, _ := iv.placementFor(scr)
	if p.SrcX != 0 || p.SrcY != 0 || p.SrcW != 0 {
		t.Errorf("zooming back to fit must drop the crop, got %+v", p)
	}
}

func TestImageViewZoomLimits(t *testing.T) {
	iv := newTestImageView(t, 10, 10)
	for i := 0; i < 200; i++ {
		iv.SetZoom(iv.zoom * 2)
	}
	if iv.zoom > imageViewMaxZoom {
		t.Errorf("zoom escaped its upper limit: %v", iv.zoom)
	}
	for i := 0; i < 200; i++ {
		iv.SetZoom(iv.zoom / 2)
	}
	if iv.zoom < imageViewMinZoom {
		t.Errorf("zoom escaped its lower limit: %v", iv.zoom)
	}
}

func TestImageViewKeys(t *testing.T) {
	iv := newTestImageView(t, 100, 100)

	press := func(char rune, vk uint16) bool {
		return iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: char, VirtualKeyCode: vk})
	}

	if !press('+', 0) || iv.zoom <= 1 {
		t.Error("plus must zoom in")
	}
	if !press('-', 0) {
		t.Error("minus must be handled")
	}
	if !press('*', 0) || iv.zoom != 1 {
		t.Errorf("star must reset the zoom, got %v", iv.zoom)
	}
	if !press('d', 0) || iv.panX <= 0 {
		t.Error("d must pan")
	}
	if press('z', 0) {
		t.Error("unrelated keys must be left to the rest of the UI")
	}
	if !press('~', 0) {
		t.Error("tilde must be handled")
	}
	if !press('ё', 0) {
		t.Error("the grave key on a Russian layout must be handled")
	}
	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_OEM_3}) {
		t.Error("the physical key left of 1 must be handled")
	}
	if !press(0, vtinput.VK_ESCAPE) || !iv.IsDone() {
		t.Error("escape must close the viewer")
	}
}

func TestImageViewShowDeclaresThePlacement(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)

	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()

	if scr.Graphics().Len() != 1 {
		t.Fatalf("expected one placement, got %d", scr.Graphics().Len())
	}

	// A frame that is not painted must not leave its picture behind.
	scr.Graphics().BeginFrame()
	scr.Graphics().EndFrame()
	if scr.Graphics().Len() != 0 {
		t.Error("the placement outlived the frame that owned it")
	}
}

// withStubPipeline replaces the application pipeline with one that decodes
// pictures out of thin air, and reports which files it was asked for.
func withStubPipeline(t *testing.T, w, h int) chan string {
	t.Helper()
	asked := make(chan string, 16)

	old := ImagePipe
	t.Cleanup(func() { ImagePipe = old })

	ImagePipe = newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		asked <- path
		return imageTestSurface(w, h), "stub", nil
	})
	ImagePipe.preview = func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		return nil, "", errors.New("no thumbnail")
	}
	return asked
}

func TestImageViewWalksItsSiblings(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "b.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 1)

	// Decode them all first, so that stepping is answered from the cache.
	for _, name := range []string{"a.png", "c.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	iv.Step(1)
	if iv.index != 2 || iv.path != "c.png" {
		t.Fatalf("a step forward went to %d, %q", iv.index, iv.path)
	}
	if iv.surface.Width != 20 || iv.surface.Height != 10 {
		t.Errorf("the new picture is not the one on screen: %dx%d", iv.surface.Width, iv.surface.Height)
	}

	// Walking past the end stays on the last picture.
	iv.Step(5)
	if iv.index != 2 {
		t.Errorf("walking past the end left the list at %d", iv.index)
	}

	iv.GoTo(0)
	if iv.index != 0 || iv.path != "a.png" {
		t.Errorf("Home went to %d, %q", iv.index, iv.path)
	}

	// A viewer without siblings has nowhere to step.
	lone := newTestImageView(t, 10, 10)
	lone.Step(1)
	if lone.path != "test.png" {
		t.Errorf("a lone picture must stay put, got %q", lone.path)
	}
}

func TestImageViewRegularArrowsAlwaysWalk(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "b.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 1)
	for _, name := range []string{"a.png", "c.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	// The enhanced arrows are the regular cluster ones: they walk the
	// directory even when the picture is zoomed and could be panned.
	iv.SetZoom(4)
	press := func(vk uint16) bool {
		return iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vk,
			ControlKeyState: vtinput.EnhancedKey})
	}
	if !press(vtinput.VK_RIGHT) || iv.index != 2 {
		t.Fatalf("the right arrow should have stepped forward, index is %d", iv.index)
	}
	if !press(vtinput.VK_LEFT) || iv.index != 1 {
		t.Fatalf("the left arrow should have stepped back, index is %d", iv.index)
	}
	if !press(vtinput.VK_DOWN) || iv.index != 2 {
		t.Fatalf("the down arrow should have stepped forward, index is %d", iv.index)
	}
	if iv.panX != 0 || iv.panY != 0 {
		t.Errorf("walking must not pan: %v %v", iv.panX, iv.panY)
	}
}

func TestImageViewNumpadArrowsPan(t *testing.T) {
	withStubPipeline(t, 400, 400)
	scr := newImageTestScreen(t)

	iv := newTestImageView(t, 400, 400)
	iv.SetSiblings([]string{"test.png", "two.png"}, 0)
	if res := ImagePipe.LoadSync(context.Background(), nil, "two.png"); res.Err != nil {
		t.Fatalf("two.png: %v", res.Err)
	}

	iv.SetZoom(8)
	if _, ok := iv.placementFor(scr); !ok {
		t.Fatal("layout failed")
	}
	if iv.panMaxX <= 0 {
		t.Fatalf("a zoomed picture must have room to pan, got %v", iv.panMaxX)
	}

	// The plain arrows are the numpad ones (the console leaves them
	// unenhanced): they pan, they never walk.
	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_RIGHT}) {
		t.Fatal("a plain right arrow must be handled")
	}
	if iv.panX <= 0 {
		t.Error("a numpad arrow must pan, not walk past")
	}
	if iv.path != "test.png" || iv.index != 0 {
		t.Errorf("the list must not have moved: %q %d", iv.path, iv.index)
	}

	// The explicit numpad codes (NumLock on) pan the same way.
	iv.panX, iv.panY = 0, 0
	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_NUMPAD6}) {
		t.Fatal("numpad 6 must be handled")
	}
	if iv.panX <= 0 {
		t.Error("numpad 6 must pan right")
	}
	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_NUMPAD2}) {
		t.Fatal("numpad 2 must be handled")
	}
	if iv.panY <= 0 {
		t.Error("numpad 2 must pan down")
	}
	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_NUMPAD4}) {
		t.Fatal("numpad 4 must be handled")
	}
	if iv.panX != 0 {
		t.Error("numpad 4 must pan back to the left edge")
	}
}

func TestImageViewJKWalk(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "b.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 1)
	for _, name := range []string{"a.png", "c.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	press := func(e *vtinput.InputEvent) bool {
		return iv.ProcessKey(e)
	}

	// j and k on the English layout.
	if !press(&vtinput.InputEvent{KeyDown: true, Char: 'j'}) || iv.index != 2 {
		t.Fatalf("j must step forward, index is %d", iv.index)
	}
	if !press(&vtinput.InputEvent{KeyDown: true, Char: 'K'}) || iv.index != 1 {
		t.Fatalf("k must step back, index is %d", iv.index)
	}
	// The same physical keys on the Russian layout: о and л.
	if !press(&vtinput.InputEvent{KeyDown: true, Char: 'о'}) || iv.index != 2 {
		t.Fatalf("the Russian о (physical j) must step forward, index is %d", iv.index)
	}
	if !press(&vtinput.InputEvent{KeyDown: true, Char: 'Л'}) || iv.index != 1 {
		t.Fatalf("the Russian Л (physical k) must step back, index is %d", iv.index)
	}
	// The physical codes, as the console and gogpu carry them.
	if !press(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_J}) || iv.index != 2 {
		t.Fatalf("VK_J must step forward, index is %d", iv.index)
	}
	if !press(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_K}) || iv.index != 1 {
		t.Fatalf("VK_K must step back, index is %d", iv.index)
	}
	if iv.panX != 0 || iv.panY != 0 {
		t.Errorf("j/k must walk, not pan: %v %v", iv.panX, iv.panY)
	}
}

func TestImageViewToggleCompare(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "b.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 1)
	for _, name := range []string{"a.png", "c.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	press := func() bool {
		return iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: '~'})
	}

	// The first press anchors the comparison and steps to the next picture.
	if !press() || iv.path != "c.png" {
		t.Fatalf("~ must step to the next picture, got %q", iv.path)
	}
	// Every later press flips between the two sides: three presses from the
	// next picture land back on the anchor.
	for i := 0; i < 3; i++ {
		press()
	}
	if iv.path != "b.png" {
		t.Fatalf("~ must toggle back to the anchor, got %q", iv.path)
	}
	if !press() || iv.path != "c.png" {
		t.Errorf("~ must flip to the next side again, got %q", iv.path)
	}

	// Moving the list elsewhere restarts the comparison there.
	iv.GoTo(0)
	if !press() || iv.path != "b.png" {
		t.Errorf("after navigating away, ~ must compare from the new picture, got %q", iv.path)
	}
}

func TestImageViewToggleCompareAtTheEndOfTheList(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "c.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 2)
	for _, name := range []string{"a.png", "b.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	press := func() bool {
		return iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: '~'})
	}

	// The last picture has no next: the comparison partner is the previous
	// one, and the toggle still flips back and forth.
	if !press() || iv.path != "b.png" {
		t.Fatalf("~ must step back from the end, got %q", iv.path)
	}
	if !press() || iv.path != "c.png" {
		t.Errorf("~ must flip back to the end, got %q", iv.path)
	}
}

func TestImageViewNavigationCarriesZoomAndPan(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "b.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 1)
	for _, name := range []string{"a.png", "c.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	iv.SetZoom(4)
	iv.Pan(1, 1)
	iv.Step(1)

	if iv.path != "c.png" || iv.index != 2 {
		t.Fatalf("a step forward went to %d, %q", iv.index, iv.path)
	}
	if iv.zoom != 4 {
		t.Errorf("the zoom must follow the reader, got %v", iv.zoom)
	}
	if iv.panX == 0 || iv.panY == 0 {
		t.Errorf("the pan must follow the reader, got %v %v", iv.panX, iv.panY)
	}

	iv.Step(-1)
	if iv.path != "b.png" || iv.zoom != 4 {
		t.Errorf("stepping back must keep the zoom too, got %q zoom %v", iv.path, iv.zoom)
	}
}

func TestImageViewCarryFractionAcrossSizes(t *testing.T) {
	// The pan follows the reader as a share of its range: the bottom of one
	// picture lands on the bottom of the next, whatever its size or shape.
	sizes := map[string][2]int{
		"a.png": {400, 400},
		"b.png": {200, 400}, // portrait: half the width
		"c.png": {400, 800}, // taller
	}
	old := ImagePipe
	t.Cleanup(func() { ImagePipe = old })
	ImagePipe = newTestPipeline(func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		sz, ok := sizes[path]
		if !ok {
			sz = [2]int{400, 400}
		}
		return imageTestSurface(sz[0], sz[1]), "stub", nil
	})
	ImagePipe.preview = func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		return nil, "", errors.New("no thumbnail")
	}

	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 400, 400)
	iv.path = "a.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 0)
	for name := range sizes {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	iv.SetZoom(4)
	if _, ok := iv.placementFor(scr); !ok {
		t.Fatal("layout failed for a")
	}
	// To the very bottom of the first picture.
	iv.panX, iv.panY = 0, iv.panMaxY
	if _, ok := iv.placementFor(scr); !ok {
		t.Fatal("layout failed for a, panned")
	}

	iv.Step(1) // to the portrait b
	if _, ok := iv.placementFor(scr); !ok {
		t.Fatal("layout failed for b")
	}
	if iv.panY != iv.panMaxY {
		t.Errorf("the bottom of a must land on the bottom of b: pan %v of %v", iv.panY, iv.panMaxY)
	}

	iv.Step(1) // to the taller c
	if _, ok := iv.placementFor(scr); !ok {
		t.Fatal("layout failed for c")
	}
	if iv.panY != iv.panMaxY {
		t.Errorf("the bottom must stay the bottom for a taller picture: pan %v of %v", iv.panY, iv.panMaxY)
	}
}

func TestImageViewCompareCarriesTheZoom(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "b.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 1)
	for _, name := range []string{"a.png", "c.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	iv.SetZoom(4)
	iv.Pan(1, 1)

	press := func() bool {
		return iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: '~'})
	}
	if !press() || iv.path != "c.png" {
		t.Fatalf("~ must step to the next picture, got %q", iv.path)
	}
	if iv.zoom != 4 || iv.panX == 0 || iv.panY == 0 {
		t.Errorf("the next side must show at the same zoom and pan, got zoom %v pan %v %v", iv.zoom, iv.panX, iv.panY)
	}

	if !press() || iv.path != "b.png" {
		t.Fatalf("~ must flip back to the anchor, got %q", iv.path)
	}
	if iv.zoom != 4 {
		t.Errorf("the anchor side must keep the zoom too, got %v", iv.zoom)
	}
}

func TestImageViewActualSizeRidesAlong(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "b.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 1)
	for _, name := range []string{"a.png", "c.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	iv.ToggleActualSize()
	iv.Step(1)
	if iv.path != "c.png" {
		t.Fatalf("a step forward went to %q", iv.path)
	}
	if !iv.actual {
		t.Error("the 1:1 mode must ride along to the next picture")
	}
}

func TestImageViewDoubleClickTogglesActualSize(t *testing.T) {
	iv := newTestImageView(t, 100, 100)

	press := func() bool {
		return iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: '*'})
	}
	if !press() || !iv.actual {
		t.Fatal("* must switch to the actual size")
	}
	if !press() || iv.actual {
		t.Fatal("* must switch back to the fitted size")
	}

	// A double left click behaves exactly like the * key.
	if !iv.ProcessMouse(&vtinput.InputEvent{
		Type: vtinput.MouseEventType, KeyDown: true,
		ButtonState:     vtinput.FromLeft1stButtonPressed,
		MouseEventFlags: vtinput.DoubleClick,
	}) || !iv.actual {
		t.Error("a double click must switch to the actual size")
	}
	if iv.ProcessMouse(&vtinput.InputEvent{
		Type: vtinput.MouseEventType, KeyDown: true,
		ButtonState: vtinput.FromLeft1stButtonPressed,
	}) {
		t.Error("a single click must be left alone")
	}
	if iv.ProcessMouse(&vtinput.InputEvent{Type: vtinput.KeyEventType}) {
		t.Error("non-mouse events must be left to the key path")
	}
}

func TestImageViewInsertPicksWithoutMoving(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "a.png"
	iv.SetSiblings([]string{"a.png", "b.png"}, 0)
	for _, name := range []string{"a.png", "b.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	var told []string
	var states []bool
	iv.OnSelect = func(path string, on bool) {
		told = append(told, path)
		states = append(states, on)
	}

	press := func(vk uint16) bool {
		return iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vk})
	}

	if !press(vtinput.VK_INSERT) {
		t.Fatal("Insert must be handled")
	}
	if !iv.selected["a.png"] {
		t.Error("Insert must pick the picture on screen")
	}
	if iv.path != "a.png" {
		t.Errorf("Insert must toggle selection without moving on, got %q", iv.path)
	}

	iv.GoTo(0)
	if !press(vtinput.VK_DELETE) {
		t.Fatal("Delete must be handled")
	}
	if iv.selected["a.png"] {
		t.Error("Delete must unpick the picture on screen")
	}

	if len(told) != 2 || told[0] != "a.png" || told[1] != "a.png" || !states[0] || states[1] {
		t.Errorf("the panel was told %v %v", told, states)
	}
}

func TestImageViewTitleMarksAPickedPicture(t *testing.T) {
	iv := newTestImageView(t, 10, 10)
	if iv.titleName() != "test.png" {
		t.Fatal("an unpicked picture must not be marked")
	}

	iv.SetSelected(iv.path, true)
	if iv.titleName() != "*test.png" {
		t.Errorf("a picked picture must be marked in the title, got %q", iv.titleName())
	}

	iv.SetSelected(iv.path, false)
	if iv.titleName() != "test.png" {
		t.Error("unpicking must take the mark away again")
	}
}

// TestImageViewWindowTitleIsNameAndResolution checks that the window title
// (and with it the workspace tab) stays short: just the name and the size in
// parentheses, "photo.jpg (1920x1080)".
func TestImageViewWindowTitleIsNameAndResolution(t *testing.T) {
	iv := newTestImageView(t, 640, 480)
	iv.path = "image.jpg"
	if got := iv.GetTitle(); got != "image.jpg (640x480)" {
		t.Errorf("window title is %q", got)
	}
}

// TestImageViewWindowTitleWithoutImage checks the title path survives an
// empty view: AddScreen/SwitchScreen resolve GetTitle for every workspace, so
// a view with no decoded surface must not dereference nil in displaySize.
func TestImageViewWindowTitleWithoutImage(t *testing.T) {
	iv := &ImageView{BaseFrame: vtui.BaseFrame{}, path: "photo.jpg"}
	if got := iv.GetTitle(); got != "photo.jpg (?)" {
		t.Errorf("window title for an empty view is %q, want %q", got, "photo.jpg (?)")
	}
}

// TestImageViewDecodeResultKeepsNoToast locks in that a finished decode does
// not pop a report into the corner: a slow decode is already announced by the
// loading toast, and a report between pictures makes the OSD jump.
func TestImageViewDecodeResultKeepsNoToast(t *testing.T) {
	iv := newTestImageView(t, 10, 10)
	iv.toast("scale: 100%")

	res := ImageResult{Surface: vtui.NewImageSurface(20, 20), Decoder: "png", DecodeDur: time.Second}
	iv.accept(iv.loadGen, res)
	if iv.tempMsg != "scale: 100%" {
		t.Errorf("a decode result must not overwrite a user message, got %q", iv.tempMsg)
	}

	iv.tempMsg = ""
	iv.accept(iv.loadGen, res)
	if iv.tempMsg != "" {
		t.Errorf("a decode result must not show a report toast, got %q", iv.tempMsg)
	}
}

func TestImageViewDecodeErrorToast(t *testing.T) {
	iv := newTestImageView(t, 10, 10)

	iv.accept(iv.loadGen, ImageResult{Err: errors.New("broken")})
	if iv.err == nil || iv.err.Error() != "broken" {
		t.Fatalf("the error must stay in the state, got %v", iv.err)
	}
	if iv.tempMsg != "error: broken" {
		t.Errorf("the error must reach the toast too, got %q", iv.tempMsg)
	}
}

func TestImageViewNavigationFollowsToThePanel(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "a.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 0)
	for _, name := range []string{"a.png", "b.png", "c.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	var followed []string
	iv.OnNavigate = func(path string) { followed = append(followed, path) }

	iv.Step(1)
	if len(followed) != 1 || followed[0] != "b.png" {
		t.Fatalf("stepping on must report the new picture, got %v", followed)
	}

	iv.Close()
	if len(followed) != 2 || followed[1] != "b.png" {
		t.Errorf("closing must leave the cursor on the viewed picture, got %v", followed)
	}
}

func TestImageViewPrefetchesItsNeighbours(t *testing.T) {
	asked := withStubPipeline(t, 8, 8)
	// The ring beyond the full decodes is only thumbnailed: catch those
	// preview requests on a second channel.
	previewed := make(chan string, 8)
	ImagePipe.preview = func(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
		previewed <- path
		return imageTestSurface(4, 4), imagePreviewDecoder, nil
	}

	iv := newTestImageView(t, 100, 100)
	iv.path = "2.jpg"
	iv.SetSiblings([]string{"0.jpg", "1.jpg", "2.jpg", "3.jpg", "4.jpg"}, 2)

	// The nearest neighbours are decoded whole; the ones beyond them are
	// only worth a thumbnail (JPEG names, so the preview filter lets them
	// through).
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		deadline := time.NewTimer(time.Second)
		select {
		case path := <-asked:
			if !deadline.Stop() {
				<-deadline.C
			}
			seen[path] = true
		case <-deadline.C:
			t.Fatalf("only %d neighbours were decoded: %v", len(seen), seen)
		}
	}
	for _, want := range []string{"1.jpg", "3.jpg"} {
		if !seen[want] {
			t.Errorf("%s was not decoded whole", want)
		}
	}
	if seen["2.jpg"] {
		t.Error("the picture on screen is not its own neighbour")
	}

	previewSeen := map[string]bool{}
	for i := 0; i < 2; i++ {
		deadline := time.NewTimer(time.Second)
		select {
		case path := <-previewed:
			if !deadline.Stop() {
				<-deadline.C
			}
			previewSeen[path] = true
		case <-deadline.C:
			t.Fatalf("only %d thumbnails were prepared: %v", len(previewSeen), previewSeen)
		}
	}
	for _, want := range []string{"0.jpg", "4.jpg"} {
		if !previewSeen[want] {
			t.Errorf("%s was not thumbnailed", want)
		}
	}
}

func TestImageViewActualSize(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)

	// Fitted, a square picture fills the 24 rows of the window.
	p, _ := iv.placementFor(scr)
	if p.Rows != 24 {
		t.Fatalf("fitted size: %dx%d cells", p.Cols, p.Rows)
	}

	iv.ToggleActualSize()
	p, ok := iv.placementFor(scr)
	if !ok {
		t.Fatal("layout failed")
	}
	// A hundred pixels are thirteen cells of eight and seven cells of
	// sixteen.
	if p.Cols != 13 || p.Rows != 7 {
		t.Errorf("actual size: %dx%d cells", p.Cols, p.Rows)
	}
	if iv.lastScale != 1 {
		t.Errorf("the actual size is a scale of one, got %v", iv.lastScale)
	}
	if iv.tempMsg != "scale: 100%" {
		t.Errorf("the literal size must say so, got %q", iv.tempMsg)
	}

	iv.ToggleActualSize()
	if p, _ = iv.placementFor(scr); p.Rows != 24 {
		t.Errorf("switching back must fit the window again: %dx%d", p.Cols, p.Rows)
	}
	if iv.tempMsg != "scale: 384%" {
		t.Errorf("back to the fitted scale, got %q", iv.tempMsg)
	}
}

func TestImageViewFullscreenTakesTheBarRows(t *testing.T) {
	restoreBars(t)
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)

	iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: 'f'})
	if !vtui.FrameManager.HideBars {
		t.Fatal("the key bar is drawn by the manager and has to be told to go away")
	}
	p, ok := iv.placementFor(scr)
	if !ok {
		t.Fatal("layout failed")
	}
	if p.Row != 0 || p.Rows != 25 {
		t.Errorf("without the bars the picture starts at row 0 and fills 25: %d, %d rows", p.Row, p.Rows)
	}
}

// TestImageViewLeavesRoomForWorkspaceTabs checks that the viewer drops by one
// row when the workspace tab strip reserves the top row: the picture starts
// below the tabs in normal mode, and in full screen it leaves the tab row
// alone instead of covering it.
func TestImageViewLeavesRoomForWorkspaceTabs(t *testing.T) {
	restoreBars(t)
	scr := newImageTestScreen(t)

	// "Always" and "when multiple" both reserve one top row, so the layout is
	// the same; the second workspace the viewer itself creates is what brings
	// the strip up in the "when multiple" mode.
	wasMode := vtui.FrameManager.WorkspaceTabMode
	vtui.FrameManager.WorkspaceTabMode = vtui.WorkspaceTabsAlways
	t.Cleanup(func() { vtui.FrameManager.WorkspaceTabMode = wasMode })

	iv := newTestImageView(t, 100, 100)
	if got := vtui.FrameManager.WorkspaceTopInset(); got != 1 {
		t.Fatalf("test setup: expected one reserved top row, got %d", got)
	}

	// Normal mode: the viewer starts below the tabs and the picture begins one
	// row down.
	if _, y1, _, _ := iv.GetPosition(); y1 != 1 {
		t.Errorf("the viewer must start below the tab row, got y1=%d", y1)
	}
	p, ok := iv.placementFor(scr)
	if !ok {
		t.Fatal("layout failed")
	}
	if p.Row != 1 || p.Rows != 23 {
		t.Errorf("without full screen the picture must start at row 1 and get 23 rows, got row %d, %d rows", p.Row, p.Rows)
	}

	// Full screen: the picture still leaves the tab row alone.
	iv.SetFullScreen(true)
	p, ok = iv.placementFor(scr)
	if !ok {
		t.Fatal("layout failed")
	}
	if p.Row != 1 || p.Rows != 24 {
		t.Errorf("in full screen the picture must start at row 1 and get 24 rows, got row %d, %d rows", p.Row, p.Rows)
	}
}

func TestImageViewIgnoresStaleResults(t *testing.T) {
	iv := newTestImageView(t, 100, 100)
	gen := iv.loadGen

	// The reader moved on while the decoder was busy.
	iv.loadGen++
	iv.accept(gen, ImageResult{Path: "old.png", Surface: vtui.NewImageSurface(7, 7)})
	if iv.surface.Width != 100 {
		t.Error("a result for a picture nobody is looking at any more must be dropped")
	}

	iv.accept(iv.loadGen, ImageResult{Path: "new.png", Surface: vtui.NewImageSurface(7, 7), Decoder: "stub"})
	if iv.surface.Width != 7 {
		t.Error("the awaited result must be taken")
	}
}

func TestImageViewFullScreenIsAToggle(t *testing.T) {
	restoreBars(t)
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)
	p, ok := iv.placementFor(scr)
	if !ok || p.Rows != 24 {
		t.Fatalf("with the key bar hidden the picture gets 24 rows, got %+v", p)
	}
	e := &vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_F}
	e.ControlKeyState |= vtinput.LeftCtrlPressed
	if !iv.ProcessKey(e) || !iv.full || !vtui.FrameManager.HideBars {
		t.Fatal("Ctrl+F did not reach whole screen mode")
	}
	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: 'f'}) || iv.full || vtui.FrameManager.HideBars {
		t.Error("F must leave whole screen mode")
	}
}

func TestImageViewCloseGivesTheBarsBack(t *testing.T) {
	restoreBars(t)
	iv := newTestImageView(t, 10, 10)
	iv.SetFullScreen(true)
	iv.Close()
	if vtui.FrameManager.HideBars {
		t.Error("a closed viewer must not leave bars hidden")
	}
}

func TestImageViewOverlayLines(t *testing.T) {
	iv := newTestImageView(t, 320, 200)
	iv.path, iv.decoder = "photo.png", "png"
	iv.fileSize, iv.sizeKnown = 4096, true
	iv.fileTime, iv.timeKnown = time.Date(2024, 5, 17, 9, 30, 0, 0, time.Local), true
	lines := iv.overlayLines()
	if len(lines) != 5 || lines[0] != "photo.png" || lines[1] != "320x200" || lines[4] != "png" || !strings.Contains(lines[2], "4.0") || lines[3] != "2024-05-17 09:30" {
		t.Errorf("the panel says %v", lines)
	}
	iv.Rotate(90)
	lines = iv.overlayLines()
	// The turn label joins before the decode timing, which stays the last line.
	if lines[1] != "200x320" || len(lines) != 6 || !strings.Contains(lines[4], "90") || lines[5] != "png" {
		t.Errorf("turned picture panel: %v", lines)
	}
	// No size and no time yet: the lines stay out, and no placeholder text
	// like "unknown size" may flash between pictures.
	bare := newTestImageView(t, 8, 8).overlayLines()
	if len(bare) != 3 || bare[2] != "test" {
		t.Errorf("size-less panel is %v", bare)
	}
}

// TestImageViewOverlayFreezesDuringSwitch locks in that the OSD keeps the
// previous picture's lines while its successor decodes: the picture on screen
// has not changed yet, so no "unknown size" or missing timestamp may flash.
func TestImageViewOverlayFreezesDuringSwitch(t *testing.T) {
	iv := newTestImageView(t, 320, 200)
	iv.path, iv.decoder = "photo.png", "png"
	iv.fileSize, iv.sizeKnown = 4096, true
	iv.fileTime, iv.timeKnown = time.Date(2024, 5, 17, 9, 30, 0, 0, time.Local), true
	before := strings.Join(iv.overlayLines(), "|")

	// A switch starts: open() snapshots the lines while the old picture is
	// still on screen, so the panel must not change even though size, time
	// and decoder are about to be reset for the next file.
	iv.osdPrev = iv.overlayLines()
	iv.loading = true
	iv.fileSize, iv.sizeKnown = 0, false
	iv.fileTime, iv.timeKnown = time.Time{}, false
	iv.decodeDur = 0
	if got := strings.Join(iv.overlayLines(), "|"); got != before {
		t.Errorf("OSD changed during the switch: %q vs %q", got, before)
	}
	// The final picture lands: the OSD speaks for it now, with its own data.
	iv.loading = false
	iv.osdPrev = nil
	iv.path, iv.decoder = "next.png", "jpeg"
	if got := iv.overlayLines(); len(got) == 0 || got[0] != "next.png" || got[len(got)-1] != "jpeg" {
		t.Errorf("the OSD must show the new picture after the switch: %v", got)
	}
}

// TestImageViewOverlayIgnoresIdentifiedBeforeDecode locks in the viewer's
// resolution consumer: the header-pass size is not reported before the
// picture decodes (it would flash over the surface still on screen), so the
// overlay and the title show only the decoded surface's dimensions. The
// camera's EXIF orientation is still read from the header.
func TestImageViewOverlayIgnoresIdentifiedBeforeDecode(t *testing.T) {
	withStubPipeline(t, 8, 8)
	iv := newTestImageView(t, 8, 8)
	iv.path = "photo.jpg"

	// The header pass identified the picture; no decode has happened yet.
	ImagePipe.mu.Lock()
	ImagePipe.idents.put(imageCacheKey{Path: "photo.jpg"}, imagedec.ImageHead{Width: 1600, Height: 900, Orientation: 6})
	ImagePipe.mu.Unlock()

	// The size on screen is the 8x8 surface; the header's 1600x900 must not
	// leak into the title or the panel ahead of the decode.
	if got := iv.displaySize(); got != "8x8" {
		t.Errorf("displaySize before decode is %q, want 8x8", got)
	}
	if got := iv.GetTitle(); strings.Contains(got, "1600x900") || strings.Contains(got, "900x1600") {
		t.Errorf("title before decode is %q, must not carry the header size", got)
	}
	lines := iv.overlayLines()
	if !strings.Contains(strings.Join(lines, "|"), "EXIF orientation 6") {
		t.Errorf("the camera's orientation must still come from the header, got %v", lines)
	}

	// Once the decoded surface lands, its own size (and the reader's turn)
	// is what is reported.
	iv.SetImage(ImageResult{Surface: vtui.NewImageSurface(1600, 900)})
	if got := iv.displaySize(); got != "1600x900" {
		t.Errorf("displaySize after decode is %q, want 1600x900", got)
	}
}

func TestImageViewOverlayGoesOverPicture(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)
	iv.path, iv.decoder = "photo.png", "png"
	if p, _ := iv.placementFor(scr); p.ZIndex != 0 {
		t.Fatalf("without overlay z index = %d", p.ZIndex)
	}
	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: 'i'}) {
		t.Fatal("I was not handled")
	}
	if p, _ := iv.placementFor(scr); p.ZIndex >= 0 {
		t.Errorf("overlay z index = %d", p.ZIndex)
	}
	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()
	if row := ScreenRow(scr, 1, 0, 20); !strings.Contains(row, "photo.png") {
		t.Errorf("overlay first line is %q", row)
	}
	if row := ScreenRow(scr, 2, 0, 20); !strings.Contains(row, "100x100") {
		t.Errorf("overlay second line is %q", row)
	}
}

func TestImageViewOverlayWidthIsCapped(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)
	iv.path, iv.decoder = "this-file-name-is-very-long-and-would-not-fit-anywhere.png", "png"
	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: 'i'}) {
		t.Fatal("I was not handled")
	}
	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()
	row := ScreenRow(scr, 1, 0, 79)
	if !strings.Contains(row, "…") || strings.Contains(row, "anywhere.png") {
		t.Errorf("truncated overlay row is %q", row)
	}
	slab := 0
	for x := 0; x < 80; x++ {
		if scr.GetCell(x, 1).Attributes == imageOverlayAttr {
			slab++
		}
	}
	if slab != 40 {
		t.Errorf("overlay slab is %d cells, want 40", slab)
	}

	// The wall flash inverts the pane for its short life.
	iv.tempFlashUntil = time.Now().Add(time.Second)
	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()
	flashed := 0
	for x := 0; x < 80; x++ {
		if scr.GetCell(x, 1).Attributes == imageWallFlashAttr {
			flashed++
		}
	}
	if flashed != 40 {
		t.Errorf("the wall flash must invert the overlay pane, %d cells", flashed)
	}
}

func TestImageViewToastNarrow(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 10, 10)
	iv.toast("scale: 100%")
	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()
	first, last := -1, -1
	for x := 0; x < 80; x++ {
		if scr.GetCell(x, 23).Attributes == imageToastAttr {
			if first < 0 {
				first = x
			}
			last = x
		}
	}
	if first != 0 || last != 12 {
		t.Errorf("toast slab spans %d..%d, want 0..12", first, last)
	}
}

func TestImageViewToastSlidesOutThenClears(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 10, 10)
	iv.toast("scale: 100%")
	iv.tempUntil = time.Now().Add(-time.Second)

	// The first paint after the expiry begins the exit; the message must
	// stay on the row while the slide runs.
	paint := func() {
		scr.Graphics().BeginFrame()
		iv.Show(scr)
		scr.Graphics().EndFrame()
	}
	paint()
	if iv.tempMsg != "scale: 100%" {
		t.Fatalf("the toast must not vanish in the same frame the slide starts, got %q", iv.tempMsg)
	}
	if iv.tempSlideStart.IsZero() {
		t.Fatal("an expired toast must begin its exit animation")
	}
	iv.stopAnimIfIdle()

	// Halfway through the window the toast has moved left: its right edge
	// has already left cell 12 even though the message is still on screen.
	iv.tempSlideStart = time.Now().Add(-100 * time.Millisecond)
	paint()
	if iv.tempMsg == "" {
		t.Fatal("mid-slide the message must still be alive")
	}
	if scr.GetCell(12, 23).Attributes == imageToastAttr {
		t.Error("mid-slide the toast must have left its right edge behind")
	}
	iv.stopAnimIfIdle()

	// Once the slide window is over the message is gone for good.
	iv.tempSlideStart = time.Now().Add(-imageToastSlideDur - time.Millisecond)
	paint()
	if iv.tempMsg != "" {
		t.Errorf("after the slide the toast must be gone, got %q", iv.tempMsg)
	}
	if iv.animStop != nil {
		t.Error("a finished slide must stop its redraw ticker")
	}
}

// One ticker serves every toast animation: starting it twice must leave a
// single redraw ticker, and stopping it once must end it.
func TestImageViewToastAnimationsShareOneTicker(t *testing.T) {
	iv := newTestImageView(t, 10, 10)
	iv.ensureAnim()
	iv.ensureAnim()
	if iv.animStop == nil {
		t.Fatal("ensureAnim must start the redraw ticker")
	}
	iv.stopAnimIfIdle()
	if iv.animStop != nil {
		t.Error("stopAnimIfIdle must stop the redraw ticker")
	}
}

// The wall flash is a paint-level state: once tempFlashUntil is in the
// past, the very next paint restores the resting colours.
func TestImageViewWallFlashRestoresRestingColours(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 10, 10)
	iv.toast("wall")
	iv.tempFlashUntil = time.Now().Add(time.Second)
	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()
	if scr.GetCell(0, 23).Attributes != imageWallFlashAttr {
		t.Fatal("the flash must show the toast in the inverted colours")
	}
	iv.tempFlashUntil = time.Now().Add(-time.Millisecond)
	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()
	if scr.GetCell(0, 23).Attributes != imageToastAttr {
		t.Error("once the flash has run out the toast must be back to its resting colours")
	}
}

// Clearing the toast must also end the redraw ticker that kept its
// animation in motion.
func TestImageViewToastClearStopsTicker(t *testing.T) {
	iv := newTestImageView(t, 10, 10)
	iv.toast("gone soon")
	iv.tempSlideStart = time.Now()
	iv.ensureAnim()
	iv.toastClear()
	if iv.animStop != nil {
		t.Error("clearing the toast must stop the redraw ticker")
	}
}

func TestImageViewLoadingToastNamesTheWait(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 10, 10)
	iv.loading = true
	iv.decodeStart = time.Now().Add(-time.Second)
	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()
	row := ScreenRow(scr, 23, 0, 79)
	if !strings.Contains(row, "decoding") {
		t.Errorf("the loading toast must name what it is waiting for, got %q", row)
	}
	iv.stopAnimIfIdle()
}

// The loading toast animates in pure grey and stays readable: the comet
// fades in from nothing and out into nothing (phase 0 and 1 are the plain
// toast), the slab shimmers inside its band range, and no cell ever
// brightens anywhere near the letters' grey — the brightest shade of the
// pass never lands under a letter of its own brightness.
func TestImageViewLoadingToastKeepsLettersReadable(t *testing.T) {
	for _, phase := range []float64{0, 1} {
		if slab := loadingToastShade(10, 40, phase); slab != imageSlabDim {
			t.Errorf("phase %v: slab %#x, want resting %#x", phase, slab, imageSlabDim)
		}
	}
	brightest := uint32(imageSlabDim)
	for x := range 40 {
		slab := loadingToastShade(x, 40, 0.5)
		if slab < imageSlabDim || slab > imageSlabBright {
			t.Errorf("x=%d: slab %#x outside the band range", x, slab)
		}
		// The gap to the text grey is the readability budget: it must never
		// close.
		if slab >= imageLoadingText {
			t.Errorf("x=%d: slab %#x collides with text grey %#x", x, slab, imageLoadingText)
		}
		if slab > brightest {
			brightest = slab
		}
	}
	if brightest == imageSlabDim {
		t.Errorf("comet never appears: brightest slab cell is the resting grey")
	}
	// The comet must move: the same cell brightens mid-pass and rests at
	// the pass quarter.
	a := loadingToastShade(11, 40, 0.25)
	b := loadingToastShade(11, 40, 0.75)
	if a == b {
		t.Errorf("comet does not move: slab %#x at both 0.25 and 0.75", a)
	}
}

func TestImageViewStepAnnouncesEdgesAndWalls(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "b.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 1)
	for _, name := range []string{"a.png", "b.png", "c.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	iv.Step(-1) // to a.png, the top edge
	if iv.tempMsg != "[1/3]" {
		t.Fatalf("arriving at the top must announce the position, got %q", iv.tempMsg)
	}
	if !iv.tempFlashUntil.IsZero() {
		t.Error("arriving at an edge must not flash")
	}

	iv.Step(-1) // above the top: the wall
	if iv.tempMsg != "[1/3]" {
		t.Errorf("a blocked step must keep the position, got %q", iv.tempMsg)
	}
	if iv.tempFlashUntil.IsZero() || time.Now().After(iv.tempFlashUntil) {
		t.Error("a blocked step must flash the toast")
	}

	iv.Step(+2) // to c.png, the bottom edge
	if iv.tempMsg != "[3/3]" {
		t.Errorf("arriving at the bottom must announce the position, got %q", iv.tempMsg)
	}
	if !iv.tempFlashUntil.IsZero() {
		t.Error("arriving at an edge must not flash")
	}

	iv.Step(-1) // back to b.png, a plain move
	if iv.tempMsg != "" {
		t.Errorf("a move to another picture must drop the toast, got %q", iv.tempMsg)
	}
}

func TestImageViewDecodeWaitOutranksThePosition(t *testing.T) {
	withStubPipeline(t, 20, 10)

	// Already on the first picture, with a decode still at work: the wall
	// must keep quiet, so the "decoding" label has the corner to itself.
	iv := newTestImageView(t, 100, 100)
	iv.path = "a.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 0)
	iv.loading = true
	iv.Step(-1) // blocked above the top
	if iv.tempMsg != "" {
		t.Errorf("a pending decode must outrank the wall toast, got %q", iv.tempMsg)
	}

	// Once the wait is over the wall may speak, and it flashes.
	iv.loading = false
	iv.Step(-1) // blocked again, now quiet
	if iv.tempMsg != "[1/3]" {
		t.Errorf("a quiet wall step must still announce, got %q", iv.tempMsg)
	}
	if iv.tempFlashUntil.IsZero() || time.Now().After(iv.tempFlashUntil) {
		t.Error("a quiet wall step must still flash")
	}

	// Another shove at the same wall blinks again without touching the
	// message.
	before := iv.tempFlashUntil
	// The wall-clock tick on some Windows systems is coarse enough that
	// two back-to-back time.Now() calls return the same instant, which
	// would make the re-flash indistinguishable from the first flash.
	time.Sleep(2 * time.Millisecond)
	iv.Step(-1)
	if iv.tempMsg != "[1/3]" {
		t.Errorf("a repeated shove must keep the message, got %q", iv.tempMsg)
	}
	if !iv.tempFlashUntil.After(before) {
		t.Error("a repeated shove at the wall must re-flash")
	}
}

func TestImageViewHomeEndAnnounceTheEdges(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "b.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 1)
	for _, name := range []string{"a.png", "b.png", "c.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	press := func(vk uint16) bool {
		return iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vk})
	}

	if !press(vtinput.VK_HOME) || iv.index != 0 {
		t.Fatalf("Home should go to the first picture, index is %d", iv.index)
	}
	if iv.tempMsg != "[1/3]" {
		t.Errorf("Home must announce the edge, got %q", iv.tempMsg)
	}

	if !press(vtinput.VK_END) || iv.index != 2 {
		t.Fatalf("End should go to the last picture, index is %d", iv.index)
	}
	if iv.tempMsg != "[3/3]" {
		t.Errorf("End must announce the edge, got %q", iv.tempMsg)
	}
}

func TestPanelImageSiblings(t *testing.T) {
	fp := &FileSystemPanel{
		entries: []*fileEntry{
			{VFSItem: vfs.VFSItem{Name: "..", IsDir: true}},
			{VFSItem: vfs.VFSItem{Name: "sub", IsDir: true}},
			{VFSItem: vfs.VFSItem{Name: "a.png"}},
			{VFSItem: vfs.VFSItem{Name: "notes.txt"}},
			{VFSItem: vfs.VFSItem{Name: "b.jpg"}},
		},
		cursorIdx: 4,
	}

	names, index := fp.ImageSiblings()
	if len(names) != 2 || names[0] != "a.png" || names[1] != "b.jpg" {
		t.Fatalf("the pictures of the panel: %v", names)
	}
	if index != 1 {
		t.Errorf("the cursor is on the second picture, got %d", index)
	}

	// A cursor on something that is not a picture has no position.
	fp.cursorIdx = 3
	if _, index := fp.ImageSiblings(); index != -1 {
		t.Errorf("expected no position, got %d", index)
	}
}

func TestImageViewRotationKeys(t *testing.T) {
	iv := newTestImageView(t, 40, 20)

	turn := func(char rune) {
		t.Helper()
		if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: char}) {
			t.Fatalf("%q was not handled", char)
		}
	}
	mirror := func(char rune) {
		t.Helper()
		e := &vtinput.InputEvent{KeyDown: true, Char: char}
		e.ControlKeyState |= vtinput.LeftAltPressed
		if !iv.ProcessKey(e) {
			t.Fatalf("Alt+%q was not handled", char)
		}
	}

	turn('>')
	if iv.rotation != 90 {
		t.Fatalf("one turn forward gave %d", iv.rotation)
	}
	if iv.display().Width != 20 || iv.display().Height != 40 {
		t.Errorf("the turned picture is %dx%d", iv.display().Width, iv.display().Height)
	}

	turn('.')
	if iv.rotation != 180 {
		t.Errorf("the dot must turn like the angle bracket, got %d", iv.rotation)
	}
	turn('<')
	turn(',')
	if iv.rotation != 0 {
		t.Errorf("turning back twice gave %d", iv.rotation)
	}
	if iv.shown != nil || iv.display() != iv.surface {
		t.Error("an unturned picture must be shown as it was decoded, without a copy")
	}

	mirror('>')
	if !iv.flipH || iv.flipV {
		t.Errorf("Alt+> mirrors across the vertical axis, got %v %v", iv.flipH, iv.flipV)
	}
	mirror('<')
	if !iv.flipV {
		t.Error("Alt+< mirrors across the horizontal axis")
	}
	if iv.rotation != 0 {
		t.Errorf("mirroring must not turn the picture, got %d", iv.rotation)
	}
	if iv.display().Width != 40 || iv.display().Height != 20 {
		t.Errorf("mirroring must keep the size, got %dx%d", iv.display().Width, iv.display().Height)
	}
}

func TestImageViewTurnFollowsTheMirror(t *testing.T) {
	iv := newTestImageView(t, 40, 20)

	// One mirrored axis reverses the direction of a turn, so the stored
	// angle has to move backwards for the picture on screen to move
	// forwards.
	iv.Flip(true, false)
	iv.Rotate(90)
	if iv.rotation != 270 {
		t.Errorf("with one axis mirrored a clockwise key must store 270, got %d", iv.rotation)
	}

	// Both axes together are a half turn, which commutes, so the angle goes
	// forward again.
	iv.Flip(false, true)
	iv.Rotate(90)
	if iv.rotation != 0 {
		t.Errorf("with both axes mirrored the angle must go forward, got %d", iv.rotation)
	}
}

func TestImageViewRotationChangesThePlacement(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 200, 100)

	wide, ok := iv.placementFor(scr)
	if !ok {
		t.Fatal("layout failed")
	}
	if wide.Surface != iv.surface {
		t.Error("an unturned picture is sent as it was decoded")
	}

	iv.Rotate(90)
	tall, ok := iv.placementFor(scr)
	if !ok {
		t.Fatal("layout failed")
	}
	if tall.Surface != iv.shown {
		t.Error("the turned copy is what has to reach the terminal")
	}
	if tall.Cols >= wide.Cols || tall.Rows <= wide.Rows {
		t.Errorf("a turned landscape picture must stand up: %dx%d cells against %dx%d",
			tall.Cols, tall.Rows, wide.Cols, wide.Rows)
	}
}

func TestImageViewRotationSurvivesTheSharperPicture(t *testing.T) {
	iv := newTestImageView(t, 40, 20)
	iv.Rotate(90)

	// The full resolution decode replaces the thumbnail of the same file,
	// which must not undo what the reader has done to the orientation.
	iv.SetImage(ImageResult{Surface: vtui.NewImageSurface(80, 40), Decoder: "stub"})
	if iv.rotation != 90 {
		t.Fatalf("the thumbnail and the picture share an orientation, got %d", iv.rotation)
	}
	if iv.display().Width != 40 || iv.display().Height != 80 {
		t.Errorf("the sharper picture arrived unturned: %dx%d", iv.display().Width, iv.display().Height)
	}
}

func TestImageViewOrientationResetsOnTheNextPicture(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "a.png"
	iv.SetSiblings([]string{"a.png", "b.png"}, 0)
	if res := ImagePipe.LoadSync(context.Background(), nil, "b.png"); res.Err != nil {
		t.Fatalf("b.png: %v", res.Err)
	}

	iv.Rotate(90)
	iv.Flip(true, false)
	iv.Step(1)

	if iv.path != "b.png" {
		t.Fatalf("the step went to %q", iv.path)
	}
	if iv.rotation != 0 || iv.flipH || iv.flipV || iv.shown != nil {
		t.Errorf("the next picture must arrive as it was decoded: %d %v %v",
			iv.rotation, iv.flipH, iv.flipV)
	}
	if iv.display().Width != 20 || iv.display().Height != 10 {
		t.Errorf("the picture on screen is %dx%d", iv.display().Width, iv.display().Height)
	}
}

// TestImageViewModeSync locks in that the graphics and the cell renderers see
// the same view at the same zoom: the placement is anchored to the canonical
// half-block grid, so the fitted scale, the visible crop and the pan range are
// identical in both modes whatever the terminal's real cell aspect is.
func TestImageViewModeSync(t *testing.T) {
	for _, cells := range [][2]int{{8, 16}, {10, 20}, {9, 17}, {8, 15}} {
		for _, z := range []float64{1, 2.5, 4} {
			scr := newImageTestScreen(t)
			iv := newTestImageView(t, 4000, 2000)
			iv.surface = solidSurface(4000, 2000, 0xCC8855)
			iv.zoom = z
			iv.panX, iv.panY = 300, 200
			gfx, ok1 := iv.placementForSize(scr, cells[0], cells[1])
			gfxScale := iv.lastScale
			iv.panX, iv.panY = 300, 200
			block, ok2 := iv.placementForSize(scr, 1, 2)
			if !ok1 || !ok2 {
				t.Fatalf("cells=%dx%d zoom=%v: placement failed (%v, %v)", cells[0], cells[1], z, ok1, ok2)
			}
			if gfx.Col != block.Col || gfx.Row != block.Row ||
				gfx.Cols != block.Cols || gfx.Rows != block.Rows ||
				gfx.SrcX != block.SrcX || gfx.SrcY != block.SrcY ||
				gfx.SrcW != block.SrcW || gfx.SrcH != block.SrcH {
				t.Errorf("cells=%dx%d zoom=%v: renderers disagree\ngfx   = %+v\nblock = %+v",
					cells[0], cells[1], z, gfx, block)
			}
			if iv.lastScale != gfxScale {
				t.Errorf("cells=%dx%d zoom=%v: the reported scale must not depend on the mode: %v vs %v",
					cells[0], cells[1], z, gfxScale, iv.lastScale)
			}
		}
	}
}

// TestImageViewActualSizeZoomKeepsTheZoom: zooming while the literal 1:1
// size is on must still zoom, with the reported scale following the zoom.
func TestImageViewActualSizeZoomKeepsTheZoom(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 4000, 2000)
	iv.surface = solidSurface(4000, 2000, 0xCC8855)
	iv.actual = true
	iv.zoom = 2
	p, ok := iv.placementFor(scr)
	if !ok {
		t.Fatal("layout failed")
	}
	if p.SrcW <= 0 || p.SrcW >= 4000 {
		t.Errorf("zoomed 1:1 must show a crop of the source, got %d", p.SrcW)
	}
	if iv.lastScale != 2 {
		t.Errorf("the zoomed literal size must report scale two, got %v", iv.lastScale)
	}
}
