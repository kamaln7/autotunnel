package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kamaln7/autotunnel"
	autotunnelconfig "github.com/kamaln7/autotunnel/internal/config"

	"github.com/gen2brain/beeep"
	"github.com/sirupsen/logrus"
	"github.com/skratchdot/open-golang/open"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/mac"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/options"
	macoptions "github.com/wailsapp/wails/v2/pkg/options/mac"
)

func main() {
	configPath := filepath.Join(os.Getenv("HOME"), ".config", "autotunnel", "config.yaml")
	logLevel := "info"
	c, err := autotunnelconfig.ReadConfig(configPath)
	if err != nil {
		notify("Error reading config", err.Error()+"; using default log level `info`")
	} else if c.LogLevel != "" {
		logLevel = c.LogLevel
	}

	ll := logrus.New()

	logPath := filepath.Join(filepath.Dir(configPath), "autotunnel.log")
	if _, err := os.Stat(logPath); err == nil {
		_ = os.Rename(logPath, logPath+".old")
	}

	logWriters := []io.Writer{os.Stderr}
	logfd, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0664)
	if err != nil {
		notify("Error opening log file", err.Error())
	} else {
		logWriters = append(logWriters, logfd)
		defer logfd.Close()
	}
	ll.SetOutput(io.MultiWriter(logWriters...))

	lv, err := logrus.ParseLevel(logLevel)
	if err != nil {
		notify("Error parsing log level", err.Error()+"; using default log level `info`")
		ll.WithError(err).Error("parsing log level")
	}
	ll.SetLevel(lv)
	ll.Info("starting autotunnel")

	wailsLogLevel := map[logrus.Level]logger.LogLevel{
		logrus.PanicLevel: logger.TRACE,
		// FatalLevel level. Logs and then calls `logger.Exit(1)`. It will exit even if the
		// logging level is set to Panic.
		logrus.FatalLevel: logger.TRACE,
		// ErrorLevel level. Logs. Used for errors that should definitely be noted.
		// Commonly used for hooks to send errors to an error tracking service.
		logrus.ErrorLevel: logger.ERROR,
		// WarnLevel level. Non-critical entries that deserve eyes.
		logrus.WarnLevel: logger.WARNING,
		// InfoLevel level. General operational entries about what's going on inside the
		// application.
		logrus.InfoLevel: logger.INFO,
		// DebugLevel level. Usually only enabled when debugging. Very verbose logging.
		logrus.DebugLevel: logger.DEBUG,
		// TraceLevel level. Designates finer-grained informational events than the Debug.
		logrus.TraceLevel: logger.TRACE,
	}[lv]

	app := newApp(ll, configPath, logPath)

	err = wails.Run(&options.App{
		Title:             "autotunnel",
		Width:             1080,
		Height:            700,
		MinWidth:          800,
		MinHeight:         600,
		StartHidden:       true,
		HideWindowOnClose: true,
		Mac: &macoptions.Options{
			WebviewIsTransparent:          true,
			WindowBackgroundIsTranslucent: true,
			TitleBar:                      macoptions.TitleBarHiddenInset(),
			Menu:                          menu.DefaultMacMenu(),
			ActivationPolicy:              macoptions.NSApplicationActivationPolicyAccessory,
		},
		LogLevel: wailsLogLevel,
		Startup:  app.startup,
	})
	if err != nil {
		ll.WithError(err).Error("running wails app")
	}
}

type app struct {
	ctx    context.Context
	cancel context.CancelFunc

	runtime *wails.Runtime

	at          *autotunnel.AutoTunnel
	atCtx       context.Context
	atCtxCancel context.CancelFunc
	atQuit      chan struct{}

	tray              *menu.TrayMenu
	refreshTicker     *time.Ticker
	startsAtLoginMenu *menu.MenuItem
	ll                *logrus.Logger
	quit              bool
	err               string
	lock              sync.RWMutex
	trayIsOpen        bool
	configPath        string
	logPath           string
}

