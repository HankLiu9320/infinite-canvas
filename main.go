package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tigerowo/infinite-canvas/config"
	"github.com/tigerowo/infinite-canvas/handler"
	"github.com/tigerowo/infinite-canvas/router"
	"github.com/tigerowo/infinite-canvas/service"
)

func main() {
	if err := config.Load(); err != nil {
		log.Fatal(err)
	}
	if err := service.EnsureDefaultAdmin(); err != nil {
		log.Fatal(err)
	}
	if err := service.EnsureDefaultAgentSkills(); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.StartCodexService(ctx); err != nil {
		log.Fatal(err)
	}
	defer service.CloseCodexService()
	service.StartPromptSyncScheduler()
	service.StartCanvasProjectCleanupScheduler()
	handler.StartVideoTaskPoller()
	server := &http.Server{Addr: ":" + config.Cfg.Port, Handler: router.New()}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Print(err)
		}
	}
	stop()
	service.CloseCodexService()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Print(err)
	}
}
