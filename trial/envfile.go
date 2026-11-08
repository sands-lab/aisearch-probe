package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// orEnv returns val when it is non-empty, otherwise the environment variable key.
// It backfills a secret flag from the env AFTER the env file is loaded: a flag's
// os.Getenv default is evaluated before loadEnvFile runs, so an env-file-only
// secret would otherwise read as empty. An explicit flag still wins.
func orEnv(val, key string) string {
	if val != "" {
		return val
	}
	return os.Getenv(key)
}

// loadEnvFile reads a KEY=value credentials file into the process environment.
//
// This exists because `source ~/.config/probe-trial/env` does NOT work the way
// it looks like it does: sourcing a file of plain KEY=value lines sets shell
// variables but does not export them, so a child process sees nothing and the
// tool silently reports no credentials. run-pilot.sh avoids that with `set -a`;
// running the binary directly had no such protection.
//
// Values already present in the environment win, so an explicit export still
// overrides the file. Never logs a value — only the names it set.
func loadEnvFile(path string) (names []string, err error) {
	if path == "" {
		return nil, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return nil, herr
		}
		path = filepath.Join(home, path[2:])
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		// Strip one layer of matching quotes.
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if os.Getenv(k) != "" {
			continue // an explicit export wins
		}
		if err := os.Setenv(k, v); err != nil {
			return names, err
		}
		names = append(names, k)
	}
	return names, sc.Err()
}

// credentialSummary reports which provider credentials are present, by NAME
// only. Printed whenever a command finds nothing to do, so "no output" is never
// the answer.
func credentialSummary() []string {
	var found []string
	for _, v := range []string{
		"OPENROUTER_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY",
		"ANTHROPIC_API_KEY", "PERPLEXITY_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
		"XAI_API_KEY",
	} {
		if os.Getenv(v) != "" {
			found = append(found, v)
		}
	}
	return found
}

// noCredentialsText is the help shown when nothing usable was found. It is built
// as a string (not printed directly) so a test can assert it names every key the
// loader actually looks for — a key missing here would be read but never reported.
func noCredentialsText() string {
	var b strings.Builder
	b.WriteString("No provider credentials found in the environment.\n\n")
	b.WriteString("  Looked for: OPENROUTER_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY,\n")
	b.WriteString("              ANTHROPIC_API_KEY, PERPLEXITY_API_KEY, XAI_API_KEY,\n")
	b.WriteString("              GOOGLE_APPLICATION_CREDENTIALS\n")
	return b.String()
}

func printNoCredentials(envPath string) {
	fmt.Print(noCredentialsText())
	fmt.Printf("  Env file  : %s ", envPath)
	if _, err := os.Stat(expandHome(envPath)); err == nil {
		fmt.Println("(exists, but no recognised keys were read from it)")
	} else {
		fmt.Println("(not found)")
	}
	fmt.Println()
	fmt.Println("  Note: plain `source file` sets shell variables WITHOUT exporting")
	fmt.Println("  them, so a child process sees nothing. Either let this tool read")
	fmt.Println("  the file (-keyFile, the default above), or export explicitly:")
	fmt.Println("      set -a; source ~/.config/probe-trial/env; set +a")
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
