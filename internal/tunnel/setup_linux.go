//go:build linux

// Package tunnel - kernel interface setup (wg-quick-style auto-configuration).
//
// This file implements the `ripws up` / `ripws down` convenience layer:
// after the TUN device is created, addresses, MTU, routes, and DNS are
// applied automatically via `ip` and `resolvconf` / direct resolv.conf
// editing — mirroring wg-quick's behaviour.
package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// InterfaceSetup holds the parameters for bringing up a kernel TUN interface
// the wg-quick way.
type InterfaceSetup struct {
	Name     string   // interface name, e.g. "ws0"
	Address  []string // CIDR addresses, e.g. "10.7.0.1/24"
	MTU      int      // MTU (0 = leave default)
	Routes   []string // CIDR routes to add via this interface
	DNS      []string // DNS servers to inject
	Table    string   // "auto" (default) or "off"
	PreUp    []string // shell hooks
	PostUp   []string // shell hooks
	PreDown  []string // shell hooks
	PostDown []string // shell hooks
}

// Up configures the interface: runs PreUp, sets MTU, assigns addresses,
// brings the link up, adds routes, configures DNS, and runs PostUp.
func (s *InterfaceSetup) Up() error {
	if err := s.runHooks("PreUp", s.PreUp); err != nil {
		return err
	}

	if s.MTU > 0 {
		if err := ip("link", "set", "dev", s.Name, "mtu", fmt.Sprint(s.MTU)); err != nil {
			return fmt.Errorf("ripws: set MTU on %s: %w", s.Name, err)
		}
	}

	for _, addr := range s.Address {
		if err := ip("addr", "add", addr, "dev", s.Name); err != nil {
			return fmt.Errorf("ripws: add address %s to %s: %w", addr, s.Name, err)
		}
	}

	if err := ip("link", "set", "up", "dev", s.Name); err != nil {
		return fmt.Errorf("ripws: bring up %s: %w", s.Name, err)
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

// Down tears down the interface: runs PreDown, removes routes (automatic on
// interface deletion), restores DNS, deletes the interface, and runs PostDown.
func (s *InterfaceSetup) Down() error {
	if err := s.runHooks("PreDown", s.PreDown); err != nil {
		return err
	}

	if len(s.DNS) > 0 {
		if err := s.restoreDNS(); err != nil {
			return fmt.Errorf("ripws: DNS restore: %w", err)
		}
	}

	if err := ip("link", "del", "dev", s.Name); err != nil {
		return fmt.Errorf("ripws: delete %s: %w", s.Name, err)
	}

	if err := s.runHooks("PostDown", s.PostDown); err != nil {
		return err
	}

	return nil
}

func (s *InterfaceSetup) addRoute(cidr string) error {
	// Skip the default route — it's already handled by the interface being up
	// with its assigned address, or by a gateway route. wg-quick also has
	// special handling for default routes; we keep it simple here.
	if cidr == "0.0.0.0/0" || cidr == "::/0" {
		return ip("route", "add", "default", "dev", s.Name)
	}
	// Use `replace` so routes that already exist (e.g. a connected route
	// created by the interface address) are silently idempotent, mirroring
	// wg-quick's lenient behaviour.
	return ip("route", "replace", cidr, "dev", s.Name)
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

const resolvBackup = "/etc/resolv.conf.ripws-backup"

func (s *InterfaceSetup) setupDNS() error {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return fmt.Errorf("read resolv.conf: %w", err)
	}
	if err := os.WriteFile(resolvBackup, data, 0644); err != nil {
		return fmt.Errorf("backup resolv.conf: %w", err)
	}

	var b strings.Builder
	for _, dns := range s.DNS {
		fmt.Fprintf(&b, "nameserver %s\n", dns)
	}
	b.Write(data)
	return os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0644)
}

func (s *InterfaceSetup) restoreDNS() error {
	data, err := os.ReadFile(resolvBackup)
	if err != nil {
		return nil // no backup = nothing to restore
	}
	if err := os.WriteFile("/etc/resolv.conf", data, 0644); err != nil {
		return fmt.Errorf("restore resolv.conf: %w", err)
	}
	os.Remove(resolvBackup)
	return nil
}

// ip runs the `ip` command with the given arguments.
func ip(args ...string) error {
	cmd := exec.Command("ip", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
