// Command e2e runs the PageForge end-to-end scenario suite against either an
// in-process in-memory object store (default, zero dependencies) or a real
// S3-compatible store pointed to by environment variables.
//
// Usage:
//
//	# Fully self-contained smoke test (default):
//	go run ./cmd/e2e
//
//	# Print available scenarios:
//	go run ./cmd/e2e -list
//
//	# Run a single scenario:
//	go run ./cmd/e2e -only publish-and-rollback
//
//	# Against a real S3-compatible store:
//	PAGEFORGE_E2E_ENDPOINT=http://10.36.59.162:9090 \
//	PAGEFORGE_E2E_ACCESS_KEY=minioadmin \
//	PAGEFORGE_E2E_SECRET_KEY=minioadmin \
//	PAGEFORGE_E2E_BUCKET=page-bucket-1 \
//	go run ./cmd/e2e -env
//
// PAGEFORGE_E2E_ENDPOINT accepts a comma-separated list; the first host that
// answers with the supplied credentials is used. The bucket is created when
// it does not exist yet.
//
// The -only flag accepts a substring match against scenario names.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	only := flag.String("only", "", "run only scenarios whose name contains this (substring match)")
	list := flag.Bool("list", false, "print scenario names and exit")
	forceEnv := flag.Bool("env", false, "require a real object store from PAGEFORGE_E2E_* (fail if unset)")
	flag.Parse()

	realStore := *forceEnv || os.Getenv("PAGEFORGE_E2E_ENDPOINT") != ""
	if *forceEnv {
		if err := validateEnv(); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(2)
		}
	}

	// Resolve workspace root (directory containing go.mod).
	ws, err := os.Getwd()
	if err != nil {
		log.Fatalf("cannot determine CWD: %v", err)
	}
	if gomod := findGoMod(ws); gomod != "" {
		ws = filepath.Dir(gomod)
	}

	if *list {
		args := []string{"test", "./internal/e2e", "-v", "-count=1", "-run", "^$", "-list", "."}
		cmd := exec.Command("go", args...)
		cmd.Dir = ws
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Build the go test invocation.
	testArgs := []string{
		"test", "./internal/e2e",
		"-v", "-count=1", "-p", "1", "-timeout", "300s",
	}
	if *only != "" {
		testArgs = append(testArgs, "-run", "TestCoreScenarios/"+escapeRegex(*only))
	}

	fmt.Printf("▶ pageforge e2e suite (dir=%s)\n", ws)
	if realStore {
		fmt.Printf("  store: real S3-compatible (%s) bucket=%s\n", os.Getenv("PAGEFORGE_E2E_ENDPOINT"), os.Getenv("PAGEFORGE_E2E_BUCKET"))
	} else {
		fmt.Println("  store: in-memory (set PAGEFORGE_E2E_* to use a real bucket)")
	}
	fmt.Printf("  cmd: go %s\n\n", strings.Join(testArgs, " "))

	cmd := exec.Command("go", testArgs...)
	cmd.Dir = ws
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The harness reads PAGEFORGE_E2E_* directly from its own environment.
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		fmt.Printf("\n✗ e2e suite failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\n✓ e2e suite passed")
}

func escapeRegex(s string) string {
	// go test -run accepts RE2; only characters that are both RE2 metacharacters
	// and plausible scenario-name characters need escaping.
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '.', '+', '*', '?', '(', ')', '[', ']', '{', '}', '|', '^', '$', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// findGoMod walks up from start looking for go.mod.
func findGoMod(start string) string {
	dir := start
	for {
		candidate := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// validateEnv checks the variables required for real-store mode.
func validateEnv() error {
	required := []string{
		"PAGEFORGE_E2E_ENDPOINT",
		"PAGEFORGE_E2E_ACCESS_KEY",
		"PAGEFORGE_E2E_SECRET_KEY",
		"PAGEFORGE_E2E_BUCKET",
	}
	var missing []string
	for _, name := range required {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return nil
}
