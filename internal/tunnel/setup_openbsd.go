//go:build openbsd

// Package tunnel - OpenBSD kernel interface setup via ifconfig(8) and route(8).
package tunnel

import (
	"bytes"
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
)

// InterfaceExists reports whether the named kernel network interface exists.
func InterfaceExists(name string) bool {
	return exec.Command("ifconfig", name).Run() == nil
}

func (s *InterfaceSetup) configureInterface() error {
	if s.Description != "" && s.Description != s.Name {
		if err := runCmd("ifconfig", s.Name, "description", "ripws:"+s.Description); err != nil {
			return fmt.Errorf("ripws: set description on %s: %w", s.Name, err)
		}
	}

	if s.MTU > 0 {
		if err := runCmd("ifconfig", s.Name, "mtu", strconv.Itoa(s.MTU)); err != nil {
			return fmt.Errorf("ripws: set MTU on %s: %w", s.Name, err)
		}
	}

	for _, raw := range s.Address {
		cidrArg, ipOnly, isV6, err := parseOpenBSDAddress(raw)
		if err != nil {
			return err
		}
		family := "inet"
		if isV6 {
			family = "inet6"
			if s.firstAddr6 == "" {
				s.firstAddr6 = ipOnly
			}
		} else if s.firstAddr4 == "" {
			s.firstAddr4 = ipOnly
		}
		if err := runCmd("ifconfig", s.Name, family, cidrArg, "alias"); err != nil {
			return fmt.Errorf("ripws: add address %s to %s: %w", raw, s.Name, err)
		}
	}

	if err := runCmd("ifconfig", s.Name, "up"); err != nil {
		return fmt.Errorf("ripws: bring up %s: %w", s.Name, err)
	}

	return nil
}

func (s *InterfaceSetup) teardownInterface() error {
	for _, pr := range s.pinnedRoutes {
		_ = exec.Command("route", "-q", "-n", "delete", pr[0], pr[1]).Run()
	}
	s.pinnedRoutes = nil

	if err := runCmd("ifconfig", s.Name, "destroy"); err != nil {
		return fmt.Errorf("ripws: destroy %s: %w", s.Name, err)
	}
	return nil
}

func (s *InterfaceSetup) addRoute(cidr string) error {
	masked, targets, err := expandOpenBSDRoutes(cidr)
	if err != nil {
		return err
	}

	isV6 := masked.Addr().Is6()
	familyFlag := "-inet"
	ifaceIP := s.firstAddr4
	if isV6 {
		familyFlag = "-inet6"
		ifaceIP = s.firstAddr6
	}
	if ifaceIP == "" {
		return fmt.Errorf("ripws: local %s address required before adding route %s", familyFlag, cidr)
	}

	// Default routes: pin the server endpoint via the pre-tunnel gateway first,
	// then cover 0.0.0.0/0 or ::/0 with two /1 routes via -iface so the host's
	// original default route remains intact underneath.
	if masked.Bits() == 0 && s.Endpoint != "" {
		if ep, err := netip.ParseAddr(s.Endpoint); err == nil && ep.Is6() == isV6 {
			s.pinEndpointRoute(familyFlag, ep)
		}
	}

	for _, target := range targets {
		if err := routeReplace(familyFlag, target.String(), "-iface", ifaceIP, "-ifp", s.Name); err != nil {
			return err
		}
	}
	return nil
}

func (s *InterfaceSetup) pinEndpointRoute(familyFlag string, ep netip.Addr) {
	out, err := exec.Command("route", "-n", "get", familyFlag, ep.Unmap().String()).Output()
	if err != nil {
		return
	}
	gw, outIf := parseOpenBSDRouteGet(out)
	if gw == "" || outIf == s.Name {
		return
	}
	hostCIDR := openBSDHostCIDR(ep)
	if err := routeReplace(familyFlag, hostCIDR, gw); err == nil {
		s.pinnedRoutes = append(s.pinnedRoutes, [2]string{familyFlag, hostCIDR})
	}
}

// routeReplace installs a route cleanly, falling back to `route change` if
// the entry already exists.
func routeReplace(args ...string) error {
	var addErr bytes.Buffer
	addCmd := exec.Command("route", append([]string{"-q", "-n", "add"}, args...)...)
	addCmd.Stderr = &addErr
	if err := addCmd.Run(); err == nil {
		return nil
	} else {
		var chgErr bytes.Buffer
		chgCmd := exec.Command("route", append([]string{"-q", "-n", "change"}, args...)...)
		chgCmd.Stderr = &chgErr
		if cerr := chgCmd.Run(); cerr == nil {
			return nil
		} else {
			msg := strings.TrimSpace(addErr.String())
			if msg == "" {
				msg = strings.TrimSpace(chgErr.String())
			}
			if msg != "" {
				return fmt.Errorf("route %v: %w (%s)", args, err, msg)
			}
			return fmt.Errorf("route %v: %w", args, err)
		}
	}
}
