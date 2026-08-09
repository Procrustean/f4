package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/unxed/vtui"
)

func TestRenderConsoleTitle(t *testing.T) {
	initTitleCache()

	// Template-only collapse: double spaces in the template become one,
	// while the state keeps its own spacing verbatim.
	if got := renderConsoleTitle("f4 - %State", "a  b"); got != "f4 - a  b" {
		t.Errorf("state spacing was eaten: %q", got)
	}
	if got := renderConsoleTitle("%State  %State", "x"); got != "x x" {
		t.Errorf("sentinel pair: %q", got)
	}

	want := cachedVersion + "|" + cachedPlat + "|" + cachedHost + "|" + cachedUser + "|" + cachedAdmin
	if got := renderConsoleTitle("%Ver|%Platform|%Host|%User|%Admin", ""); got != want {
		t.Errorf("static placeholders: %q, want %q", got, want)
	}
}

func TestTitleTemplate(t *testing.T) {
	orig := AppConfig.ConsoleTitleTemplate
	defer func() { AppConfig.ConsoleTitleTemplate = orig }()

	// Viewer: file + image info first, build info after the dash.
	if got := titleTemplate(true); got != "%State - f4 %Ver %Platform %Admin" {
		t.Errorf("viewer template = %q", got)
	}

	// Other frames keep the configured template; blank falls back.
	AppConfig.ConsoleTitleTemplate = "custom %State"
	if got := titleTemplate(false); got != "custom %State" {
		t.Errorf("custom template = %q", got)
	}
	AppConfig.ConsoleTitleTemplate = ""
	if got := titleTemplate(false); got != "f4 - %State" {
		t.Errorf("fallback template = %q", got)
	}
}

func TestUpdateWindowTitle(t *testing.T) {
	// 1. Резервное копирование текущего состояния конфигурации
	origTemplate := AppConfig.ConsoleTitleTemplate
	defer func() {
		AppConfig.ConsoleTitleTemplate = origTemplate
	}()

	// 2. Инициализация кэша заголовков для детерминированности теста
	initTitleCache()

	// 3. Создание изолированного буфера экрана для перехвата вывода
	var out bytes.Buffer
	scr := vtui.NewScreenBuf()
	scr.AllocBuf(80, 25)
	scr.Writer = &out

	// See snapshotFrameManagerState for why this isn't a plain struct copy.
	defer snapshotFrameManagerState(t)()

	// Инициализируем чистый стек окон во фреймворке
	vtui.FrameManager.Init(scr)

	// Добавляем тестовый фрейм, чтобы заголовок экрана стал "Desktop"
	desktop := vtui.NewDesktop()
	vtui.FrameManager.Push(desktop)

	// A transient menu is the top frame, but it must not leak into the host
	// terminal tab title.
	menu := vtui.NewVMenu("Commands")
	vtui.FrameManager.Push(menu)

	tests := []struct {
		name     string
		template string
		want     string
	}{
		{
			name:     "Default Template",
			template: "f4 - %State",
			want:     "f4 - Desktop",
		},
		{
			name:     "Custom Template with User & Host",
			template: "[%User@%Host] %State",
			want:     "[" + cachedUser + "@" + cachedHost + "] Desktop",
		},
		{
			name:     "All Placeholders",
			template: "%State|%Ver|%Platform|%Backend|%Host|%User|%Admin",
			want:     "Desktop|" + cachedVersion + "|" + cachedPlat + "|" + getBackendName() + "|" + cachedHost + "|" + cachedUser + "|" + cachedAdmin,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out.Reset()
			AppConfig.ConsoleTitleTemplate = tt.template

			// Имитируем проход рендеринга
			UpdateWindowTitle(scr)

			// Выталкиваем накопленные ESC-последовательности в буфер
			scr.Flush()

			got := out.String()
			// Ожидаем корректную управляющую последовательность OSC 0
			expectedSequence := "\x1b]0;" + tt.want + "\x07"
			if !strings.Contains(got, expectedSequence) {
				t.Errorf("UpdateWindowTitle() output = %q, expected to contain %q", got, expectedSequence)
			}
		})
	}
}
