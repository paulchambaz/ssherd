package main

import (
	"log"
	"os"

	"github.com/paulchambaz/ssherd/daemon"
	"github.com/paulchambaz/ssherd/internal"
)

func main() {
	args, err := internal.ParseArgs()
	if err != nil {
		log.Fatal(err)
	}

	if args.ShowHelp {
		internal.PrintUsage()
		os.Exit(0)
	}

	if args.ShowVersion {
		internal.PrintVersion()
		os.Exit(0)
	}

	config, err := internal.LoadConfig(args.ConfigPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	srv, err := daemon.NewServer(&config)
	if err != nil {
		log.Printf("Could not create server %s", err)
	}
	srv.Run()
}
