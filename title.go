package main

import (
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"unicode"

	"github.com/unxed/vtui"
)

var (
	titleOnce     sync.Once
	cachedHost    string
	cachedUser    string
	cachedAdmin   string
	cachedVersion string
	cachedPlat    string
	// static replacer only; %State and %Backend are per-call in renderConsoleTitle.
	staticTitleReplacer *strings.Replacer
)

func initTitleCache() {
	h, _ := os.Hostname()
	cachedHost = h

	u, err := user.Current()
	if err == nil && u != nil {
		cachedUser = u.Username
		if idx := strings.LastIndex(cachedUser, "\\"); idx != -1 {
			cachedUser = cachedUser[idx+1:]
		}
	} else {
		cachedUser = "user"
	}

	cachedAdmin = getAdminString()
	cachedVersion = getShortVersionInfo()
	cachedPlat = runtime.GOARCH

	staticTitleReplacer = strings.NewReplacer(
		"%Ver", cachedVersion, "%Platform", cachedPlat,
		"%Host", cachedHost, "%User", cachedUser, "%Admin", cachedAdmin)
}

func isReleaseVersion(v string) bool {
	if !strings.HasPrefix(v, "v") {
		return false
	}
	s := v[1:]
	for _, r := range s {
		if !unicode.IsDigit(r) && r != '.' {
			return false
		}
	}
	return true
}

func getGitTag() string {
	out, err := exec.Command("git", "describe", "--tags", "--abbrev=0").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func getGitFallback() (rev string, dirty string, timeStr string) {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", "", ""
	}
	rev = strings.TrimSpace(string(out))
	if rev == "" {
		return "", "", ""
	}
	statusOut, err := exec.Command("git", "status", "--porcelain").Output()
	if err == nil && len(strings.TrimSpace(string(statusOut))) > 0 {
		dirty = "-dirty"
	}
	timeOut, err := exec.Command("git", "log", "-1", "--format=%cI").Output()
	if err == nil {
		tStr := strings.TrimSpace(string(timeOut))
		if len(tStr) >= 16 {
			timeStr = strings.Replace(tStr[:16], "T", " ", 1)
		}
	}
	return rev, dirty, timeStr
}

func getVCSInfo() (rev string, dirty string, timeStr string) {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
				if len(rev) > 7 {
					rev = rev[:7]
				}
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "-dirty"
				}
			case "vcs.time":
				timeStr = s.Value
				if len(timeStr) >= 16 {
					timeStr = strings.Replace(timeStr[:16], "T", " ", 1)
				}
			}
		}
	}
	if rev == "" {
		rev, dirty, timeStr = getGitFallback()
	}
	return rev, dirty, timeStr
}

func getShortVersionInfo() string {
	baseVer := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		baseVer = info.Main.Version
	}
	if baseVer == "" || baseVer == "(devel)" {
		baseVer = getGitTag()
	}
	if baseVer == "" {
		baseVer = "(devel)"
	}

	rev, dirty, _ := getVCSInfo()
	if isReleaseVersion(baseVer) {
		return baseVer + dirty
	}
	if rev != "" {
		return rev + dirty
	}
	return baseVer
}

func getLongVersionInfo() string {
	baseVer := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		baseVer = info.Main.Version
	}
	if baseVer == "" || baseVer == "(devel)" {
		baseVer = getGitTag()
	}
	if baseVer == "" {
		baseVer = "(devel)"
	}

	rev, dirty, timeStr := getVCSInfo()
	var sb strings.Builder
	if isReleaseVersion(baseVer) {
		sb.WriteString(baseVer + dirty)
	} else if rev != "" {
		sb.WriteString(rev + dirty)
	} else {
		sb.WriteString(baseVer)
	}
	if timeStr != "" {
		sb.WriteString(" [" + timeStr + "]")
	}
	return sb.String()
}

func UpdateWindowTitle(scr *vtui.ScreenBuf) {
	titleOnce.Do(initTitleCache)

	if vtui.FrameManager == nil {
		return
	}

	state := "Panels"
	viewer := false
	if len(vtui.FrameManager.Screens) > 0 {
		active := vtui.FrameManager.Screens[vtui.FrameManager.ActiveIdx]
		state = stableWorkspaceTitle(active)
		if n := len(active.Frames); n > 0 {
			_, viewer = active.Frames[n-1].(*ImageView)
		}
	}

	vtui.SetWindowTitle(renderConsoleTitle(titleTemplate(viewer), state))

	// Macro recording indicator — drawn after MenuBar so it's always on top
	if MacroMgr != nil && MacroMgr.Recording {
		scr.Write(0, 0, vtui.StringToCharInfo(" R ", vtui.SetRGBBoth(0, 0xFFFFFF, 0xFF0000)))
	}
}

func stableWorkspaceTitle(screen *vtui.AppScreen) string {
	// Keep compatibility while the corresponding VTUI API is being reviewed.
	// Once available, the structural assertion starts using it automatically.
	if provider, ok := any(screen).(interface{ GetWorkspaceTitle() string }); ok {
		return provider.GetWorkspaceTitle()
	}
	for i := len(screen.Frames) - 1; i >= 0; i-- {
		if screen.Frames[i].IsModal() {
			continue
		}
		if title := strings.TrimSpace(screen.Frames[i].GetTitle()); title != "" {
			return title
		}
	}
	return screen.GetTitle()
}

// titleTemplate: while the viewer is up, the file name leads — taskbar and
// tabs truncate titles from the end, so the f4 build info goes after "-".
func titleTemplate(viewerActive bool) string {
	if viewerActive {
		return "%State - f4 %Ver %Platform %Admin"
	}
	if t := AppConfig.ConsoleTitleTemplate; t != "" {
		return t
	}
	return "f4 - %State"
}

// renderConsoleTitle: %State is parked on NUL so the double-space collapse
// (meant for templates) cannot eat the state's own spacing.
func renderConsoleTitle(template, state string) string {
	title := strings.ReplaceAll(template, "%State", "\x00")
	title = staticTitleReplacer.Replace(title)
	title = strings.ReplaceAll(title, "%Backend", getBackendName())
	title = strings.ReplaceAll(title, "  ", " ")
	return strings.ReplaceAll(title, "\x00", state)
}

func getBackendName() string {
	if vtui.FrameManager == nil {
		return "Console"
	}
	return vtui.FrameManager.GetBackendName()
}
