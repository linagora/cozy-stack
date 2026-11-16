package job

// OrgGroupsWorker reconciles organization groups on one instance's sharings.
const OrgGroupsWorker = "share-org-groups"

// OrgGroupsMessage identifies directory state to read, rather than a delta to replay.
// An empty GroupID reconciles membership of all existing organization groups.
// FanOut, on the organization instance, schedules that reconciliation on every member instance.
type OrgGroupsMessage struct {
	OrganizationID string `json:"organization_id"`
	GroupID        string `json:"group_id,omitempty"`
	SharingID      string `json:"sharing_id,omitempty"`
	AfterID        string `json:"after_id,omitempty"`
	FanOut         bool   `json:"fan_out,omitempty"`
}
