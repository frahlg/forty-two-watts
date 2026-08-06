package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/localauth"
	"github.com/srcfl/ftw/go/internal/state"
)

// runUserCLI implements `ftw user <add|list|disable|enable|delete|passwd>`,
// the bootstrap path for api.auth.mode: the first operator account must
// exist before login-required modes are usable, and a CLI on the box is
// the one channel that needs no prior credential.
func runUserCLI(args []string) {
	fs := flag.NewFlagSet("user", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "Path to config.yaml")
	role := fs.String("role", "operator", "Role for `add`: operator | viewer")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: ftw user [flags] <add|list|disable|enable|delete|passwd> [username]

Local API accounts (api.auth.mode). Password is prompted on stdin.`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	verb := rest[0]

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatalf("load config: %v", err)
	}
	statePath := "state.db"
	if cfg.State != nil && cfg.State.Path != "" {
		statePath = cfg.State.Path
	}
	st, err := state.Open(statePath)
	if err != nil {
		fatalf("open state: %v", err)
	}
	defer st.Close()

	name := ""
	if len(rest) > 1 {
		name = rest[1]
	}
	switch verb {
	case "list":
		users, err := st.ListUsers()
		if err != nil {
			fatalf("list: %v", err)
		}
		if len(users) == 0 {
			fmt.Println("no users — create one with: ftw user add <name>")
			return
		}
		for _, u := range users {
			state := "enabled"
			if u.Disabled {
				state = "disabled"
			}
			fmt.Printf("%-20s %-9s %s\n", u.Username, u.Role, state)
		}
	case "add":
		requireName(name)
		if !localauth.ValidRole(*role) {
			fatalf("role must be operator or viewer")
		}
		hash := promptPasswordHash()
		if err := st.CreateUser(state.User{Username: name, Role: *role, PasswordHash: hash}); err != nil {
			fatalf("add: %v", err)
		}
		fmt.Printf("user %q added (%s)\n", name, *role)
	case "passwd":
		requireName(name)
		hash := promptPasswordHash()
		if err := st.UpdateUserPassword(name, hash); err != nil {
			fatalf("passwd: %v", err)
		}
		fmt.Printf("password updated for %q (existing sessions end on restart)\n", name)
	case "disable", "enable", "delete":
		requireName(name)
		// Never remove the last enabled operator: with a login-required
		// mode configured that would lock the operator out of the box.
		if verb != "enable" {
			if u, ok, _ := st.UserByName(name); ok && u.Role == localauth.RoleOperator && !u.Disabled {
				if n, _ := st.CountOperators(); n <= 1 && cfg.API.AuthMode() != "open" {
					fatalf("refusing: %q is the last enabled operator and api.auth.mode is %q", name, cfg.API.AuthMode())
				}
			}
		}
		var err error
		switch verb {
		case "disable":
			err = st.SetUserDisabled(name, true)
		case "enable":
			err = st.SetUserDisabled(name, false)
		case "delete":
			err = st.DeleteUser(name)
		}
		if err != nil {
			fatalf("%s: %v", verb, err)
		}
		fmt.Printf("%s: %q\n", verb, name)
	default:
		fs.Usage()
		os.Exit(2)
	}
}

func requireName(name string) {
	if name == "" {
		fatalf("username required")
	}
}

func promptPasswordHash() string {
	fmt.Fprint(os.Stderr, "Password: ")
	var pw string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			fatalf("read password: %v", err)
		}
		pw = string(b)
	} else {
		// Piped stdin (scripts, tests): first line is the password.
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() {
			fatalf("read password: empty stdin")
		}
		pw = strings.TrimRight(sc.Text(), "\r\n")
	}
	hash, err := localauth.HashPassword(pw)
	if err != nil {
		fatalf("%v", err)
	}
	return hash
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
