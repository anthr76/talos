// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package operator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/nclient6"
	"github.com/jsimonetti/rtnetlink"
	"github.com/talos-systems/go-retry/retry"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"inet.af/netaddr"

	"github.com/talos-systems/talos/pkg/machinery/nethelpers"
	"github.com/talos-systems/talos/pkg/machinery/resources/network"
)

// DHCP6 implements the DHCPv6 network operator.
type DHCP6 struct {
	logger *zap.Logger

	linkName string

	mu        sync.Mutex
	addresses []network.AddressSpecSpec
	hostname  []network.HostnameSpecSpec
	resolvers []network.ResolverSpecSpec
}

// NewDHCP6 creates DHCPv6 operator.
func NewDHCP6(logger *zap.Logger, linkName string) *DHCP6 {
	return &DHCP6{
		logger:   logger,
		linkName: linkName,
	}
}

// Prefix returns unique operator prefix which gets prepended to each spec.
func (d *DHCP6) Prefix() string {
	return fmt.Sprintf("dhcp6/%s", d.linkName)
}

// Run the operator loop.
//
//nolint:gocyclo,dupl
func (d *DHCP6) Run(ctx context.Context, notifyCh chan<- struct{}) {
	iface, err := net.InterfaceByName(d.linkName)
	if err != nil {
		d.logger.Warn("link not found", zap.String("link", d.linkName))
	} else if err = d.waitIPv6LinkReady(ctx, iface); err != nil {
		d.logger.Warn("error waiting for IPv6 ready", zap.Error(err), zap.String("link", d.linkName))
	}

	const minRenewDuration = 5 * time.Second // protect from renewing too often

	renewInterval := minRenewDuration

	for {
		leaseTime, err := d.renew(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			d.logger.Warn("renew failed", zap.Error(err), zap.String("link", d.linkName))
		}

		if err == nil {
			select {
			case notifyCh <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}

		if leaseTime > 0 {
			renewInterval = leaseTime / 2
		} else {
			renewInterval /= 2
		}

		if renewInterval < minRenewDuration {
			renewInterval = minRenewDuration
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(renewInterval):
		}
	}
}

// AddressSpecs implements Operator interface.
func (d *DHCP6) AddressSpecs() []network.AddressSpecSpec {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.addresses
}

// LinkSpecs implements Operator interface.
func (d *DHCP6) LinkSpecs() []network.LinkSpecSpec {
	return nil
}

// RouteSpecs implements Operator interface.
func (d *DHCP6) RouteSpecs() []network.RouteSpecSpec {
	return nil
}

// HostnameSpecs implements Operator interface.
func (d *DHCP6) HostnameSpecs() []network.HostnameSpecSpec {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.hostname
}

// ResolverSpecs implements Operator interface.
func (d *DHCP6) ResolverSpecs() []network.ResolverSpecSpec {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.resolvers
}

// TimeServerSpecs implements Operator interface.
func (d *DHCP6) TimeServerSpecs() []network.TimeServerSpecSpec {
	return nil
}

func (d *DHCP6) parseReply(reply *dhcpv6.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if reply.Options.OneIANA() != nil && reply.Options.OneIANA().Options.OneAddress() != nil {
		addr, _ := netaddr.FromStdIPNet(&net.IPNet{
			IP:   reply.Options.OneIANA().Options.OneAddress().IPv6Addr,
			Mask: net.CIDRMask(128, 128),
		})

		d.addresses = []network.AddressSpecSpec{
			{
				Address:     addr,
				LinkName:    d.linkName,
				Family:      nethelpers.FamilyInet6,
				Scope:       nethelpers.ScopeGlobal,
				Flags:       nethelpers.AddressFlags(nethelpers.AddressPermanent),
				ConfigLayer: network.ConfigOperator,
			},
		}
	} else {
		d.addresses = nil
	}

	if len(reply.Options.DNS()) > 0 {
		dns := make([]netaddr.IP, len(reply.Options.DNS()))

		for i := range dns {
			dns[i], _ = netaddr.FromStdIP(reply.Options.DNS()[i])
		}

		d.resolvers = []network.ResolverSpecSpec{
			{
				DNSServers:  dns,
				ConfigLayer: network.ConfigOperator,
			},
		}
	} else {
		d.resolvers = nil
	}

	if reply.Options.FQDN() != nil && len(reply.Options.FQDN().DomainName.Labels) > 0 {
		d.hostname = []network.HostnameSpecSpec{
			{
				Hostname:    reply.Options.FQDN().DomainName.Labels[0],
				Domainname:  strings.Join(reply.Options.FQDN().DomainName.Labels[1:], "."),
				ConfigLayer: network.ConfigOperator,
			},
		}
	} else {
		d.hostname = nil
	}
}

func (d *DHCP6) renew(ctx context.Context) (time.Duration, error) {
	cli, err := nclient6.New(d.linkName)
	if err != nil {
		return 0, err
	}

	defer cli.Close() //nolint:errcheck

	reply, err := cli.RapidSolicit(ctx)
	if err != nil {
		return 0, err
	}

	d.logger.Debug("DHCP6 REPLY", zap.String("link", d.linkName), zap.String("dhcp", collapseSummary(reply.Summary())))

	d.parseReply(reply)

	return reply.Options.OneIANA().Options.OneAddress().ValidLifetime, nil
}

func (d *DHCP6) waitIPv6LinkReady(ctx context.Context, iface *net.Interface) error {
	conn, err := rtnetlink.Dial(nil)
	if err != nil {
		return err
	}

	defer conn.Close() //nolint:errcheck

	return retry.Constant(30*time.Second, retry.WithUnits(100*time.Millisecond)).RetryWithContext(ctx, func(ctx context.Context) error {
		ready, err := d.isIPv6LinkReady(iface, conn)
		if err != nil {
			return err
		}

		if !ready {
			return retry.ExpectedErrorf("IPv6 address is still tentative")
		}

		return nil
	})
}

// isIPv6LinkReady returns true if the interface has a link-local address
// which is not tentative.
func (d *DHCP6) isIPv6LinkReady(iface *net.Interface, conn *rtnetlink.Conn) (bool, error) {
	addrs, err := conn.Address.List()
	if err != nil {
		return false, err
	}

	for _, addr := range addrs {
		if addr.Index != uint32(iface.Index) {
			continue
		}

		if addr.Family != unix.AF_INET6 {
			continue
		}

		if addr.Attributes.Address.IsLinkLocalUnicast() && (addr.Flags&unix.IFA_F_TENTATIVE == 0) {
			if addr.Flags&unix.IFA_F_DADFAILED != 0 {
				d.logger.Warn("DADFAILED for %v, continuing anyhow", zap.Stringer("address", addr.Attributes.Address), zap.String("link", d.linkName))
			}

			return true, nil
		}
	}

	return false, nil
}
