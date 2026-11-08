//go:build !identitytest

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "baby-tracker-load needs -tags identitytest")
	os.Exit(2)
}
