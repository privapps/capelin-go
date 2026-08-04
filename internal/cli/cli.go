// Package cli owns command-line process dispatch for capelin-go.
package cli

import (
	"capelin-go/internal/app"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// Main runs the command using the process arguments and executable name.
// The command packages use this as their only application-facing operation.
func Main(version string) int {
	return Run(os.Args[1:], filepath.Base(os.Args[0]), version)
}

// Run parses args and dispatches the selected execution mode. It returns a
// process exit status rather than calling os.Exit, which keeps command
// entrypoints thin and makes dispatch behavior straightforward to test.
func Run(args []string, executable, version string) int {
	cfg, err := app.LoadConfig(args)
	if err != nil {
		if errors.Is(err, app.ErrHelpRequested) {
			app.PrintUsage(os.Stdout, executable)
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if cfg.ShowVersion() {
		fmt.Fprintf(os.Stdout, "%s %s\n", executable, version)
		return 0
	}
	if cfg.ServerPort() > 0 {
		if err := app.StartServer(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if !cfg.Interactive() && cfg.InitialQuestion() == "" {
		app.PrintUsage(os.Stderr, executable)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if cfg.Interactive() {
		if err := application.RunInteractive(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}

	if err := application.RunQuestion(ctx, cfg.InitialQuestion()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
