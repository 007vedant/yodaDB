// Command yodadb is the command-line interface for yodaDB.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

const version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
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
		fmt.Fprintln(stderr, "database operations are not implemented until a later milestone")
		return 2
	}

	flags.Usage()
	return 0
}
