//go:build linux || openbsd

// Package tunnel - kernel interface setup (wg-quick-style auto-configuration).
//
// This file implements the shared `ripws up` / `ripws down` orchestration:
// PreUp/PostUp/PreDown/PostDown hooks, address/route/MTU plumbing (delegated to
// OS-specific files), and /etc/resolv.conf management.
package tunnel

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// InterfaceSetup holds the parameters for bringing up a kernel TUN interface
// the wg-quick way.
type InterfaceSetup struct {
	Name        string   // kernel interface name, e.g. "ws0" or "tun0"
	Description string   // optional logical name (e.g. "ws0" when Name is auto-assigned tunN)
	Address     []string // CIDR addresses, e.g. "10.7.0.1/24"
	MTU         int      // MTU (0 = leave default)
	Routes      []string // CIDR routes to add via this interface
	DNS         []string // DNS servers to inject
	Table       string   // "auto" (default) or "off"
	Endpoint    string   // client: the VPN server's host/IP; keeps the WebSocket reachable after the default route moves
	PreUp       []string // shell hooks
	PostUp      []string // shell hooks
	PreDown     []string // shell hooks
	PostDown    []string // shell hooks

	// Platform-internal tracking used by OS setup backends (e.g. OpenBSD
	// -iface gateway address and pinned endpoint host routes).
	firstAddr4   string
	firstAddr6   string
	pinnedRoutes [][2]string
}

// Up configures the interface: runs PreUp, sets MTU, assigns addresses,
// brings the link up, adds routes, configures DNS, and runs PostUp.
func (s *InterfaceSetup) Up() error {
	if err := s.runHooks("PreUp", s.PreUp); err != nil {
		return err
	}

	if err := s.configureInterface(); err != nil {
		return err
	}

	if s.Table != "off" {
		for _, route := range s.Routes {
			if err := s.addRoute(route); err != nil {
				return fmt.Errorf("ripws: add route %s via %s: %w", route, s.Name, err)
			}
		}
	}

	if len(s.DNS) > 0 {
		if err := s.setupDNS(); err != nil {
			return fmt.Errorf("ripws: DNS setup: %w", err)
		}
	}

	if err := s.runHooks("PostUp", s.PostUp); err != nil {
		return err
	}

	return nil
}

// Down tears down the interface: runs PreDown, restores DNS, deletes the
// interface, and runs PostDown. Teardown is best-effort: a failing step is
// logged (into the returned error) but does not abort the remaining steps —
// a broken PreDown hook must not leave the interface alive.
func (s *InterfaceSetup) Down() error {
	var errs []error
	if err := s.runHooks("PreDown", s.PreDown); err != nil {
		errs = append(errs, err)
	}

	if len(s.DNS) > 0 {
		if err := s.restoreDNS(); err != nil {
			errs = append(errs, err)
		}
	}

	if err := s.teardownInterface(); err != nil {
		errs = append(errs, err)
	}

	if err := s.runHooks("PostDown", s.PostDown); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func (s *InterfaceSetup) runHooks(phase string, hooks []string) error {
	for _, h := range hooks {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		cmd := exec.Command("sh", "-c", h)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ripws: %s hook failed: %w", phase, err)
		}
	}
	return nil
}

// resolvBackupPath returns the per-interface resolv.conf backup path so
// multiple tunnels do not clobber each other's backups.
func resolvBackupPath(ifName string) string {
	return "/etc/resolv.conf.ripws-" + ifName + "-backup"
}

func (s *InterfaceSetup) setupDNS() error {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return fmt.Errorf("read resolv.conf: %w", err)
	}
	if err := os.WriteFile(resolvBackupPath(s.Name), data, 0644); err != nil {
		return fmt.Errorf("backup resolv.conf: %w", err)
	}

	var b strings.Builder
	for _, dns := range s.DNS {
		fmt.Fprintf(&b, "nameserver %s\n", dns)
	}
	// Drop the old nameservers rather than keeping them: leaving them would
	// leak queries outside the tunnel (the whole point of a VPN resolver).
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "nameserver") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0644)
}

func (s *InterfaceSetup) restoreDNS() error {
	path := resolvBackupPath(s.Name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil // no backup = nothing to restore
	}
	if err := os.WriteFile("/etc/resolv.conf", data, 0644); err != nil {
		return fmt.Errorf("restore resolv.conf: %w", err)
	}
	os.Remove(path)
	return nil
}

// runCmd runs an administrative command with stdout/stderr forwarded to stderr.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
