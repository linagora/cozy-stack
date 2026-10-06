package sharing

import (
	"strings"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/utils"
)

// IsTrustedMember checks if a member is trusted for auto-accepting sharings.
// Trust is determined by:
// - Domain matching: member's domain matches configured trusted domains (or subdomains)
// - Organization membership: both instances have the same organization ID
// - Contact trust: member's contact has been marked as trusted (by previously accepting a sharing)
//
// Contact-based trust also works when no trusted domains are configured.
func IsTrustedMember(inst *instance.Instance, member *Member) bool {
	if inst == nil || member == nil {
		return false
	}
	options := config.GetSharingConfig(inst.ContextName)

	if !options.AutoAcceptTrusted {
		return false
	}

	// Extract the member's instance domain
	memberDomain := utils.ExtractInstanceHost(member.Instance)
	if memberDomain == "" {
		return false
	}

	// Check if member's domain matches any trusted domain
	for _, domain := range options.TrustedDomains {
		if domain == "" {
			continue
		}
		d := utils.NormalizeDomain(domain)
		if d == "" {
			continue
		}
		if memberDomain == d || strings.HasSuffix(memberDomain, "."+d) {
			inst.Logger().WithNamespace("sharing-trust").
				Infof("Member %s trusted (trusted domain: %s)", member.Instance, domain)
			return true
		}
	}

	if isSameOrganizationMember(inst, member) {
		inst.Logger().WithNamespace("sharing-trust").
			Infof("Member %s trusted (same organization)", member.Instance)
		return true
	}

	// Check if this member is a trusted contact
	if !options.AutoAcceptTrustedContacts {
		return false
	}
	if isTrustedContact(inst, member) {
		inst.Logger().WithNamespace("sharing-trust").
			Infof("Member %s trusted (trusted contact)", member.Instance)
		return true
	}

	return false
}

// isSameOrganizationMember compares the OrgID of the sender's instance on this
// stack. A spoofed URL gains nothing: the answer is sent to that URL.
func isSameOrganizationMember(inst *instance.Instance, member *Member) bool {
	if inst == nil || member == nil || inst.OrgID == "" || !inst.HasCommonContacts() {
		return false
	}
	host := utils.NormalizeDomain(member.InstanceHost())
	if host == "" {
		return false
	}
	sender, err := instance.Get(host)
	return err == nil && sender.OrgID == inst.OrgID
}

// isTrustedContact checks if a member is marked as a trusted contact
func isTrustedContact(inst *instance.Instance, member *Member) bool {
	if member.Email == "" {
		return false
	}

	c, err := contact.FindByEmail(inst, member.Email)
	if err != nil {
		return false
	}

	return c.IsTrusted()
}
