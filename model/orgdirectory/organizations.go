package orgdirectory

import (
	"context"
	"fmt"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/utils"
)

// OrganizationInstances is the resolved local instance scope for an
// organization-directory operation.
type OrganizationInstances struct {
	OrganizationID string
	Instances      []*instance.Instance
}

// ResolveOrganizationInstances retrieves organization instances by ID when it
// is available, otherwise by organization domain.
func ResolveOrganizationInstances(organizationID, organizationDomain string) (OrganizationInstances, error) {
	organizationID = strings.TrimSpace(organizationID)
	organizationDomain = utils.NormalizeDomain(organizationDomain)

	var list []*instance.Instance
	var err error
	if organizationID != "" {
		list, err = lifecycle.ListOrgInstancesByID(organizationID)
		if err != nil {
			return OrganizationInstances{}, fmt.Errorf("list organization instances by id %s: %w", organizationID, err)
		}
	} else if organizationDomain != "" {
		list, err = lifecycle.ListOrgInstances(organizationDomain)
		if err != nil {
			return OrganizationInstances{}, fmt.Errorf("list organization instances by domain %s: %w", organizationDomain, err)
		}
	} else {
		return OrganizationInstances{}, fmt.Errorf("missing organizationId or organization domain")
	}

	if len(list) == 0 {
		return OrganizationInstances{}, fmt.Errorf("organization has no instances")
	}

	resolvedID := organizationID
	if resolvedID == "" {
		for _, inst := range list {
			resolvedID = strings.TrimSpace(inst.OrgID)
			if resolvedID != "" {
				break
			}
		}
	}

	return OrganizationInstances{
		OrganizationID: resolvedID,
		Instances:      list,
	}, nil
}

func findOrganizationInstance(ctx context.Context, organizationID string) (*instance.Instance, error) {
	scope, err := ResolveOrganizationInstances(organizationID, "")
	if err != nil {
		return nil, fmt.Errorf("org-directory: %w", err)
	}
	for _, inst := range scope.Instances {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if inst.IsOrganizationInstance() {
			return inst, nil
		}
	}
	return nil, nil
}
