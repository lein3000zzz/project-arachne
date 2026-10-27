// Command user manages accounts. Registration is closed, so this is where they
// come from; it runs inside the search container (make user, make token).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"web-crawler/internal/auth"
	"web-crawler/internal/platform"

	"go.uber.org/zap"
	"golang.org/x/term"
)

const usage = `usage: user <command> [flags] <username>

  create [-admin] <username>   new account; asks for the password
  password <username>          set a new password; ends the user's sessions
  disable <username>           block sign-in; ends the user's sessions
  enable <username>
  token [-name cli] <username> print a new API token (replaces one of that name)
  revoke -name <n> <username>  delete an API token
`

const timeout = time.Minute

func main() {
	if len(os.Args) < 2 {
		exit(2, usage)
	}

	command := os.Args[1]

	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	admin, name := new(bool), new(string)

	switch command {
	case "create":
		admin = flags.Bool("admin", false, "make an administrator")
	case "token":
		name = flags.String("name", "cli", "token name")
	case "revoke":
		name = flags.String("name", "", "token name")
	case "password", "disable", "enable":
	default:
		exit(2, usage)
	}

	if err := flags.Parse(os.Args[2:]); err != nil || flags.NArg() != 1 {
		exit(2, usage)
	}

	username := flags.Arg(0)

	var password string

	if command == "create" || command == "password" {
		var err error
		if password, err = readPassword(); err != nil {
			exit(1, "error: "+err.Error())
		}
	}

	platform.LoadEnv()

	logger := quietLogger()
	cfg := platform.LoadConfig(logger)
	sm := platform.NewSecretManager(logger)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pool := platform.OpenDatabase(ctx, logger, sm, 2)
	defer pool.Close()

	accounts := platform.NewAuthService(ctx, logger, cfg, pool)

	var err error

	switch command {
	case "create":
		var user auth.User
		if user, err = accounts.CreateUser(ctx, username, password, *admin); err == nil {
			fmt.Printf("created %s%s\n", user.Username, map[bool]string{true: " (admin)"}[user.Admin])
		}
	case "password":
		if err = accounts.SetPassword(ctx, username, password); err == nil {
			fmt.Printf("password of %s changed; their sessions ended\n", username)
		}
	case "disable", "enable":
		if err = accounts.SetDisabled(ctx, username, command == "disable"); err == nil {
			fmt.Printf("%s %sd\n", username, command)
		}
	case "token":
		var token string
		if token, err = accounts.CreateToken(ctx, username, *name); err == nil {
			fmt.Fprintf(os.Stderr, "new API token %q of %s; the old one of that name no longer works\n", *name, username)
			fmt.Println(token)
		}
	case "revoke":
		if err = accounts.RevokeToken(ctx, username, *name); err == nil {
			fmt.Printf("token %q of %s revoked\n", *name, username)
		}
	}

	if err != nil {
		exit(1, "error: "+err.Error())
	}
}

// readPassword asks twice on a terminal, without echo; piped, it takes the
// first line, so scripts can create accounts too.
func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())

	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return "", fmt.Errorf("reading the password from stdin: %w", err)
		}

		return strings.TrimRight(line, "\r\n"), nil
	}

	ask := func(prompt string) (string, error) {
		fmt.Fprint(os.Stderr, prompt)
		defer fmt.Fprintln(os.Stderr)

		b, err := term.ReadPassword(fd)

		return string(b), err
	}

	first, err := ask("Password (12+ characters): ")
	if err != nil {
		return "", err
	}

	second, err := ask("Again: ")
	if err != nil {
		return "", err
	}

	if first != second {
		return "", errors.New("the passwords differ")
	}

	return first, nil
}

// Warnings only: this is a command people read, not a service's log.
func quietLogger() *zap.SugaredLogger {
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(zap.WarnLevel)

	logger, err := cfg.Build()
	if err != nil {
		exit(1, "error: "+err.Error())
	}

	return logger.Sugar()
}

func exit(code int, message string) {
	fmt.Fprint(os.Stderr, strings.TrimRight(message, "\n")+"\n")
	os.Exit(code)
}
