package main

import (
	"github.com/mattn/go-runewidth"
	"github.com/unxed/vtui"
)

// TopBar is a generic top status bar used by Editor and Viewer.
type TopBar struct {
	vtui.Bar
	GetLeft  func() string
	GetRight func() string
	ColorIdx int

	// GetAttr, when set, chooses the colour of the bar. A zero answer means
	// the palette decides, so a frame only has to say something when it has
	// something to say. It is how the picture viewer shows that the file on
	// screen is selected.
	GetAttr func() uint64
}

func NewTopBar(getLeft, getRight func() string) *TopBar {
	return &TopBar{GetLeft: getLeft, GetRight: getRight, ColorIdx: ColViewerStatus}
}

func (tb *TopBar) Show(scr *vtui.ScreenBuf) {
	tb.Bar.Show(scr)
	if !tb.IsVisible() {
		return
	}
	attr := vtui.Palette[tb.ColorIdx]
	if tb.GetAttr != nil {
		if a := tb.GetAttr(); a != 0 {
			attr = a
		}
	}
	tb.DrawBackground(scr, attr)

	leftStr := ""
	if tb.GetLeft != nil {
		leftStr = tb.GetLeft()
	}
	rightStr := ""
	if tb.GetRight != nil {
		rightStr = tb.GetRight()
	}

	width := tb.X2 - tb.X1 + 1
	leftW := runewidth.StringWidth(leftStr)
	rightW := runewidth.StringWidth(rightStr)

	if leftW+rightW > width {
		if width > leftW+1 {
			// The status is shortened from the middle so that the size at the
			// start and the state at the end survive; the name keeps its cell
			// budget until there is no room left for the status at all.
			rightStr = truncateMiddle(rightStr, width-leftW-1)
			rightW = runewidth.StringWidth(rightStr)
		} else {
			rightStr = ""
			rightW = 0
			leftStr = runewidth.Truncate(leftStr, width, "…")
		}
	}

	if leftStr != "" {
		scr.Write(tb.X1, tb.Y1, vtui.StringToCharInfo(leftStr, attr))
	}
	if rightStr != "" {
		scr.Write(tb.X2-runewidth.StringWidth(rightStr)+1, tb.Y1, vtui.StringToCharInfo(rightStr, attr))
	}
}

func truncateMiddle(s string, limit int) string {
	w := runewidth.StringWidth(s)
	if w <= limit {
		return s
	}
	tail := (limit - 1) / 2
	post := runewidth.TruncateLeft(s, w-tail, "")
	head := limit - 1 - runewidth.StringWidth(post)
	if head < 0 {
		head = 0
	}
	return runewidth.Truncate(s, head, "") + "…" + post
}
