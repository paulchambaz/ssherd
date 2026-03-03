SSHERD(1) "github.com/alias-asso/ssherd" "General Commands Manual"

# NAME

*ssherd* - TODO

# SYNOPSIS

*ssherd* [OPTIONS]

# DESCRIPTION

TODO

# OPTIONS

- *-v*         Print version information
- *-h*         Print this help message

# CONFIGURATION

*ssherd* uses a configuration file, its default location is
/etc/ssherd/ssherd.cfg. The configuration file uses TOML format.

## server

*host*
	Host on which the server will be hosted (default: 0.0.0.0)

*port*
	Port on which the server will be hosted (default: TODO)

# FILES

*/etc/ssherd/ssherd.cfg*
	Default configuration file location

# BUGS

Bug reports and feature requests should be submitted to:
https://github.com/paulchambaz/ssherd

# AUTHORS

Written by Paul Chambaz (2025).
Licensed under GPL-v3.
