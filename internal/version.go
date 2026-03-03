package internal

import "fmt"

const version = "0.1.0"

func PrintVersion() {
	fmt.Printf("ssherd version %s\n", version)
}

