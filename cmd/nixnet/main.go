package main

import (
	"os"

	"github.com/oversonv/nixnet"
)

func main() {
	os.Exit(nixnet.RunCLI(os.Args[1:]))
}