func newApp(ll *logrus.Logger, configPath, logPath string) *app {
	a := &app{
		ll:         ll,
		atQuit:     make(chan struct{}),
		configPath: configPath,
		logPath:    logPath,
	}

	a.startsAtLoginMenu = &menu.MenuItem{
		Label:   "🚪 Start at Login",
		Type:    menu.CheckboxType,
		Checked: false,
		Click:   a.updateStartOnLogin,
	}
	startsAtLogin, err := mac.StartsAtLogin()
	if err != nil {
		ll.WithError(err).Error("checking start at login")
		a.startsAtLoginMenu.Label = "🚪 Start at Login"
		a.startsAtLoginMenu.Disabled = true
	} else {
		a.startsAtLoginMenu.Checked = startsAtLogin
	}

	return a
}

func (a *app) updateStartOnLogin(data *menu.CallbackData) {
	err := mac.StartAtLogin(data.MenuItem.Checked)
	if err != nil {
		a.startsAtLoginMenu.Label = "☹ Start at Login unavailable"
		a.startsAtLoginMenu.Disabled = true
	}
	a.refreshTray()
}

func (a *app) startup(runtime *wails.Runtime) {
	a.runtime = runtime

	a.ctx, a.cancel = context.WithCancel(context.Background())
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		a.ll.Info("got ^C, cleaning up and shutting down")
		a.cancel()
	}()

	go func() {
		<-a.ctx.Done()
		a.runtime.Quit()
		os.Exit(0)
	}()

	go a.start()
	a.tray = &menu.TrayMenu{
		Label:   "at",
		Menu:    &menu.Menu{},
		OnOpen:  a.onMenuWillOpen,
		OnClose: a.onMenuDidClose,
	}
	a.runtime.Menu.SetTrayMenu(a.tray)

	go func() {
		a.refreshTray()
		t := time.NewTicker(1500 * time.Millisecond)
		for {
			<-t.C
			a.refreshTray()
		}
	}()
}

func (a *app) onMenuWillOpen() {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.trayIsOpen = true
}

func (a *app) onMenuDidClose() {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.trayIsOpen = false
}

func (a *app) refreshTray() {
	a.ll.Debug("refresh tray")

	a.ll.Trace("grabbing read mutex")
	a.lock.RLock()
	defer a.lock.RUnlock()

	if a.trayIsOpen {
		a.ll.Trace("not refreshing while tray is open")
		return
	}

	a.ll.Trace("creating tray")
	a.tray.Menu = &menu.Menu{
		Items: []*menu.MenuItem{},
	}
	a.tray.Label = boolText(a.at == nil, "!at", "at")
	if a.at != nil {
		status := a.at.Status()
		a.ll.WithField("status", status).Debug("got status")
		for _, s := range status {
			var label string
			if s.Paused {
				label += "⏸"
			}
			label += boolText(s.Connected, "✅", "💤")
			label += " " + s.Name
			i := &menu.MenuItem{
				Type:  menu.SubmenuType,
				Label: label,
				SubMenu: &menu.Menu{
					Items: []*menu.MenuItem{
						{
							Type:    menu.CheckboxType,
							Label:   boolText(s.Connected, "✅ Connected", "💤 Not Connected"),
							Checked: s.Connected,
						},
						{
							Type:  menu.TextType,
							Label: boolText(s.Paused, "⏸ Paused", "🆗 Not Paused"),
						},
						{
							Type:  menu.TextType,
							Label: fmt.Sprintf("🌏 Local Port: %s", s.LocalPort),
						},
						{
							Type:  menu.TextType,
							Label: fmt.Sprintf("📶 Active Connections: %d", s.ActiveConnections),
						},
					},
				},
			}

			if s.LastError != "" {
				i.SubMenu.Append(&menu.MenuItem{
					Type:  menu.TextType,
					Label: fmt.Sprintf("⚠️ Last Error: %s", s.LastError),
				})
			}

			a.tray.Menu.Append(i)
		}
		a.tray.Menu.Append(menu.Separator())
	}

	if a.err != "" {
		a.tray.Tooltip = "⚠️ Error: " + a.err
		a.tray.Menu.Append(&menu.MenuItem{
			Type:     menu.TextType,
			Label:    "⚠️ Error: " + a.err,
			Disabled: true,
		})
		a.tray.Menu.Append(menu.Separator())
	}

	a.tray.Menu.Append(&menu.MenuItem{
		Type:  menu.TextType,
		Label: "🔄 Reload config and restart",
		Click: func(d *menu.CallbackData) {
			notify("autotunnel", "Reloading and restarting...")
			a.restart()
		},
	})
	a.tray.Menu.Append(&menu.MenuItem{
		Type:  menu.TextType,
		Label: "📄 Open log file",
		Click: func(d *menu.CallbackData) {
			err := open.Start(a.logPath)
			if err != nil {
				notify("Error opening log file", err.Error())
			}
		},
	})
	a.tray.Menu.Append(a.startsAtLoginMenu)
	a.tray.Menu.Append(&menu.MenuItem{
		Type:  menu.TextType,
		Label: "👋 Quit autotunnel",
		Click: func(d *menu.CallbackData) {
			a.shutdown()
		},
	})
	a.runtime.Menu.SetTrayMenu(a.tray)
	a.ll.Trace("updated tray")
}

