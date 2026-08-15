# Image and video support in f4 — plan and handover

This file is the entry point for the work on issue #186 (viewing pictures in
f4). It is written so that the work can be continued with nothing but the
repository at hand. Read it first, then, as needed:

- `TERMINAL.md` — the built-in terminal, including the kitty graphics protocol
  it accepts from child processes.
- `../vtui/GRAPHICS.md` — the graphics layer: `ImageSurface`, `ImagePlacement`,
  the protocol backends, cell size negotiation.
- `PLUGIN_PLAN.md` — unrelated work, but the same kind of document, and a good
  example of the shape this one is meant to keep.

## 1. How the work is done

The user is unxed, the author of f4. The conversation is in Russian; **all
code, comments and documentation are in English**. Testing happens on Linux
Mint 22.3 Cinnamon (X11) with `./run_all_tests.sh`; CI builds every OS,
including 32-bit ARM.

Rules, which have not changed since the plugin work:

- Every reply with code carries **tests** and an **English commit message**.
- Split a large task across several replies and start with a plan.
- Nothing from far2l copied verbatim: it is GPL2 and f4 is not.

The user values being told *why* a solution looks the way it does, being shown
the fork in the road and the reason one branch was taken, and being told what
was found along the way. Do not smooth things over.

## 2. Architecture

**One pipeline, several consumers.** `ImagePipe` (`image_pipeline.go`) is the
only thing that turns a path into pixels: it caches decoded surfaces, merges
concurrent requests for the same file, runs a small pool of workers, and
prefetches the neighbours of whatever is on screen. `PreviewSync` answers with
the best picture that can be had without decoding the whole file — a cached
one, a thumbnail seen earlier, or the thumbnail the file carries inside itself.
`LoadSync` decodes properly. Everything that shows a picture — the viewer, the
gallery, and eventually the quick view panel — asks the pipeline and never
touches a decoder directly.

**vtui ships rectangles of pixels, not pictures.** `ImageSurface` is straight
alpha RGBA; `ImagePlacement` says where on the cell grid it goes, which part of
the source to take, and at which z index. Backends (kitty today, iTerm2 and
sixel later, plus the native GUI renderers) know how to put that rectangle on
screen and nothing else. Anything the viewer wants to change about the picture
itself — a turn, a mirror — has to be baked into the pixels first. That is what
`image_transform.go` is for.

**The viewer is one frame with modes.** `ImageView` is a single frame that can
be showing one picture, the thumbnail grid, or a slide show. The grid and the
show are not separate frames because all three share the sibling list, the
pipeline, the graphics key and the set of picked pictures; separate frames
would need callbacks to keep four things in step and would gain nothing.

## 3. File map

In `f4` (package `main`):

- `image_pipeline.go` — the cache, the job queue, the workers, prefetch
- `image_lru.go` — the one weight-bounded LRU behind every pipeline cache
- `image_preview.go` — the Exif thumbnail path
- `image_transform.go` — rotation and mirroring over the RGBA bytes
- `image_block.go` — the half-block cell renderer (fallback for no graphics),
  with the plain (glyph-less) insurance tier and its `BlockPlain` setting
- `image_view.go` — the viewer frame: layout, zoom, panning, orientation,
  the info overlay, keys
- `image_gallery.go` — the F12 thumbnail grid and the shared selection
- `image_slideshow.go` — the Ctrl+S timer
- `imagedec/` — every decoder: `image_decode.go` (registry and stdlib),
  `image_qoi.go`, `image_bmp_ico.go`, `image_netpbm.go`, `image_external.go`
  (`magick`/`convert`/`ffmpeg`), `image_decode_win_api.go` (WIC and the
  Windows shell), `image_native_darwin.go`
- `kitty_graphics.go` — accepting the kitty protocol in the built-in terminal
- `actions.go` — `tryOpenImageViewer`, `imageSiblingPaths`, and the wiring of
  the selection between the viewer and the panel
- `file_panel.go` — `ImageSiblings`, `SetSelectedByName`, `IsNameSelected`
- `config.go` — the `[Images]` section

In `vtui`:

- `graphics.go` — `ImageSurface`, `ImagePlacement`, the placement list
- `graphics_kitty.go` — the kitty output backend
- `graphics_scale.go` — `FitInside` and the scalers
- `framemanager.go` — `HideBars`, for a frame that wants the bar rows
- `terminal_env.go` — protocol detection

