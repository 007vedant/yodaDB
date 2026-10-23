// Command yodadb is the command-line interface for yodaDB.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"yodadb"
	"yodadb/storage/btree"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

const version = "dev"

func main() {
	os.Exit(runWithInput(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithInput(args, strings.NewReader(""), stdout, stderr)
}

func runWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	command := newRootCommand(stdin, stdout, stderr)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	return 0
}

func newRootCommand(stdin io.Reader, stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{
		Use:           "yodadb",
		Short:         "Embedded relational database and diagnostic tools",
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if len(args) != 0 {
				return errors.New("database files are not implemented until M4; run without a filename for the in-memory shell")
			}
			if code := runShell(command.Context(), stdin, stdout, stderr); code != 0 {
				return errors.New("in-memory shell failed")
			}
			return nil
		},
	}
	command.SetIn(stdin)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetVersionTemplate("yodadb {{.Version}}\n")
	command.CompletionOptions.DisableDefaultCmd = true
	command.AddCommand(newInspectCommand(stdout))
	command.AddCommand(newPagesCommand(stdout))
	return command
}

func newInspectCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect DATABASE",
		Short: "Display validated database and page headers",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			database, err := manifest.Open(command.Context(), args[0], manifest.Options{ReadOnly: true})
			if err != nil {
				return err
			}
			defer database.Close()
			metadata := database.Metadata()
			fmt.Fprintf(stdout, "file: %s\n", database.Path())
			fmt.Fprintf(stdout, "file ID: %s\n", metadata.FileID)
			fmt.Fprintf(stdout, "format version: %d\n", page.FormatVersion)
			fmt.Fprintf(stdout, "page size: %d\n", metadata.PageSize)
			fmt.Fprintf(stdout, "active metapage: %d\n", metadata.ActiveSlot)
			fmt.Fprintf(stdout, "generation: %d\n", metadata.Generation)
			fmt.Fprintf(stdout, "latest transaction: %d\n", metadata.LatestTxnID)
			fmt.Fprintf(stdout, "latest manifest page: %d\n", metadata.LatestManifestPageID)
			fmt.Fprintf(stdout, "page count: %d\n", database.PageCount())
			for id := page.ID(0); uint64(id) < database.PageCount(); id++ {
				decoded, err := database.ReadPage(id)
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "page %d: type=%s generation=%d payload=%d checksum=%016x\n",
					id, decoded.Header.Type, decoded.Header.Generation, len(decoded.Payload), decoded.Header.Checksum)
			}
			return nil
		},
	}
}

func newPagesCommand(stdout io.Writer) *cobra.Command {
	var treeRoot string
	command := &cobra.Command{
		Use:   "pages DATABASE --tree ROOT_PAGE",
		Short: "Display storage page structures",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if treeRoot == "" {
				return errors.New("--tree ROOT_PAGE is required")
			}
			root, err := strconv.ParseUint(treeRoot, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid tree root %q: %w", treeRoot, err)
			}
			database, err := manifest.Open(command.Context(), args[0], manifest.Options{ReadOnly: true})
			if err != nil {
				return err
			}
			defer database.Close()
			tree, err := btree.Open(database, page.ID(root))
			if err != nil {
				return err
			}
			return tree.WriteTree(command.Context(), stdout)
		},
	}
	command.Flags().StringVar(&treeRoot, "tree", "", "numeric B+ tree root page ID")
	return command
}

func runShell(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
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
				executeShellStatement(ctx, database, statement.String(), stdout, stderr)
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
		executeShellStatement(ctx, database, statement.String(), stdout, stderr)
		statement.Reset()
	}
}

func executeShellStatement(ctx context.Context, database *yodadb.DB, sql string, stdout, stderr io.Writer) {
	outcome, err := database.Execute(ctx, sql)
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
