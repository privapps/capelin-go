package main

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// TestRenderLogPanelBackground uses tcell's SimulationScreen to verify
// the round-3 + round-4 claim: in both light and dark themes the log
// panel area renders with the theme's LogBg, and tool-call text is
// rendered in a colour that is neither ColorDefault, ColorBlack, nor
// ColorWhite. This is the regression guard for the
// "log panel dark, tool text black" / "tool call invisible" complaints.
//
// Note: the buffer is built exactly the way appendLogEntry builds it
// (tui.go), including tview.Escape on the raw text so the literal
// "[tool]" prefix is preserved as text, not parsed as a color tag.
func TestRenderLogPanelBackground(t *testing.T) {
	for _, tc := range []struct {
		name  string
		theme tuiTheme
	}{
		{"light", newLightTheme()},
		{"dark", newDarkTheme()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sim := tcell.NewSimulationScreen("UTF-8")
			if err := sim.Init(); err != nil {
				t.Fatalf("sim.Init: %v", err)
			}
			defer sim.Fini()
			sim.SetSize(80, 24)

			tv := tview.NewTextView()
			tv.SetRect(10, 5, 60, 10)
			tv.SetDynamicColors(true)
			tv.SetBorder(true)
			tv.SetBackgroundColor(tc.theme.LogBg)
			tv.SetBorderColor(tc.theme.InactiveBorder)
			tv.SetTitleColor(tc.theme.TitleColor)
			tv.SetTextStyle(tcell.StyleDefault.
				Foreground(tcell.ColorNames[tc.theme.LogContent]).
				Background(tc.theme.LogBg).
				Bold(false))

			// Build the buffer the way appendToolLog (tui.go) builds it.
			bgName := colorName(tc.theme.LogBg)
			openTag := "[" + tc.theme.LogTool + ":" + bgName + ":]"
			closeTag := "[-:" + bgName + ":-]"
			rawText := "[tool] read_file(/etc/hostname)\n"
			rendered := tview.Escape(rawText)
			tv.SetText(openTag + rendered + closeTag)

			tv.Draw(sim)
			sim.Show()

			x, y, ow, oh := tv.GetRect()
			cells, screenW, _ := sim.GetContents()

			// 1) Majority of inner cells must be bg=LogBg (Box.Draw fill).
			bgSeen := 0
			total := 0
			for row := 1; row < oh-1; row++ {
				for col := 1; col < ow-1; col++ {
					cell := cells[(y+row)*screenW+(x+col)]
					_, bg, _ := cell.Style.Decompose()
					total++
					if bg == tc.theme.LogBg {
						bgSeen++
					}
				}
			}
			if total == 0 || bgSeen*2 < total {
				t.Errorf("expected majority of inner cells with bg=%v; got %d/%d", tc.theme.LogBg, bgSeen, total)
			}

			// 2) Tool text on the first content line must be rendered
			//    with a foreground that is a concrete colour (not
			//    ColorDefault, which is how tcell reports an unknown
			//    colour-name lookup — see round-4 bug) and visibly
			//    distinct from ColorBlack and ColorWhite.
			//
			// We check the first inner cell on the first content
			// row: that cell is where the open tag's fg takes
			// effect. Trailing cells beyond the rendered text
			// legitimately fall back to ColorDefault fg once the
			// close tag is processed, so we don't iterate the whole
			// row.
			firstCell := cells[(y+1)*screenW+(x+1)]
			fg, _, _ := firstCell.Style.Decompose()
			if fg == tcell.ColorDefault {
				t.Errorf("tool-call text foreground is tcell.ColorDefault — LogTool %q is likely not in tcell.ColorNames (round-4 bug)",
					tc.theme.LogTool)
			}
			if fg == tcell.ColorBlack || fg == tcell.ColorWhite {
				t.Errorf("expected tool-call text foreground to be visibly coloured (LogTool=%q); got %v", tc.theme.LogTool, fg)
			}
		})
	}
}
