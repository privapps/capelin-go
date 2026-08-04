package main

import (
	"capelin-go/internal/cli"
	"os"
)

// version is set by the release build's -ldflags value. Keep this variable in
// the compatibility entrypoint so `go build .` retains the historical path.
var version = "dev"

func main() {
	os.Exit(cli.Main(version))
}