## 4. Status

Done before the current sequence of work: accepting the kitty protocol in the
built-in terminal (transmission, placement, drawing through
`vtui.GraphicsLayer`, cursor movement per the specification); pixel geometry in
`TIOCSWINSZ` and in the answers to `CSI 14/16 t`; announcing image support to
the child process (`KITTY_WINDOW_ID`, `TERM_PROGRAM`, `TERM=xterm-kitty` under
a terminfo check and the `AnnounceKittyTerm` option); the decoding pipeline;
preview from the Exif thumbnail; QOI and BMP decoders; walking the neighbours
with prefetch; the 100% mode; `Ctrl+R`; errors shown in a toast.

Done in the current sequence, listed in the order the work happened rather
than in the order of the numbering below, which the work has already outgrown.
An entry numbered `R` comes from the issue tracker and one numbered `B` is a
defect; the twelve steps below foresaw neither.

**1. Rotation and mirroring.** `image_transform.go` rotates and mirrors a
surface in plain Go. `ImageView` keeps `rotation`, `flipH`, `flipV` and a
`shown` surface; `display()` returns `shown` when the orientation has been
changed and the decoded `surface` when it has not, so an untouched picture is
never copied. Keys: `>` and `.` turn clockwise, `<` and `,` counter-clockwise,
`Alt+>` mirrors across the vertical axis, `Alt+<` across the horizontal one.

**2. Full screen and the info overlay.** `FrameManager.HideBars` in vtui;
`ImageView.ResizeConsole` remembers the console size and gives the key bar row
to the picture in full screen; `F` and `Ctrl+F` switch it, `Close` gives it
back. `Ctrl+I` (and `I`) raises a panel with the name, the picture size, the
file size and its modification time, the decoder and decode duration, and the
orientation.

**3. The gallery and the slide show.** `F12` opens a grid of thumbnails;
`Ins` and `Del` pick and unpick, and the choice is shared with the panel in
both directions. `Ctrl+S` runs a slide show with the interval from
`[Images] SlideShowDelay`, five seconds by default.

**4. Quick view on `Ctrl+Q`.** In `quick_view_panel.go`, shows a picture through
the pipeline instead of the hex dump when one is under the cursor. The
placement is computed the way `ImageView.placementFor` does it, but inside the
bounds of the panel.

**5. External decoder.** `image_external.go` registers a decoder at priority
−10 that hands the file to `magick`, `convert` or `ffmpeg` — whichever is on
the `PATH` — and reads PNG back from its standard output. It is registered
only when one of them is there, and it claims only the formats that one can
read, so `IsImageFile` never promises a picture the machine cannot open. The
`[Images]` section gained `ExternalTimeout` and `DecoderPriority`, and
`ImageDecoder` gained an optional context-aware `Decode`.

**6a. Kitty in the built-in terminal: shared memory, resize, alt screen.**
`t=s` resolves the name of a POSIX shared memory object inside `/dev/shm`,
reads it and unlinks it, as the protocol requires. `kittyResizePlacements`
moves the pictures of the main screen by the same shift the reflow gives the
text and drops what left the buffer for good; `kittyRecomputeSpans` works out
again the side of a span the client left to us, both after a resize and after
a cell changes size. Leaving the alternate screen now drops its pictures.

**R1. Picking and walking in the single picture view.** Asked for on the
issue: `Ins` and `Del` pick and unpick the picture on screen and move on to
the next, exactly as they do in the grid, and the window title marks a picked
picture with an asterisk. The regular (enhanced) arrows walk the directory;
the numpad arrows pan; `w`, `a`, `s` and `d` pan whatever happens. `j`/`k`
walk too, and `~` flips between two neighbouring pictures for comparison.

Done after that, on top of the sequence above: the `~` compare toggle that
carries zoom and pan between the two sides; zoom and pan following the reader
from one picture to the next (as a share of the pan range); `j`/`k` walking;
the numpad-vs-arrow split; a double click for the 1:1 fit; the viewer's own
title bar removed in favour of a short window title; the gallery starting at
the top row; in-flight tile decodes cancelled when the viewer closes, the grid
closes or a slide show starts; the three pipeline caches merged into one
generic LRU; and the WIC progress callback lifetime fix.

