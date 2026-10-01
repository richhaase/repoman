package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/OWNER/REPO/internal/cli"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	v, c, d := getVersionInfo()
	return cli.Execute(ctx, v, c, d)
}

func getVersionInfo() (string, string, string) {
	if version != "dev" {
		return version, commit, date
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, commit, date
	}

	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if len(setting.Value) >= 7 {
				commit = setting.Value[:7]
			}
		case "vcs.time":
			date = setting.Value
		case "vcs.modified":
			if setting.Value == "true" {
				commit += "-dirty"
			}
		}
	}

	return version, commit, date
}
