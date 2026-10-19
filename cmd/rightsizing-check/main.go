// Command rightsizing-check finds Kubernetes requests that are far above the observed usage.
package main

import (
	"os"

	"github.com/robertasolimandonofreo/rightsizing-check/internal/app"
)

var version = "dev"

func main() {
	os.Exit(app.Run(os.Args[1:], app.Env{Stdout: os.Stdout, Stderr: os.Stderr, Version: version}))
}
