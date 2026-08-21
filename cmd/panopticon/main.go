package main

import (
	"os"

	panopticon "panopticon/internal/panopticon"
)

func main() {
	os.Exit(panopticon.Main(os.Args[1:]))
}
