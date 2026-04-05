package main

import (
	"os"

	"github.com/personal/broxy/internal/app"
)

func main() {
	cmd := app.NewRootCommand()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
