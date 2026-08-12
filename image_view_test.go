package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/unxed/f4/vfs"
	"github.com/unxed/vtinput"
	"github.com/unxed/vtui"
)

func newImageTestScreen(t *testing.T) *vtui.ScreenBuf {
	t.Helper()
	scr := vtui.NewScreenBuf()
	scr.Writer = io.Discard
	scr.AllocBuf(80, 25)
	scr.Graphics().SetProtocol(vtui.GraphicsKitty)
	scr.Graphics().SetCellSize(8, 16)
	return scr
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

// screenRow reads a stretch of one row back out of the screen.
func screenRow(scr *vtui.ScreenBuf, y, x1, x2 int) string {
	var b strings.Builder
	for x := x1; x <= x2; x++ {
		if r := rune(scr.GetCell(x, y).Char); r != 0 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestImageViewFitsAndCentres(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)

	p, ok := iv.placementFor(scr)
	if !ok {
		t.Fatal("layout failed")
	}
	// The window has 23 available rows (h=25, minus topbar and bottom border).
	// Available cells: 80x23, cell size 8x16 -> 640x368 pixels.
	// A square image fits to 368x368, which is 46x23 cells, centred horizontally.
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
	if press('~', 0) {
		t.Error("unrelated keys must be left to the rest of the UI")
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

func TestImageViewArrowsWalkWhenThereIsNothingToPan(t *testing.T) {
	withStubPipeline(t, 20, 10)

	iv := newTestImageView(t, 100, 100)
	iv.path = "b.png"
	iv.SetSiblings([]string{"a.png", "b.png", "c.png"}, 1)
	for _, name := range []string{"a.png", "c.png"} {
		if res := ImagePipe.LoadSync(context.Background(), nil, name); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
	}

	press := func(vk uint16) bool {
		return iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vk})
	}

	// Nothing has been drawn yet and the picture fits anyway, so the pan
	// range is zero and the arrows walk the directory.
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
		t.Errorf("nothing should have been panned: %v %v", iv.panX, iv.panY)
	}
}

func TestImageViewArrowsPanAZoomedPicture(t *testing.T) {
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

	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_RIGHT}) {
		t.Fatal("the right arrow must be handled")
	}
	if iv.panX <= 0 {
		t.Error("a zoomed picture must be panned, not walked past")
	}
	if iv.path != "test.png" || iv.index != 0 {
		t.Errorf("the list must not have moved: %q %d", iv.path, iv.index)
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

func TestImageViewDecodeToastDefersToUser(t *testing.T) {
	iv := newTestImageView(t, 10, 10)
	iv.toast("scale: 100%")

	res := ImageResult{Surface: vtui.NewImageSurface(20, 20), Decoder: "png", DecodeDur: time.Second}
	iv.accept(iv.loadGen, res)
	if iv.tempMsg != "scale: 100%" {
		t.Errorf("a decode report must not overwrite a user message, got %q", iv.tempMsg)
	}

	iv.tempMsg = ""
	iv.accept(iv.loadGen, res)
	if iv.tempMsg != "png 1.00s" {
		t.Errorf("with the slot free the decode report is shown, got %q", iv.tempMsg)
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
	iv.path = "2.png"
	iv.SetSiblings([]string{"0.png", "1.png", "2.png", "3.png", "4.png"}, 2)

	// The nearest neighbours are decoded whole; the ones beyond them are
	// only worth a thumbnail.
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
	for _, want := range []string{"1.png", "3.png"} {
		if !seen[want] {
			t.Errorf("%s was not decoded whole", want)
		}
	}
	if seen["2.png"] {
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
	for _, want := range []string{"0.png", "4.png"} {
		if !previewSeen[want] {
			t.Errorf("%s was not thumbnailed", want)
		}
	}
}

func TestImageViewActualSize(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)

	// Fitted, a square picture fills the 23 rows of the window.
	p, _ := iv.placementFor(scr)
	if p.Rows != 23 {
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
	if p, _ = iv.placementFor(scr); p.Rows != 23 {
		t.Errorf("switching back must fit the window again: %dx%d", p.Cols, p.Rows)
	}
	if iv.tempMsg != "scale: 368%" {
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
	if !ok || p.Rows != 23 {
		t.Fatalf("with both bars the picture gets 23 rows, got %+v", p)
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
	if len(lines) != 5 || lines[0] != "photo.png" || lines[1] != "320 x 200" || lines[4] != "png" || !strings.Contains(lines[2], "4.0") || lines[3] != "2024-05-17 09:30" {
		t.Errorf("the panel says %v", lines)
	}
	iv.Rotate(90)
	lines = iv.overlayLines()
	if lines[1] != "200 x 320" || len(lines) != 6 || !strings.Contains(lines[5], "90") {
		t.Errorf("turned picture panel: %v", lines)
	}
	if got := newTestImageView(t, 8, 8).overlayLines()[2]; got != "unknown size" {
		t.Errorf("unknown size line is %q", got)
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
	if row := screenRow(scr, 2, 0, 20); !strings.Contains(row, "photo.png") {
		t.Errorf("overlay first line is %q", row)
	}
	if row := screenRow(scr, 3, 0, 20); !strings.Contains(row, "100 x 100") {
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
	row := screenRow(scr, 2, 0, 79)
	if !strings.Contains(row, "…") || strings.Contains(row, "anywhere.png") {
		t.Errorf("truncated overlay row is %q", row)
	}
	slab := 0
	for x := 0; x < 80; x++ {
		if scr.GetCell(x, 2).Attributes == imageOverlayAttr {
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
		if scr.GetCell(x, 2).Attributes == imageWallFlashAttr {
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
	row := screenRow(scr, 23, 0, 79)
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
