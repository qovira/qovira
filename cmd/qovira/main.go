package main

import (
	"os"

	"github.com/qovira/qovira/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
