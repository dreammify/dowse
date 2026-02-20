// Package tui provides a terminal dashboard for live monitoring of LSP sessions,
// displaying session status, diagnostics counts, and resource usage.
package tui

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/dreammify/dowse/internal/daemon"
)

type keyAction struct {
	key         rune
	description string
	handler     func()
}

// App is the TUI dashboard application.
type App struct {
	app    *tview.Application
	client *Client

	table  *tview.Table
	header *tview.TextView
	flash  *tview.TextView
	footer *tview.TextView

	keyActions []keyAction
	flashCh    chan string
	stopCh     chan struct{}
	refreshCh  chan struct{}

	socketPath string
	tracker    *processTracker
}

var tableColumns = []string{"ID", "Workspace", "LSP", "Status", "PID", "CPU%", "RSS", "Model", "Files", "Diags", "Idle"}

// Run starts the TUI dashboard and blocks until it exits.
func Run(ctx context.Context, socketPath string, pollInterval time.Duration) error {
	tuiApp := &App{
		app:        tview.NewApplication(),
		client:     NewClient(socketPath),
		flashCh:    make(chan string, 8),
		stopCh:     make(chan struct{}),
		refreshCh:  make(chan struct{}, 1),
		socketPath: socketPath,
		tracker:    newProcessTracker(),
	}

	tuiApp.setupLayout()
	tuiApp.setupKeyBindings()
	tuiApp.startPolling(ctx, pollInterval)
	tuiApp.startFlashConsumer()

	tuiApp.app.EnableMouse(true)

	if err := tuiApp.app.Run(); err != nil {
		return fmt.Errorf("tui run: %w", err)
	}
	close(tuiApp.stopCh)
	return nil
}

func (a *App) setupLayout() {
	a.header = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	a.header.SetBorderPadding(0, 0, 1, 1)

	a.table = tview.NewTable().
		SetFixed(1, 0).
		SetSelectable(true, false).
		SetBorders(false).
		SetSeparator(' ')
	a.table.SetBorderPadding(0, 0, 1, 1)

	// Set header row.
	for col, name := range tableColumns {
		cell := tview.NewTableCell(name).
			SetTextColor(tcell.ColorYellow).
			SetSelectable(false).
			SetExpansion(0)
		if name == "Workspace" {
			cell.SetExpansion(1)
		}
		a.table.SetCell(0, col, cell)
	}

	a.flash = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	a.flash.SetBorderPadding(0, 0, 1, 1)

	a.footer = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	a.footer.SetBorderPadding(0, 0, 1, 1)

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.header, 3, 0, false).
		AddItem(a.table, 0, 1, true).
		AddItem(a.flash, 1, 0, false).
		AddItem(a.footer, 1, 0, false)

	a.app.SetRoot(flex, true)
}

func (a *App) setupKeyBindings() {
	a.keyActions = []keyAction{
		{'q', "Quit", func() { a.app.Stop() }},
		{'k', "Kill", a.killSelected},
		{'r', "Refresh", func() {
			select {
			case a.refreshCh <- struct{}{}:
			default:
			}
		}},
		{'S', "Stop daemon", a.stopDaemon},
		{'s', "Start daemon", a.startDaemon},
	}

	// Build footer text.
	var hints []string
	for _, action := range a.keyActions {
		hints = append(hints, fmt.Sprintf("[yellow]%c[white]:%s", action.key, action.description))
	}
	a.footer.SetText(strings.Join(hints, "  "))

	a.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyRune {
			for _, action := range a.keyActions {
				if event.Rune() == action.key {
					action.handler()
					return nil
				}
			}
		}
		return event
	})
}

func (a *App) startPolling(ctx context.Context, interval time.Duration) {
	go func() {
		// Do an immediate fetch on startup.
		a.poll(ctx)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-a.stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.poll(ctx)
			case <-a.refreshCh:
				a.poll(ctx)
			}
		}
	}()
}

func (a *App) poll(ctx context.Context) {
	data, err := a.client.FetchDashboard(ctx)
	if err != nil {
		a.app.QueueUpdateDraw(func() {
			a.header.SetText("[red]Daemon not running[white] — press [yellow]s[white] to start")
			// Clear table data rows, keep header.
			rowCount := a.table.GetRowCount()
			for row := rowCount - 1; row >= 1; row-- {
				a.table.RemoveRow(row)
			}
		})
		return
	}

	// Collect metrics for daemon and each session PID using the persistent
	// tracker so Percent(0) can compute CPU deltas between polls.
	daemonMetrics := a.tracker.Collect(ctx, data.Daemon.PID)
	activePIDs := map[int]bool{data.Daemon.PID: true}
	sessionMetrics := make(map[int]ProcessMetrics)
	for _, session := range data.Sessions {
		if session.LSPPID > 0 {
			activePIDs[session.LSPPID] = true
			sessionMetrics[session.LSPPID] = a.tracker.Collect(ctx, session.LSPPID)
		}
	}
	a.tracker.Prune(activePIDs)

	a.app.QueueUpdateDraw(func() {
		a.updateHeader(data.Daemon, daemonMetrics)
		a.updateTable(data.Sessions, sessionMetrics)
	})
}

