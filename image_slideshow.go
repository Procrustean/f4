package main

import (
	"time"

	"github.com/unxed/vtui"
)

// defaultSlideShowDelay is the fallback seconds per picture.
const defaultSlideShowDelay = 5

// slideShowInterval is how long one picture is shown; zero or negative falls
// back to the default.
func slideShowInterval() time.Duration {
	seconds := AppConfig.SlideShowDelay
	if seconds <= 0 {
		seconds = defaultSlideShowDelay
	}
	return time.Duration(seconds) * time.Second
}

// ToggleSlideShow starts or stops walking the pictures on a timer. The timer
// goroutine only asks the UI thread (which owns the index, pipeline and
// placement) to take the next step.
func (iv *ImageView) ToggleSlideShow() {
	if iv.slideStop != nil {
		iv.stopSlideShow()
		return
	}
	if len(iv.siblings) < 2 {
		return // one picture is not a slide show
	}

	// The grid and the show can't both own the current picture.
	iv.gal = nil

	stop := make(chan struct{})
	iv.slideStop = stop
	interval := slideShowInterval()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				vtui.FrameManager.PostTask(func() {
					// The reader may have stopped the show between tick and task.
					if iv.slideStop == stop {
						iv.slideStep()
					}
				})
			}
		}
	}()
}

// stopSlideShow closes the timer's channel, so the goroutine wakes at once.
func (iv *ImageView) stopSlideShow() {
	if iv.slideStop == nil {
		return
	}
	close(iv.slideStop)
	iv.slideStop = nil
}

// slideStep shows the next picture, wrapping at the end (unlike Step).
func (iv *ImageView) slideStep() {
	total := len(iv.siblings)
	if total == 0 {
		iv.stopSlideShow()
		return
	}
	next := iv.index + 1
	if next < 0 || next >= total {
		next = 0
	}
	iv.GoTo(next)
	vtui.FrameManager.Redraw()
}
