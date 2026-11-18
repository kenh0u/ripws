//go:build linux

// Package tunnel - kernel interface setup (wg-quick-style auto-configuration).
//
// This file implements the `ripws up` / `ripws down` convenience layer:
// after the TUN device is created, addresses, MTU, routes, and DNS are
// applied automatically via `ip` and `resolvconf` / direct resolv.conf
// editing — mirroring wg-quick's behaviour.
package tunnel

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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
	Endpoint string   // client: the VPN server's host/IP; keeps the WebSocket reachable after the default route moves
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

	if err := ip("link", "del", "dev", s.Name); err != nil {
		errs = append(errs, fmt.Errorf("ripws: delete %s: %w", s.Name, err))
	}

	if err := s.runHooks("PostDown", s.PostDown); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// addRoute installs one CIDR route via the interface. A default route
// (0.00.0.0/0 or ::/0) cannot simply be `ip route add`ed when the host
// already has a default route — that fails and would abort `up` on any
// normally-connected machine. Instead, mirroring wg-quick's Table=off+
// policy-routing approach, we move the existing default route into a
// dedicated routing table and install ours in the main table.
func (s *InterfaceSetup) addRoute(cidr string) error {
	if cidr != "0.0.0.0/0" && cidr != "::/0" {
		// Use `replace` so routes that already exist (e.g. a connected route
		// created by the interface address) are silently idempotent,
		// mirroring wg-quick's lenient behaviour.
		return ip("route", "replace", cidr, "dev", s.Name)
	}

	family := "-4"
	defCmd := "default"
	if cidr == "::/0" {
		family = "-6"
	}

	// 1. Preserve the current default route in the RIPWS routing table so
	//    `down` (and the endpoint pin below) can still reach it.
	if out, err := exec.Command("ip", family, "route", "show", "default").Output(); err == nil {
		fields := bytes.Fields(out)
		if len(fields) >= 2 {
			args := []string{family, "route", "replace"}
			for _, f := range fields {
				args = append(args, string(f))
			}
			args = append(args, "table", strconv.Itoa(routeTable))
			if err := exec.Command("ip", args...).Run(); err != nil {
				return fmt.Errorf("ripws: preserve default route in table %d: %w", routeTable, err)
			}
			// 2. Pin the VPN endpoint via the preserved gateway so the outer
			//    WebSocket connection survives our new default route.
			if s.Endpoint != "" && string(fields[0]) == "via" {
				if err := ip("route", "replace", s.Endpoint+"/32", "via", string(fields[1]), "onlink"); err != nil {
					return fmt.Errorf("ripws: pin endpoint %s via %s: %w", s.Endpoint, string(fields[1]), err)
				}
			}
		}
	}
	// 3. Install our default route in the main table (replace: idempotent
	//    even if a default route already exists there).
	return ip(family, "route", "replace", defCmd, "dev", s.Name)
}

// routeTable is the auxiliary routing table where the pre-VPN default route
// is stashed while the tunnel owns the main table's default.
const routeTable = 51820 // wg-quick's conventional offload table

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

// ip runs the `ip` command with the given arguments.
func ip(args ...string) error {
	cmd := exec.Command("ip", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
