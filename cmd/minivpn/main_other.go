//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "minivpn runs on Windows only: the client side is a tray app with a TUN interface.")
	os.Exit(1)
}
