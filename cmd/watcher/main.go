package main

import (
	"fmt"
	"io"
	"os"
	"time"

	watcher "docker-watcher"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck(os.Stderr))
	}
	os.Exit(run(watcher.Start, os.Stderr))
}

func runHealthcheck(stderr io.Writer) int {
	config, err := watcher.ConfigFromEnvironment()
	if err != nil {
		fmt.Fprintln(stderr, watcher.ConfigurationError(err))
		return 1
	}
	if watcher.HeartbeatIsFresh(config.HeartbeatFile, config.PollIntervalSeconds, config.BuildTimeoutSeconds, time.Now()) {
		return 0
	}
	return 1
}

func run(start func() error, stderr io.Writer) int {
	if err := start(); err != nil {
		fmt.Fprintln(stderr, watcher.ConfigurationError(err))
		return 1
	}
	return 0
}