**The plain insurance tier.** `BlockPlain` (`[Images]`) stamps every cell
as a space with one colour — the linear average of the two halves — instead
of the `▀` half-block with a fg/bg pair, so a font that lacks the block
glyph cannot break the picture (it still degrades to 256/16 colours through
vtui's writer). It is also automatic on 16-colour consoles, whose fonts
cannot be trusted with the glyph. `Shift+F4` cycles the renderers on every
terminal: with the graphics protocol it is graphics → half-block → plain,
and without one it flips the two block variants, so the glyph-free picture
is one keypress away anywhere. The renderer's memo counts the plain flag,
so toggling it mid-view cannot serve stale cells.

## 5. What is left, in order

Before the numbered work, one defect that is on screen right now:

**B1. A strip of background below the picture, with something flickering in
it.** Under investigation. `ImageView.logGeometry` writes one `IMAGE_GEOM`
line per change of layout under `VTUI_DEBUG=1`, and it has already ruled out
both of the causes that were suspected first. On a console of `153x38` with a
`20x41` cell the frame runs `0,0..152,36` with one row of title bar, and the
placement comes out as `place=45,1 62x36`: the picture covers rows one to
thirty six, which is the whole of its area, so the centring leaves nothing
over. Every line reports `layer=1`, so nothing besides the viewer is drawing
into the graphics layer.

The fault is therefore below the cell grid, in a native renderer, which agrees
with what was observed: it appears on X11 and not in kitty and not on gogpu.
One defect there is proved from the source and fixed in vtui — the X11 frame
buffer covered only the whole cells of the window, leaving `height mod cellH`
pixels, up to forty of them on a cell forty one high, that nothing ever wrote
to and that therefore kept whatever the X server last put in them. That
accounts for a strip *below* the key bar.

Still open: whether the strip that was reported is that one, or lies *between*
the picture and the key bar, which would instead mean that
`vtui.drawNativePlacements` paints a picture smaller than the rectangle the
placement gives it. The new `X11_GFX` line prints the rectangle drawn beside
the rectangle asked for and settles it.

**6b. Kitty polish, what is left.** Unicode placeholders (`U=1` and the
character `U+10EEEE`), and a negative `z`, which needs a change in vtui first:
see the entry in section 8.

**7. iTerm2 and sixel output in vtui.** Add `GraphicsITerm2` (OSC 1337, base64
PNG) and `GraphicsSixel` (DCS, up to 256 palette colours, dithering) to
`graphics.go`; detect them from `TERM_PROGRAM=iTerm.app` and from a `CSI c`
answer containing 4. Test the shape of the sequences, not the pixels.

**8. Accepting iTerm2 and sixel in the built-in terminal.** Symmetrical to the
kitty side: OSC 1337 in `handleOSC`, sixel DCS in the parser, both feeding the
same placement layer.

**9. Fixing the kitty receiver in far2l.** In `far2l/src/vt/vtansi_kitty.cpp`:

- use `GetInt` rather than `GetChar` for `i` and `p` in `a=p` and `a=d`;
- apply `c`, `r`, `X`, `Y` and `z` when placing;
- `d=i` removes the placement but keeps the pixels, `d=I` frees the data;
- `a=d` with no `d`, and `d=a` / `d=A`, remove every visible placement;
- in `AddImage`, `if (rows > 0) { img.cols = cols; }` tests the wrong field —
  it has to test `cols`, and the missing side has to be computed from the
  aspect ratio;
- in `KittyArgs`, replace `if (i + 1 > j && s[j + 1] == '=')` with a test of
  `j + 1 < i`;
- answer `a=q` according to what the backend can actually do, through
  `GetConsoleImageCaps`.

**10. far2l to f4 detection.** far2l sets something like `FAR2L_IMAGES=1` for
its child when `GetConsoleImageCaps` reports RGBA support; take it into account
in `detectGraphicsProtocol` in `vtui/terminal_env.go` so that f4 running inside
far2l turns kitty on.

**11. far2l's own protocol in f4's built-in terminal.** Accept
`FARTTY_INTERACT_IMAGE_*` (see `far2l/WinPort/FarTTY.h`) in `HandleFar2lAPC`,
so that far2l running inside f4 can hand pictures over through its own channel.

**12. Video.** A second source of frames on top of the same placement layer:
decode through an external `ffmpeg` into a stream of RGBA, a frame timer, and
controls from the viewer (`Right`/`Left` for ±10 seconds, `Up`/`Down` for
volume). A still frame from a video is done — the Windows shell decoder
renders the first frame from a real path — but playback, seeking and volume
are not.

Note that steps 9 to 11 exist because **kitty images do not work in either
direction between f4 and far2l today**: not when f4 runs inside far2l, and not
the other way round.

**13. Viewer features and keys that are still missing.** Not done yet; the
list is a wish-list rather than a schedule.

- Animated GIF and WebP: only the first frame is shown today.
- Video playback controls: seek, volume, a frame timer (see step 12).
- Mouse wheel zoom and drag-to-pan; today the mouse only toggles 1:1 on a
  double click.
- Fit-width and fit-height modes, in addition to fit and 1:1.
- EXIF metadata display beyond the thumbnail (shutter, ISO, lens).
- File operations from the viewer: delete, move, rename, copy the path.
- Rotate/save-as: persist an orientation instead of only viewing it.
- Settings-dialog entries for `SlideShowDelay`, `ExternalTimeout`,
  `DecoderPriority`, `BlockRenderer`, `BlockPlain`, `FullScreen` and
  `ShowOverlay` — they are config-file only today.
- Gallery tile size (18x9 cells) as a `[Images]` setting.
- A slide show that waits for a decode instead of skipping a still-loading
  frame.
- iTerm2 and sixel output and input (steps 7 and 8) and the far2l interop
  (steps 9 to 11).

## 6. Decisions worth not undoing

- **The orientation is reset in `open()`, not in `SetImage()`.** `SetImage` is
  also called when the full resolution decode replaces the thumbnail of the
  *same* file. Resetting there would snap a picture back moments after the
  reader turned it.
- **A mirror reverses the direction of a turn.** The state is "rotate by
  `rotation`, then mirror", and for a reflection `R₉₀∘F = F∘R₋₉₀`. So when
  exactly one axis is mirrored, `Rotate` negates the delta; with both axes the
  mirror is a half turn, which commutes, and the sign stands.
- **`HideBars` has to live in the frame manager.** `ScreenObject.Show` forces
  an object visible, so `SetVisible(false)` on the key bar does not survive the
  next frame. The manager hides it — rather than merely skipping the drawing —
  because an invisible bar that still reports itself visible keeps swallowing
  clicks on the bottom row in `dispatchEvent`.
- **The overlay uses a negative z index.** In kitty, a `z` between −1073741824
  and −1 puts the picture under the glyphs but still over the cell background,
  which is what lets the info panel be readable without a box hiding the
  picture. Below −1073741824 the picture would go under the background too.
- **Thumbnails are fetched off the drawing path.** `PreviewSync` is cheap on a
  cached picture but reads the file header on one it has not seen; a screenful
  of tiles would otherwise mean a screenful of reads on every frame.
- **The slide show wraps around, `Step` does not.** Stopping at the ends of the
  directory makes it obvious where the directory ends; a show that stopped
  there would only be a slow way of pressing space.
- **`Stat` for the overlay happens once, lazily, and only when the overlay is
  actually up.** It can be a network round trip on a remote file system.
- **The cell grid is not the window.** A native backend gets a window sized in
  pixels by somebody else, and the whole cells inside it do not reach its
  edges. Everything f4 computes is in cells and stops being the whole story at
  that boundary, which is why an `IMAGE_GEOM` line that looks perfect can sit
  above a defect on screen.
- **The external converter is fed a file, not a pipe.** `heic`, `avif` and
  `jxl` are containers that are read by seeking around them, and both
  ImageMagick's delegates and `ffmpeg` refuse a stream they cannot rewind.
  The bytes are in memory already, so a temporary file costs one write. Its
  name carries an extension guessed from the magic bytes, because ImageMagick
  picks a delegate by the name before it looks inside.
- **The list of formats depends on the converter that was found.** Claiming an
  extension is what makes the panel call a file a picture and the viewer open
  it, so a decoder that claimed `psd` on a machine with only `ffmpeg` would
  turn a hex dump into an error message.
- **`convert` is skipped on Windows.** There, `convert.exe` is the file system
  conversion utility that ships with the system. ImageMagick 7 answers to
  `magick` everywhere, so nothing is lost.
- **Decoder priorities live beside the registry, not in it.** A decoder
  registers with the priority its author chose and the overrides are applied
  when the registry is read, so emptying `DecoderPriority` restores the
  built-in order without a second copy of the built-in numbers.

- **A resize moves pictures but never rescales them.** A placement is a
  rectangle of cells and kitty keeps it that way; `kittyClipPlacement` already
  trims what does not fit at drawing time, so widening the window brings the
  whole picture back. Shrinking `Cols` instead would lose it for good.
- **`WantCols` and `WantRows` are kept beside the computed span.** A side the
  client gave in `c` or `r` is a promise about the layout of the screen and
  stands through everything; only the side we chose for it, and the clamp to
  the size of the screen, are worked out again.
- **A shared memory name is one path component.** `shm_open(3)` allows nothing
  else, and a name with a separator in it would turn `t=s` into a way of
  reading any file on the machine. It is refused before it reaches the file
  system rather than after.

- **The regular arrows walk, the numpad arrows pan.** The console flags the
  cluster arrows `EnhancedKey` and leaves the numpad ones plain, so the two
  can be told apart; `w`, `a`, `s` and `d` pan unconditionally. A reader who
  zooms a picture pans with the numpad or `wasd`, and never jumps to the next
  file by accident.
- **The window title marks a picked picture with an asterisk.** The asterisk
  survives a terminal whose colours nobody has set up; the title itself stays
  short (`name (WxH)`) so the workspace tab does not crowd.

## 7. Traps

**A span with one side given follows the shape of the cell, not its size.**
When a client gives `c` and leaves `r` to the terminal, the rows come out of
`srcH * cols * cellW / (srcW * cellH)`, and halving both sides of the cell
leaves that untouched. Only a change of the cell's aspect ratio moves it. A
test that took a cell from ten by twenty to five by ten and expected the
height to change was wrong about the arithmetic, not about the code — the
mistake was carrying an expectation over from the case where both sides are
computed, which does depend on the absolute size.

**`Ctrl+I` is Tab.** On a terminal without an extended keyboard protocol,
`Ctrl+I` arrives as `VK_TAB`, which the viewer uses for the 1:1 fit. Nothing in
f4 can tell them apart — the information is not in the event. Hence the plain
`I` alias. The same class of problem may affect `Alt+>` and `Alt+<`, which are
currently matched by `Char` with the Alt flag set; that has not been confirmed
on a real terminal.

## 8. Open questions and rough edges

None of these are bugs; they are places where a decision was made without
asking, and the user may want a different one.

- `Ctrl+R` goes through `open()` and therefore also resets the orientation.
  Arguably right for "read the file again", but it was not asked about.
- The gallery tile is 18 by 9 cells, chosen by eye for an 80x25 terminal. It
  could reasonably become a setting in `[Images]`.
- In the grid, the cursor colour wins over the picked colour, so a picked tile
  under the cursor only looks like the cursor. far2l marks the selection with a
  separate character; the grid could too.
- The slide show does not wait for a picture to finish decoding, so on a slow
  file system with a short interval frames will be skipped. A check on
  `iv.loading` in `slideStep` would fix it.
- `SlideShowDelay` is read and written but does not appear in the settings
  dialog.
- `vtui/framemanager_hidebars_test.go` calls `fm.renderPhase()` directly, which
  no test did before. If it turns out to be too heavy to call from a test,
  move the check one level down.
- `image_gallery_test.go` passes a nil context to `ImagePipe.LoadSync`, which
  the function handles but which no other test does.
- **A negative `z` cannot be honoured without changing vtui.** The attribute
  word in `vtui/colors.go` carries either an eight bit index or twenty four
  bits of RGB, and has no way of saying "whatever the terminal calls default".
  `Show` fills the viewer with an explicit dark background, so a picture the
  terminal is asked to keep under the glyphs ends up under an opaque fill and
  is never seen. Either the attribute needs a "default colour" state, or the
  graphics layer needs to tell the backend not to paint the cells a negative
  `z` placement covers.
- `ImageView.panMaxX` and `panMaxY` come from the last frame, so a carry
  taken before the first draw sees a zero range and lands at the origin. That
  only happens for a picture that has never been drawn, where there is nothing
  to carry.
- `t=s` works wherever shared memory objects appear in the file system, which
  is Linux and the BSDs. On macOS and Windows `kittyShmPath` reports that the
  system has none, and the client gets `EBADF`. Doing better would need cgo
  and `shm_open`, for a medium almost nothing uses.
- Leaving the alternate screen drops its pictures but does not tell the store
  that they are gone, so the images themselves wait for the `kittyMaxImages`
  eviction. The same was already true of the erase path.
- A decode is never cancelled mid-flight. The job's context is threaded to
  the loader and the result is dropped when the waiter's context expires, but
  the decoder keeps running to completion; the pipeline has no way to abort a
  job it has already handed to a decoder.
- Only the still picture is taken from a converter that could give more: an
  animated `webp` arrives as its first frame. Animation belongs with step 12.
- `ExternalTimeout` and `DecoderPriority` are read and written but do not
  appear in the settings dialog, same as `SlideShowDelay`.

## 9. Mip-mapping and tiled rendering at zoom and pan — analysis, not code

Asked about: whether precomputed mip levels and a tile-based display would
help when a large picture is zoomed or panned, so only part of it (or a very
small part) is on screen. This is a CPU-side question: the backends ship
rectangles of pixels to a terminal or a native renderer, they do not expose
OpenGL/DX-style texture sampling, so any answer has to be about the two
renderers f4 itself controls.

**The graphics backends already tile the display.** A kitty/sixel placement
carries a source rect (`SrcX/SrcY/SrcW/SrcH`) and the terminal scales that
rect into the cell grid. Deep zoom sends a placement whose source rect is the
visible region only — the terminal keeps the full picture under the same
`gfxKey` and scales the crop, so only the placement command is re-sent, not
the pixels. The decoded surface in memory is still the whole picture, but the
display cost does not grow with the image; it grows with the window. There is
nothing to tile on the display side for these backends.

**The block renderer is the only CPU filter, and it already bounds its work
two ways.** The half-block pass is a separable box filter in linear RGB that
averages the source crop into the cell rect. Two caches keep it from running
every frame: `memoHit` re-stamps the cells while the geometry is unchanged,
and `workDraw` resamples the visible region plus a margin once and pans cut
cached colours from that working copy. So a static view, and pans inside the
margin, never re-filter. What still costs is a geometry change on a very
large picture zoomed out: the horizontal pass walks the whole crop (a fully
fitted 12000x8000 picture reads ~96M pixels once, ~100-200 ms), and each zoom
step outside the margin re-runs it.

**Mip-maps would help only that one case.** A lazy 2x pyramid — build levels
only when a zoomed-out view needs them, pick the level whose density is a
few source pixels per output pixel, then box-filter a small region — would
bound the filter to ~4-16x the output size regardless of the source. The
costs are extra memory (~1.33x the pixels if all levels are kept; a lazy
build keeps only the levels actually used) and one downscale pass per level.
For the common photo sizes the current filter is fine, so the pyramid should
only kick in past a threshold (say a few megapixels of crop). A cheaper
alternative with the same effect is decimation: when the target density is
below one output pixel per handful of source pixels, stride the horizontal
pass by a factor of K so the box average still samples a few pixels per
output cell. Slightly lower quality than a true mip, no memory, trivial to
implement — the right first step if profiling shows the zoomed-out filter is
actually the bottleneck.

**Tiled decode is a format question, not a renderer one.** JPEG and PNG are
not randomly addressable: any visible region requires decoding the whole
frame (or a WIC scaler, which decodes whole at a reduced size). Only tiled
TIFF and, awkwardly, progressive JPEG support cropping during decode, and the
pipeline's `requestFull` already fetches the whole picture for deep zoom. The
display-side tiling the backends already do makes decode-side tiling
pointless for the common formats; it would only matter for genuinely huge
images (hundreds of megapixels) where decoding the whole frame into memory is
the real cost, and there the answer is a sized decode, not tiles.

**Bottom line.** Nothing to change for the graphics backends — they already
scale crops terminal-side and the working copy covers the block renderer's
pans. If a huge zoomed-out picture feels slow to re-fit in half-block mode,
add decimation (or a lazy mip) to the block filter; measure first, since the
filter already runs once per geometry change.