func (a *App) updateHeader(info daemon.DaemonInfo, metrics ProcessMetrics) {
	cpuStr := fmt.Sprintf("%.1f%%", metrics.CPUPercent)
	rssStr := formatBytes(metrics.RSS)
	a.header.SetText(fmt.Sprintf(
		"[yellow]Dowse Daemon[white]  PID:%d  Uptime:%s  Sessions:%d  CPU:%s  RSS:%s",
		info.PID, info.Uptime, info.Sessions, cpuStr, rssStr,
	))
}

func (a *App) updateTable(sessions []daemon.DashboardSession, metrics map[int]ProcessMetrics) {
	// Remove old data rows.
	rowCount := a.table.GetRowCount()
	for row := rowCount - 1; row >= 1; row-- {
		a.table.RemoveRow(row)
	}

	// Sort by ID for stable ordering.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	for i, session := range sessions {
		row := i + 1

		statusColor := "green"
		switch {
		case session.Status == "inactive":
			statusColor = "gray"
		case session.Status != "ready":
			statusColor = "yellow"
		}

		var cpuStr, rssStr string
		if m, ok := metrics[session.LSPPID]; ok && m.Alive {
			cpuStr = fmt.Sprintf("%.1f%%", m.CPUPercent)
			rssStr = formatBytes(m.RSS)
		}

		var pidStr string
		if session.LSPPID > 0 {
			pidStr = fmt.Sprintf("%d", session.LSPPID)
		}

		statusText := session.Status
		if session.Percent != nil {
			statusText = fmt.Sprintf("%s (%d%%)", session.Status, *session.Percent)
		}

		values := []string{
			fmt.Sprintf("%d", session.ID),
			session.Workspace,
			session.LSP,
			fmt.Sprintf("[%s]%s[white]", statusColor, statusText),
			pidStr,
			cpuStr,
			rssStr,
			session.DiagModel,
			fmt.Sprintf("%d", session.OpenFiles),
			fmt.Sprintf("%d", session.TotalDiags),
			session.IdleFor,
		}

		for col, value := range values {
			cell := tview.NewTableCell(value)
			if col == 0 {
				cell.SetReference(session.ID)
			}
			a.table.SetCell(row, col, cell)
		}
	}
}

func (a *App) killSelected() {
	row, _ := a.table.GetSelection()
	if row < 1 {
		return
	}
	cell := a.table.GetCell(row, 0)
	if cell == nil || cell.GetReference() == nil {
		return
	}
	sessionID, ok := cell.GetReference().(int)
	if !ok {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.client.KillSession(ctx, sessionID); err != nil {
			a.showFlash(fmt.Sprintf("[red]Kill failed: %v", err))
			return
		}
		a.showFlash(fmt.Sprintf("[green]Killed session %d", sessionID))
		// Trigger immediate refresh.
		select {
		case a.refreshCh <- struct{}{}:
		default:
		}
	}()
}

func (a *App) stopDaemon() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.client.Shutdown(ctx); err != nil {
			a.showFlash(fmt.Sprintf("[red]Stop failed: %v", err))
			return
		}
		a.showFlash("[yellow]Daemon stopped")
	}()
}

func (a *App) startDaemon() {
	go func() {
		exe, err := exec.LookPath("dowse")
		if err != nil {
			a.showFlash("[red]dowse not found in PATH")
			return
		}
		cmd := exec.Command(exe, "start")
		if output, err := cmd.CombinedOutput(); err != nil {
			a.showFlash(fmt.Sprintf("[red]Start failed: %s", output))
			return
		}
		a.showFlash("[green]Daemon started")
		// Trigger immediate refresh.
		select {
		case a.refreshCh <- struct{}{}:
		default:
		}
	}()
}

func (a *App) showFlash(msg string) {
	select {
	case a.flashCh <- msg:
	default:
	}
}

func (a *App) startFlashConsumer() {
	go func() {
		for {
			select {
			case <-a.stopCh:
				return
			case msg := <-a.flashCh:
				a.app.QueueUpdateDraw(func() {
					a.flash.SetText(msg)
				})
				time.Sleep(3 * time.Second)
				a.app.QueueUpdateDraw(func() {
					a.flash.SetText("")
				})
			}
		}
	}()
}

func formatBytes(bytes uint64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(bytes)/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}
