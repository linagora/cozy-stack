package sharing

import (
	"fmt"
	"strings"

	"github.com/cozy/cozy-stack/pkg/utils"
)

// RecipientSource describes one sharing scope through which a recipient has
// access to the target: which sharing, which root, and how the access can be
// managed from the target's share modal.
type RecipientSource struct {
	SharingID   string `json:"sharing_id"`
	RootID      string `json:"root_id"`
	RootName    string `json:"root_name"`
	Kind        string `json:"kind"` // "self" | "ancestor"
	MemberIndex int    `json:"member_index"`
	ReadOnly    bool   `json:"read_only"`
	Manageable  bool   `json:"manageable"`
}

// EffectiveRecipient is a deduplicated person who can access the target,
// either through the target's own share ("self") or inherited from a shared
// ancestor ("ancestor"). ReadOnly is merged across sources with read-write
// winning over read-only. CanEditHere is true when at least one source is
// the target's own share.
type EffectiveRecipient struct {
	Name        string            `json:"name"`
	Email       string            `json:"email"`
	Instance    string            `json:"instance"`
	Status      string            `json:"status"`
	ReadOnly    bool              `json:"read_only"`
	CanEditHere bool              `json:"can_edit_here"`
	Sources     []RecipientSource `json:"sources"`
}

// EffectiveRecipients returns the combined list of people who can access the
// given file or folder: the direct members of every active additive sharing
// scope applying to the target (its own share plus inherited ancestor
// shares). Revoked members are excluded. Recipients are deduplicated by
// instance, with an email fallback. It is a read-only view: no sharing
// document is mutated and no inherited member is copied anywhere.
//
// When member is not nil, the scopes whose root the member cannot read are
// left out: a member of a nested drive learns nothing about the drive above.
func (r *AccessResolver) EffectiveRecipients(targetID string, member *Member) ([]EffectiveRecipient, error) {
	sharings, rootBySharing, err := r.applicableSharings(targetID)
	if err != nil {
		return nil, err
	}

	var recipients []EffectiveRecipient
	index := make(map[string]int)
	dropped := make(map[int]bool)
	for _, s := range sharings {
		info, ok := rootBySharing[s.SID]
		if !ok {
			continue
		}
		if member != nil && !memberReadsRoot(sharings, rootBySharing, info.RootPath, member) {
			continue
		}
		kind := "ancestor"
		if info.RootID == targetID {
			kind = "self"
		}
		canManage := false
		if self := s.MemberFor(r.inst); self != nil && !self.ReadOnly {
			canManage = true
		}
		for i := range s.Members {
			m := &s.Members[i]
			if m.Status == MemberStatusRevoked {
				continue
			}
			candidate := EffectiveRecipient{
				Name:     m.PrimaryName(),
				Email:    m.Email,
				Instance: m.Instance,
				Status:   m.Status,
				ReadOnly: m.ReadOnly,
				Sources: []RecipientSource{{
					SharingID:   s.SID,
					RootID:      info.RootID,
					RootName:    info.RootName,
					Kind:        kind,
					MemberIndex: i,
					ReadOnly:    m.ReadOnly,
					// i != 0: RevokeRecipient rejects the owner entry (index 0),
					// so it must not be reported as manageable.
					Manageable: kind == "self" && canManage && i != 0,
				}},
			}
			candidate.CanEditHere = kind == "self"

			key, aliases := recipientKeys(s.SID, i, m)
			pos, ok := index[key]
			if !ok {
				// The canonical key may miss when an earlier sharing
				// only registered an alias for this person: fall back
				// to the aliases before creating a duplicate.
				for _, alias := range aliases {
					if p, found := index[alias]; found {
						pos, ok = p, true
						break
					}
				}
			}
			if ok {
				recipients[pos].absorb(&candidate)
				index[key] = pos
				for _, alias := range aliases {
					if q, found := index[alias]; found && q != pos {
						// The alias was registered by another recipient that
						// turns out to be the same person (e.g. known by
						// instance in one share, by email in another, and a
						// third share bridges the two): merge it instead of
						// leaving an orphaned duplicate behind.
						recipients[pos].absorb(&recipients[q])
						dropped[q] = true
						for k, v := range index {
							if v == q {
								index[k] = pos
							}
						}
					}
					index[alias] = pos
				}
				continue
			}
			pos = len(recipients)
			index[key] = pos
			for _, alias := range aliases {
				index[alias] = pos
			}
			recipients = append(recipients, candidate)
		}
	}
	if len(dropped) > 0 {
		kept := recipients[:0]
		for i := range recipients {
			if !dropped[i] {
				kept = append(kept, recipients[i])
			}
		}
		recipients = kept
	}
	return recipients, nil
}

// absorb merges another occurrence of the same person into the recipient:
// read-write wins over read-only, the most advanced status is kept, missing
// identity fields are filled in, and sources are combined.
func (rc *EffectiveRecipient) absorb(other *EffectiveRecipient) {
	rc.ReadOnly = rc.ReadOnly && other.ReadOnly // read-write wins
	rc.CanEditHere = rc.CanEditHere || other.CanEditHere
	if statusRank(other.Status) > statusRank(rc.Status) {
		rc.Status = other.Status
	}
	if (rc.Name == "" || rc.Name == rc.Email) && other.Name != "" {
		// The current name is empty or just the email fallback:
		// prefer an actual name when another source has one.
		rc.Name = other.Name
	}
	if rc.Email == "" {
		rc.Email = other.Email
	}
	if rc.Instance == "" {
		rc.Instance = other.Instance
	}
	rc.Sources = append(rc.Sources, other.Sources...)
}

// recipientKeys builds the dedup keys for a member: the instance host when
// known plus the lowercased email, so a person invited by email in one share
// and known by instance in another is still merged. The first key is the
// canonical one (instance preferred over email). When neither is set, a
// per-member unique key prevents merging nameless pending members.
func recipientKeys(sharingID string, memberIndex int, m *Member) (string, []string) {
	var keys []string
	if host := m.InstanceHost(); host != "" {
		keys = append(keys, "instance:"+host)
	}
	if m.Email != "" {
		keys = append(keys, "email:"+strings.ToLower(m.Email))
	}
	if len(keys) == 0 {
		return fmt.Sprintf("member:%s:%d", sharingID, memberIndex), nil
	}
	return keys[0], keys[1:]
}

// memberReadsRoot tells if the member belongs to one of the sharings whose
// root is rootPath or above it. The sharings all apply to the same target, so
// their roots are on its path and no other lookup is needed.
func memberReadsRoot(sharings []*Sharing, rootBySharing map[string]rootInfo, rootPath string, member *Member) bool {
	for _, s := range sharings {
		info, ok := rootBySharing[s.SID]
		if !ok {
			continue
		}
		above := info.RootPath == rootPath || strings.HasPrefix(rootPath, utils.EnsureHasSuffix(info.RootPath, "/"))
		if above && s.MemberMatching(member) != nil {
			return true
		}
	}
	return false
}
