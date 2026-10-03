package main

import (
	"fmt"
	"io"
	"os"

	watcher "docker-watcher"
)

func main() {
	os.Exit(run(watcher.Start, os.Stderr))
}

func run(start func() error, stderr io.Writer) int {
	if err := start(); err != nil {
		fmt.Fprintln(stderr, watcher.ConfigurationError(err))
		return 1
	}
	return 0
}
