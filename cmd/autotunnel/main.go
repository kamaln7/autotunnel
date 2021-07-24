package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kamaln7/autotunnel"
	autotunnelconfig "github.com/kamaln7/autotunnel/internal/config"
	"github.com/sirupsen/logrus"
)

func main() {
	logLevel := flag.String("log-level", "info", "log verbosity")
	configPath := flag.String("config", filepath.Join(os.Getenv("HOME"), ".config", "autotunnel", "config.yaml"), "path to config file")
	flag.Parse()

	ll := logrus.New()
	lv, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		ll.WithError(err).Fatal("parsing log level")
	}
	ll.SetLevel(lv)
	ll.Info("starting autotunnel")

	config, err := autotunnelconfig.ReadConfig(*configPath)
	if err != nil {
		ll.WithError(err).Fatal("reading config")
	}

	if len(config.Tunnels) == 0 {
		ll.Fatal("no tunnels configured")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		ll.Info("got ^C, cleaning up and shutting down")
		cancel()

		time.Sleep(3 * time.Second)
		ll.Warn("couldn't exit cleanly within 3s")
		os.Exit(1)
	}()

	at, err := autotunnel.New(ctx, config, ll)
	if err != nil {
		ll.WithError(err).Fatal("starting autotunnel")
	}

	err = at.Start()
	if err != nil && !errors.Is(err, context.Canceled) {
		ll.Fatal(err)
	}
	ll.Info("goodbye")
}
