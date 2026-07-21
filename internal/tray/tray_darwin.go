//go:build darwin

package tray

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os/exec"
	"time"

	"github.com/getlantern/systray"
	"github.com/sjzsdu/free-router/internal/service"
)

// Run starts the native macOS menu bar UI and blocks until it exits.
func Run(ctx context.Context, manager *service.Manager, consoleURL, version string) error {
	var statusItem, openItem, restartItem, quitItem *systray.MenuItem
	ready := make(chan struct{})

	onReady := func() {
		icon := statusIcon()
		systray.SetTemplateIcon(icon, icon)
		systray.SetTooltip("Free Router")

		statusItem = systray.AddMenuItem("Checking service…", "Current free-router daemon status")
		statusItem.Disable()
		systray.AddSeparator()
		openItem = systray.AddMenuItem("Open Console", consoleURL)
		aboutItem := systray.AddMenuItem("About Free Router", "Version and endpoint")
		versionItem := aboutItem.AddSubMenuItem("Version "+version, "")
		versionItem.Disable()
		endpointItem := aboutItem.AddSubMenuItem(consoleURL, "")
		endpointItem.Disable()
		systray.AddSeparator()
		restartItem = systray.AddMenuItem("Restart Service", "Restart the free-router daemon")
		systray.AddSeparator()
		quitItem = systray.AddMenuItem("Quit Menu Bar", "The routing daemon keeps running")
		close(ready)
	}

	go func() {
		select {
		case <-ctx.Done():
			systray.Quit()
		case <-ready:
			runMenu(ctx, manager, consoleURL, statusItem, openItem, restartItem, quitItem)
		}
	}()

	systray.Run(onReady, func() {})
	return nil
}

func runMenu(ctx context.Context, manager *service.Manager, consoleURL string, statusItem, openItem, restartItem, quitItem *systray.MenuItem) {
	refresh := func() {
		statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		status, err := manager.Status(statusCtx)
		if err != nil {
			statusItem.SetTitle("⚠ Service Status Unavailable")
			return
		}
		if status.Running {
			statusItem.SetTitle(fmt.Sprintf("● Service Running · PID %d", status.PID))
		} else {
			statusItem.SetTitle("○ Service Stopped")
		}
	}

	refresh()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			systray.Quit()
			return
		case <-ticker.C:
			refresh()
		case <-openItem.ClickedCh:
			_ = exec.Command("/usr/bin/open", consoleURL).Start()
		case <-restartItem.ClickedCh:
			restartItem.Disable()
			statusItem.SetTitle("↻ Restarting Service…")
			restartCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := manager.Restart(restartCtx)
			cancel()
			if err != nil {
				statusItem.SetTitle("⚠ Restart Failed")
			} else {
				refresh()
			}
			restartItem.Enable()
		case <-quitItem.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func statusIcon() []byte {
	const size = 36
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	ink := color.NRGBA{A: 255}
	line(img, 7, 18, 17, 18, ink, 3)
	line(img, 17, 18, 26, 8, ink, 3)
	line(img, 17, 18, 28, 18, ink, 3)
	line(img, 17, 18, 26, 28, ink, 3)
	for _, point := range [][2]int{{6, 18}, {28, 7}, {30, 18}, {28, 29}} {
		circle(img, point[0], point[1], 4, ink)
	}
	var output bytes.Buffer
	_ = png.Encode(&output, img)
	return output.Bytes()
}

func line(img *image.NRGBA, x0, y0, x1, y1 int, ink color.NRGBA, width int) {
	dx, dy := abs(x1-x0), -abs(y1-y0)
	sx, sy := -1, -1
	if x0 < x1 {
		sx = 1
	}
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	for {
		circle(img, x0, y0, width/2, ink)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func circle(img *image.NRGBA, cx, cy, radius int, ink color.NRGBA) {
	for y := cy - radius; y <= cy+radius; y++ {
		for x := cx - radius; x <= cx+radius; x++ {
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= radius*radius && image.Pt(x, y).In(img.Bounds()) {
				img.SetNRGBA(x, y, ink)
			}
		}
	}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
