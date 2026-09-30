package main

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandPath expands a leading ~ and makes the path absolute.
func ExpandPath(p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

func xdgDir(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fallback
	}
	return filepath.Join(home, fallback)
}

// HerdrConfigDir is the herdr configuration root.
func HerdrConfigDir() string { return xdgDir("XDG_CONFIG_HOME", ".config") + "/herdr" }

// SessionSocketPath returns the socket of a named herdr session; an empty or
// "default" name selects the default session.
func SessionSocketPath(session string) string {
	if session == "" || session == "default" {
		return filepath.Join(HerdrConfigDir(), "herdr.sock")
	}
	return filepath.Join(HerdrConfigDir(), "sessions", session, "herdr.sock")
}

// DefaultSocketPath mirrors the herdr client resolution order:
// explicit env, then named session, then the default session socket.
func DefaultSocketPath() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	if s := os.Getenv("HERDR_SESSION"); s != "" && s != "default" {
		return SessionSocketPath(s)
	}
	return SessionSocketPath("")
}

// DefaultStateDir is where the bridge keeps rules, state, pid, and logs.
func DefaultStateDir() string {
	return filepath.Join(xdgDir("XDG_STATE_HOME", ".local/state"), "herdr-bridge")
}

// DefaultConfigPath is the bridge config file location.
func DefaultConfigPath() string {
	if p := os.Getenv("HERDR_BRIDGE_CONFIG"); p != "" {
		return ExpandPath(p)
	}
	return filepath.Join(xdgDir("XDG_CONFIG_HOME", ".config"), "herdr-bridge", "config.json")
}
