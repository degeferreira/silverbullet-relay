package main

import (
	"context"
	"errors"
	"github.com/degeferreira/silverbullet-relay/go/hub"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func run() error {
	settings := env("HUB_SETTINGS_DIR", "/state/configuration")
	config, e := hub.LoadConfig(env("HUB_CONFIG", "/config/integrations.json"), settings)
	if e != nil {
		return e
	}
	state, e := hub.OpenState(env("HUB_STATE", "/state/state.db"))
	if e != nil {
		return e
	}
	defer state.Close()
	s, e := hub.NewService(config, state, nil)
	if e != nil {
		return e
	}
	defer s.Client.CloseIdleConnections()
	if e = s.WriteEffective(); e != nil {
		return e
	}
	host := hub.NewHost(s)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() {
		if os.Getenv("HUB_DISABLE_WORKERS") == "1" {
			<-ctx.Done()
			done <- ctx.Err()
			return
		}
		done <- host.Worker(ctx)
	}()
	server := &http.Server{Addr: env("HUB_LISTEN", "127.0.0.1:8088"), Handler: host.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 180 * time.Second, IdleTimeout: 60 * time.Second}
	serve := make(chan error, 1)
	go func() { serve <- server.ListenAndServe() }()
	select {
	case e = <-serve:
		stop()
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	stop()
	<-done
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
func main() {
	if e := run(); e != nil {
		log.Printf("Relay startup failed (%T)", e)
		os.Exit(1)
	}
}
