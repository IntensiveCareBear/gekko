package main

import (
	"log"
	"os"
	"strings"
)

// liveTradingEnabled is the master switch for credentials: without LIVE_TRADING=true the broker
// loads no exchange secrets at all, from the environment or from files.
func liveTradingEnabled() bool { return strings.EqualFold(os.Getenv("LIVE_TRADING"), "true") }

// brokerSecret returns the secret NAME from the environment, or from the file named by NAME_FILE.
// It returns "" unless LIVE_TRADING=true. A file readable by group or others is refused, since
// these hold wallet and exchange keys (chmod 600). Values are never logged.
func brokerSecret(name string) string {
	if !liveTradingEnabled() {
		return ""
	}
	if v := os.Getenv(name); v != "" {
		return v
	}
	path := os.Getenv(name + "_FILE")
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return "" // absent file simply means "not configured"
	}
	if info.Mode().Perm()&0o077 != 0 {
		log.Printf("[ERROR] Refusing %s_FILE %s: mode %o is readable by others (chmod 600)", name, path, info.Mode().Perm())
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[ERROR] Cannot read %s_FILE %s: %v", name, path, err)
		return ""
	}
	return strings.TrimSpace(string(b))
}