func (a *app) restart() {
	a.ll.Info("restarting")

	a.stop()
	a.start()
}

func (a *app) stop() {
	a.ll.Debug("stopping autotunnel")
	a.ll.Trace("grabbing lock")
	a.lock.Lock()
	defer func() {
		a.lock.Unlock()
		a.err = ""
	}()

	if a.atCtxCancel == nil {
		a.ll.Trace("no running instance")
		return
	}
	a.ll.Trace("stopping running instance")
	a.atCtxCancel()
	a.ll.Trace("waiting for quit signal")
	<-a.atQuit

	a.atCtx, a.atCtxCancel = nil, nil
	a.at = nil
	a.ll.Trace("stopped running instance")
}

func (a *app) start() {
	a.ll.Debug("starting autotunnel")
	a.ll.Trace("grabbing lock")
	a.lock.Lock()
	defer a.lock.Unlock()

	config, err := autotunnelconfig.ReadConfig(a.configPath)
	if err != nil {
		a.err = "reading config:" + err.Error()
		a.ll.WithError(err).Error("reading config")
		return
	}

	logLevel := "info"
	if config.LogLevel != "" {
		logLevel = config.LogLevel
	}
	lv, err := logrus.ParseLevel(logLevel)
	if err != nil {
		notify("Error parsing log level", err.Error()+"; using default log level `info`")
		a.ll.WithError(err).Error("parsing log level")
	}
	a.ll.SetLevel(lv)

	if len(config.Tunnels) == 0 {
		a.err = "no tunnels configured"
		a.ll.Error(a.err)
		return
	}

	a.atCtx, a.atCtxCancel = context.WithCancel(a.ctx)
	at, err := autotunnel.New(a.atCtx, config, a.ll)
	if err != nil {
		notify("Error starting autotunnel", err.Error())
		a.err = "starting autotunnel: " + err.Error()
		a.ll.WithError(err).Error("starting autotunnel")
		go func() {
			a.atQuit <- struct{}{}
		}()
		return
	}

	a.at = at
	a.err = ""
	go func() {
		err := a.at.Start()
		if err != nil && !errors.Is(err, context.Canceled) {
			a.ll.Error(err.Error())
			notify("Error starting autotunnel", err.Error())
		}
		go func() {
			a.atQuit <- struct{}{}
		}()

		if a.quit {
			a.runtime.Quit()
			os.Exit(0)
		}
	}()
}

func (a *app) shutdown() {
	a.ll.Info("shutting down")
	a.cancel()
}

func boolText(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

func notify(title, message string) {
	title = strings.ReplaceAll(title, `"`, `\"`)
	message = strings.ReplaceAll(message, `"`, `\"`)
	_ = beeep.Notify(title, message, "")
}
