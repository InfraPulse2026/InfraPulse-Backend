package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/pki"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "init-ca" {
		fmt.Fprintln(os.Stderr, "usage: pki init-ca --directory NEW_DIRECTORY --hosts localhost,127.0.0.1")
		os.Exit(2)
	}
	flags := flag.NewFlagSet("init-ca", flag.ExitOnError)
	directory := flags.String("directory", "", "new directory for CA/server credentials")
	hosts := flags.String("hosts", "localhost,127.0.0.1", "DNS names/IPs for HTTPS and gRPC")
	_ = flags.Parse(os.Args[2:])
	if *directory == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "--directory is required; no positional arguments")
		os.Exit(2)
	}
	path, err := filepath.Abs(*directory)
	if err == nil {
		err = pki.InitCA(path, strings.Split(*hosts, ","))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Created CA/server credentials in", path)
	fmt.Println("Keep ca.key and server.key private; do not add this directory to Git.")
}
