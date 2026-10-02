// Command minutiae is the Minutiae forensic acquisition CLI.
package main

import (
	"os"

	"github.com/rbenzing/minutiae/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.DefaultDeps()))
}
