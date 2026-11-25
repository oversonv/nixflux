package main

import (
	"os"

	"nixflux"
)

func main() {
	os.Exit(nixflux.RunCLI(os.Args[1:]))
}
