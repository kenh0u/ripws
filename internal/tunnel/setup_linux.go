//go:build linux

// Package tunnel - Linux kernel interface setup via iproute2 (`ip`).
package tunnel

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
)

// InterfaceExists reports whether the named kernel network interface exists.
func InterfaceExists(name string) bool {
	return exec.Command("ip", "link", "show", name).Run() == nil
}

func (s *InterfaceSetup) configureInterface() error {
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

	return nil
}

func (s *InterfaceSetup) teardownInterface() error {
	if err := ip("link", "del", "dev", s.Name); err != nil {
		return fmt.Errorf("ripws: delete %s: %w", s.Name, err)
	}
	return nil
}

// addRoute installs one CIDR route via the interface. A default route
// (0.0.0.0/0 or ::/0) cannot simply be `ip route add`ed when the host
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

// ip runs the `ip` command with the given arguments.
func ip(args ...string) error {
	return runCmd("ip", args...)
}
