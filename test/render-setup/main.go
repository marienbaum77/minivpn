// render-setup prints the node setup script with throwaway values, so CI
// can run shellcheck on exactly what a node receives.
package main

import (
	"fmt"
	"os"

	"github.com/marienbaum77/minivpn/internal/node"
)

func main() {
	keys, err := node.NewKeys()
	if err != nil {
		panic(err)
	}
	s, err := node.Script(node.Params{
		Name: "minivpn-render", Keys: keys, Port: 443, SNI: "www.apple.com",
		APIBase: "https://api.serverspace.ru", APIKey: "placeholder", Watchdog: node.DefaultWatchdog(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(s)
}
