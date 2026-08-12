// Command yodadb is the command-line interface for yodaDB.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"yodadb"
)

const version = "dev"

func main() {
	os.Exit(runWithInput(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithInput(args, strings.NewReader(""), stdout, stderr)
}

func runWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("yodadb", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: yodadb [options] [database]")
		fmt.Fprintln(flags.Output(), "")
		fmt.Fprintln(flags.Output(), "yodaDB is an embedded relational database under development.")
		fmt.Fprintln(flags.Output(), "")
		fmt.Fprintln(flags.Output(), "Options:")
		flags.PrintDefaults()
	}

	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "yodadb %s\n", version)
		return 0
	}
	if flags.NArg() > 0 {
		fmt.Fprintln(stderr, "database files are not implemented until M4; run without a filename for the M1 in-memory shell")
		return 2
	}
	return runShell(stdin, stdout, stderr)
}

func runShell(stdin io.Reader, stdout, stderr io.Writer) int {
	database := yodadb.New()
	defer database.Close()
	scanner := bufio.NewScanner(stdin)
	var statement strings.Builder
	for {
		if statement.Len() == 0 {
			fmt.Fprint(stdout, "yoda> ")
		} else {
			fmt.Fprint(stdout, "   ...> ")
		}
		if !scanner.Scan() {
			if strings.TrimSpace(statement.String()) != "" {
				executeShellStatement(database, statement.String(), stdout, stderr)
			}
			if err := scanner.Err(); err != nil {
				fmt.Fprintf(stderr, "input error: %v\n", err)
				return 1
			}
			return 0
		}
		line := scanner.Text()
		statement.WriteString(line)
		statement.WriteByte('\n')
		if !strings.HasSuffix(strings.TrimSpace(line), ";") {
			continue
		}
		executeShellStatement(database, statement.String(), stdout, stderr)
		statement.Reset()
	}
}

func executeShellStatement(database *yodadb.DB, sql string, stdout, stderr io.Writer) {
	outcome, err := database.Execute(context.Background(), sql)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return
	}
	if outcome.Rows == nil {
		if affected := outcome.Result.RowsAffected(); affected > 0 {
			fmt.Fprintf(stdout, "%d row(s) affected\n", affected)
		}
		return
	}
	defer outcome.Rows.Close()
	for outcome.Rows.Next() {
		values := outcome.Rows.Values()
		formatted := make([]string, len(values))
		for index, value := range values {
			if value == nil {
				formatted[index] = "NULL"
			} else {
				formatted[index] = fmt.Sprint(value)
			}
		}
		fmt.Fprintln(stdout, strings.Join(formatted, " | "))
	}
	if err := outcome.Rows.Err(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
	}
}
