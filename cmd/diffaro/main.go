// Command diffaro is the process entry point. It forwards arguments to
// cli.Run and maps the hidden __helm-render argument to the bundled Helm
// worker subprocess (localhelm.Worker).
package main

import (
	"context"
	"diffaro/internal/cli"
	"diffaro/internal/localhelm"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "__helm-render" {
		os.Exit(localhelm.Worker(os.Stdin, os.Stdout))
	}
	// Cancel without a cause: signal.NotifyContext records the signal as the
	// cancellation cause, net/http then returns that cause instead of
	// context.Canceled, and interrupted requests were reported as connection
	// failures rather than cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		cancel()
	}()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
