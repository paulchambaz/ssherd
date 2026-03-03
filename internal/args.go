package internal

import (
	"fmt"
	"os"
	"strings"
)

type Args struct {
	ConfigPath  string
	ShowHelp    bool
	ShowVersion bool
}

func ParseArgs() (*Args, error) {
	args := &Args{
		ConfigPath: "/etc/ssherd/ssherd.cfg",
	}

	if len(os.Args) <= 1 {
		return args, nil
	}

	for _, arg := range os.Args[1:] {
		if arg == "-h" || arg == "--help" {
			PrintUsage()
			os.Exit(0)
		}
	}

	osArgs := os.Args[1:]
	for i := 0; i < len(osArgs); i++ {
		arg := osArgs[i]
		switch arg {
		case "-v":
			args.ShowVersion = true
			return args, nil
		case "-c":
			if i+1 >= len(osArgs) {
				return nil, fmt.Errorf("error: -c requires a path argument")
			}
			nextArg := osArgs[i+1]
			if strings.HasPrefix(nextArg, "-") {
				return nil, fmt.Errorf("error: -c requires a path argument")
			}
			args.ConfigPath = nextArg
			i++
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, fmt.Errorf("error: unknown flag: %s", arg)
			}
		}
	}

	return args, nil
}

func PrintUsage() {
	fmt.Printf(`Usage: ssherd [OPTIONS]

TODO

Options:
  -c <path>   Path to config file (default: /etc/ssherd/ssherd.cfg)
  -v          Print version information
  -h          Print this help message

For bug reporting and more information, please see:
https://github.com/paulchambaz/ssherd
`)
}

