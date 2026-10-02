// Command gen-docs writes upgradescope's shell completions and man pages,
// rendered from the same cobra command tree the binary runs, so they cannot
// drift from its flags. GoReleaser's before hook runs it; the release
// archives, deb/rpm/apk packages and the Homebrew formula ship the result:
//
//	<out>/completions/upgradescope.{bash,zsh,fish,ps1}
//	<out>/manpages/upgradescope[-<command>...].1.gz
//
// Unlike the other tools/ programs it is part of the main module: it
// imports internal/cli. It is a separate program rather than a hidden
// subcommand so the shipped binary does not link the man page renderer.
//
// The man page date is -date (RFC 3339; GoReleaser passes the commit date),
// else $SOURCE_DATE_EPOCH, else now. Output is byte-for-byte reproducible
// for a given date: cobra's auto-generated tag is off and the gzip headers
// carry no timestamp.
//
//	go run ./tools/gen-docs -out packaging/generated -date 2026-10-01T12:00:00Z
//
// With -reference it writes the docs site's generated references instead
// (reference.go): the CLI pages, the ClusterReadiness CRD and the REST API
// rendered from api/openapi.yaml. They carry no date, so the committed
// copies can be checked for drift (TestReferenceIsFresh, make docs-check):
//
//	go run ./tools/gen-docs -reference docs/reference
package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/abd-ulbasit/upgradescope/internal/cli"
)

func main() {
	out := flag.String("out", "packaging/generated", "output directory (completions/ and manpages/ are replaced)")
	dateFlag := flag.String("date", "", "man page date, RFC 3339 (default: $SOURCE_DATE_EPOCH, else now)")
	reference := flag.String("reference", "", "write the docs site references (cli/, crd.md, api.md) into this directory instead")
	openapi := flag.String("openapi", "api/openapi.yaml", "OpenAPI document rendered into <reference>/api.md")
	flag.Parse()
	if *reference != "" {
		if err := genReference(*reference, *openapi); err != nil {
			fmt.Fprintln(os.Stderr, "gen-docs:", err)
			os.Exit(1)
		}
		return
	}
	when, err := parseDate(*dateFlag)
	if err == nil {
		err = generate(*out, when)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-docs:", err)
		os.Exit(1)
	}
}

func parseDate(s string) (time.Time, error) {
	if s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, fmt.Errorf("-date %q: want RFC 3339 (2026-10-01T12:00:00Z): %w", s, err)
		}
		return t, nil
	}
	if epoch := os.Getenv("SOURCE_DATE_EPOCH"); epoch != "" {
		sec, err := strconv.ParseInt(epoch, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH %q: want Unix seconds: %w", epoch, err)
		}
		return time.Unix(sec, 0).UTC(), nil
	}
	return time.Now().UTC(), nil
}

// generate replaces <out>/completions and <out>/manpages.
func generate(out string, when time.Time) error {
	completions := filepath.Join(out, "completions")
	manpages := filepath.Join(out, "manpages")
	for _, d := range []string{completions, manpages} {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	root := cli.Root()
	// cobra adds `completion` only on Execute; add it now so its pages and
	// completions exist too.
	root.InitDefaultCompletionCmd()
	root.DisableAutoGenTag = true

	for name, gen := range map[string]func(io.Writer) error{
		"upgradescope.bash": func(w io.Writer) error { return root.GenBashCompletionV2(w, true) },
		"upgradescope.zsh":  root.GenZshCompletion,
		"upgradescope.fish": func(w io.Writer) error { return root.GenFishCompletion(w, true) },
		"upgradescope.ps1":  root.GenPowerShellCompletionWithDesc,
	} {
		var buf bytes.Buffer
		if err := gen(&buf); err != nil {
			return fmt.Errorf("completion %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(completions, name), buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return genMan(root, manpages, when)
}

// genMan writes one gzipped page per command (cobra skips help and hidden
// commands), named as doc.GenManTree names them.
func genMan(cmd *cobra.Command, dir string, when time.Time) error {
	for _, c := range cmd.Commands() {
		if !c.IsAvailableCommand() || c.IsAdditionalHelpTopicCommand() {
			continue
		}
		if err := genMan(c, dir, when); err != nil {
			return err
		}
	}
	header := &doc.GenManHeader{
		Title:   "",
		Section: "1",
		Source:  "upgradescope",
		Manual:  "upgradescope manual",
		Date:    &when,
	}
	var page bytes.Buffer
	if err := doc.GenMan(cmd, header, &page); err != nil {
		return fmt.Errorf("man page for %q: %w", cmd.CommandPath(), err)
	}
	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := zw.Write(page.Bytes()); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	name := filepath.Join(dir, manBase(cmd)+".1.gz")
	return os.WriteFile(name, gz.Bytes(), 0o644)
}

// manBase is "upgradescope-tokens-create" for `upgradescope tokens create`,
// the name the SEE ALSO sections of the other pages refer to.
func manBase(cmd *cobra.Command) string {
	if !cmd.HasParent() {
		return cmd.Name()
	}
	return manBase(cmd.Parent()) + "-" + cmd.Name()
}
