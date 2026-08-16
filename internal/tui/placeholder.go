package tui

import tea "github.com/charmbracelet/bubbletea"

// Placeholder stands in for a real screen until its ticket lands. It satisfies
// Screen so the shell (and its tests) behave identically to the finished UI.
type Placeholder struct {
	Name   string
	Ticket string // e.g. "#5"
}

// Tab returns the label shown in the tab row.
func (p Placeholder) Tab() string { return p.Name }

// Init is a no-op; the placeholder holds no state.
func (p Placeholder) Init() tea.Cmd { return nil }

// Update is a no-op; the placeholder holds no state.
func (p Placeholder) Update(msg tea.Msg) (tea.Model, tea.Cmd) { return p, nil }

// View renders the placeholder body and the shared key hints.
func (p Placeholder) View() string {
	return titleStyle.Render(p.Name) + "\n\n" +
		hintStyle.Render("Not implemented yet — lands in ticket "+p.Ticket+".") + "\n\n" +
		"Keys:  [ prev · ] next (or arrows) · 1–8 jump · / search · q quit"
}
