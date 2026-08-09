package main

import (
	"strings"
	"testing"
	"time"

	"github.com/unxed/vtinput"
	"github.com/unxed/vtui"
)

// restoreBars keeps the whole screen flag from leaking between tests: it
// lives on the frame manager, not on the frame.
func restoreBars(t *testing.T) {
	t.Helper()
	was := vtui.FrameManager.HideBars
	t.Cleanup(func() { vtui.FrameManager.HideBars = was })
}

func TestImageViewFullScreenIsAToggle(t *testing.T) {
	restoreBars(t)
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)

	p, ok := iv.placementFor(scr)
	if !ok {
		t.Fatal("layout failed")
	}
	if p.Rows != 23 {
		t.Fatalf("with both bars the picture gets 23 rows, got %d", p.Rows)
	}

	e := &vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_F}
	e.ControlKeyState |= vtinput.LeftCtrlPressed
	if !iv.ProcessKey(e) {
		t.Fatal("Ctrl+F was not handled")
	}
	if !iv.full || !vtui.FrameManager.HideBars {
		t.Fatal("Ctrl+F did not reach the whole screen mode")
	}

	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: 'f'}) {
		t.Fatal("F was not handled")
	}
	if iv.full || vtui.FrameManager.HideBars {
		t.Error("leaving the whole screen mode must give the bars back")
	}
	if p, _ = iv.placementFor(scr); p.Rows != 23 {
		t.Errorf("back to 23 rows, got %d", p.Rows)
	}
}

func TestImageViewCloseGivesTheBarsBack(t *testing.T) {
	restoreBars(t)
	iv := newTestImageView(t, 10, 10)

	iv.SetFullScreen(true)
	if !vtui.FrameManager.HideBars {
		t.Fatal("the flag did not reach the manager")
	}
	iv.Close()
	if vtui.FrameManager.HideBars {
		t.Error("a closed viewer must not leave the key bar hidden")
	}
}

func TestImageViewOverlayLines(t *testing.T) {
	iv := newTestImageView(t, 320, 200)
	iv.path = "photo.png"
	iv.decoder = "png"
	iv.fileSize, iv.sizeKnown = 4096, true
	iv.fileTime, iv.timeKnown = time.Date(2024, 5, 17, 9, 30, 0, 0, time.Local), true

	lines := iv.overlayLines()
	if len(lines) != 5 {
		t.Fatalf("an unturned picture describes itself in five lines: %v", lines)
	}
	if lines[0] != "photo.png" || lines[1] != "320 x 200" || lines[4] != "png" {
		t.Errorf("the panel says %v", lines)
	}
	if !strings.Contains(lines[2], "4.0") {
		t.Errorf("the file size line is %q", lines[2])
	}
	if lines[3] != "2024-05-17 09:30" {
		t.Errorf("the date line is %q", lines[3])
	}

	// The panel describes what is on screen, not what came out of the decoder.
	iv.Rotate(90)
	lines = iv.overlayLines()
	if lines[1] != "200 x 320" {
		t.Errorf("after a quarter turn the size line is %q", lines[1])
	}
	if len(lines) != 6 || !strings.Contains(lines[5], "90") {
		t.Errorf("a turned picture has to say so: %v", lines)
	}

	unasked := newTestImageView(t, 8, 8)
	if got := unasked.overlayLines()[2]; got != "unknown size" {
		t.Errorf("a size nobody has asked the file system for is %q", got)
	}
}

func TestImageViewOverlayGoesOverThePicture(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)
	iv.path = "photo.png"
	iv.decoder = "png"

	if p, _ := iv.placementFor(scr); p.ZIndex != 0 {
		t.Fatalf("without the overlay the placement keeps the default z index, got %d", p.ZIndex)
	}

	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: 'i'}) {
		t.Fatal("I was not handled")
	}
	p, _ := iv.placementFor(scr)
	if p.ZIndex >= 0 {
		t.Errorf("with the overlay up the picture has to go under the glyphs, got z %d", p.ZIndex)
	}

	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()

	if row := ScreenRow(scr, 2, 0, 20); !strings.Contains(row, "photo.png") {
		t.Errorf("the first line of the panel is %q", row)
	}
	if row := ScreenRow(scr, 3, 0, 20); !strings.Contains(row, "100 x 100") {
		t.Errorf("the second line of the panel is %q", row)
	}

	e := &vtinput.InputEvent{KeyDown: true, VirtualKeyCode: vtinput.VK_I}
	e.ControlKeyState |= vtinput.LeftCtrlPressed
	if !iv.ProcessKey(e) || iv.overlay {
		t.Error("Ctrl+I must switch the panel off again")
	}
}

func TestImageViewOverlayWidthIsCapped(t *testing.T) {
	scr := newImageTestScreen(t)
	iv := newTestImageView(t, 100, 100)
	iv.path = "this-file-name-is-very-long-and-would-not-fit-anywhere.png"
	iv.decoder = "png"

	if !iv.ProcessKey(&vtinput.InputEvent{KeyDown: true, Char: 'i'}) {
		t.Fatal("I was not handled")
	}
	scr.Graphics().BeginFrame()
	iv.Show(scr)
	scr.Graphics().EndFrame()

	row := screenRow(scr, 2, 0, 79)
	if !strings.Contains(row, "…") {
		t.Errorf("a name that does not fit ends with an ellipsis, got %q", row)
	}
	if strings.Contains(row, "anywhere.png") {
		t.Errorf("the tail of the name must be cut off, got %q", row)
	}
	slab := 0
	for x := 0; x < 80; x++ {
		if scr.GetCell(x, 2).Attributes == imageOverlayAttr {
			slab++
		}
	}
	if slab != 40 {
		t.Errorf("the panel is %d cells wide, want the half frame: 40", slab)
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
		t.Errorf("the toast slab spans %d..%d, want 0..12", first, last)
	}
}
