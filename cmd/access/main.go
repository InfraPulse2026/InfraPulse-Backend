// access creates the first local administrator using existing migrated tables.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/config"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"io"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 || args[0] != "init-admin" {
		return fmt.Errorf("usage: access init-admin --username oleg (--generate-password or --password-stdin)")
	}
	flags := flag.NewFlagSet("init-admin", flag.ContinueOnError)
	name := flags.String("username", "", "first administrator username")
	generate := flags.Bool("generate-password", false, "print a fresh random password once")
	stdin := flags.Bool("password-stdin", false, "read password from standard input; never a command-line argument")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *generate == *stdin {
		return fmt.Errorf("choose exactly one password input mode")
	}
	username, err := access.Username(*name)
	if err != nil {
		return err
	}
	password := ""
	if *generate {
		token, err := access.NewToken()
		if err != nil {
			return err
		}
		password = strings.TrimPrefix(token, "ip_sess_")
	} else {
		b, err := io.ReadAll(io.LimitReader(input, 258))
		if err != nil {
			return err
		}
		password = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	hash, err := access.HashPassword(password)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := storage.Open(ctx, cfg.DatabaseURL, cfg.AgentTimeout, cfg.MonitoringDefaults, cfg.TelephonyIntervalSeconds)
	if err != nil {
		return err
	}
	defer db.Close()
	// Startup must apply migration 008 first. Migrate also resets Agent sessions,
	// so this administrative command must never invoke it on a running backend.
	u, err := db.AccessCreateUser(ctx, access.Principal{}, username, "admin", hash, true, access.Meta{RequestID: "local-bootstrap", Peer: "local-cli"})
	if err != nil {
		return err
	}
	result := map[string]any{"user": u}
	if *generate {
		result["generated_password"] = password
	}
	return json.NewEncoder(output).Encode(result)
}
