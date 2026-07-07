package sharing

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroupReadOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance(&lifecycle.Options{
		Email:      "alice@example.net",
		PublicName: "Alice",
	})

	t.Run("checkGroupReadOnlyChangeConsistency", func(t *testing.T) {
		t.Run("no_other_groups", func(t *testing.T) {
			s := &Sharing{
				Owner: true,
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
				},
			}
			err := s.checkGroupReadOnlyChangeConsistency(0, true)
			assert.NoError(t, err)
		})

		t.Run("other_group_same_state", func(t *testing.T) {
			s := &Sharing{
				Owner: true,
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0, 1}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
					{Name: "Family", ReadOnly: true},
				},
			}
			err := s.checkGroupReadOnlyChangeConsistency(0, true)
			assert.NoError(t, err)
		})

		t.Run("other_group_same_state_upgrade", func(t *testing.T) {
			s := &Sharing{
				Owner: true,
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0, 1}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
					{Name: "Family", ReadOnly: true},
				},
			}
			err := s.checkGroupReadOnlyChangeConsistency(0, false)
			assert.ErrorIs(t, err, ErrGroupReadOnlyConflict)
		})

		t.Run("other_group_conflicting_state_downgrade", func(t *testing.T) {
			s := &Sharing{
				Owner: true,
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0, 1}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
					{Name: "Family", ReadOnly: false},
				},
			}
			err := s.checkGroupReadOnlyChangeConsistency(0, true)
			assert.ErrorIs(t, err, ErrGroupReadOnlyConflict)
		})

		t.Run("other_group_conflicting_state_upgrade", func(t *testing.T) {
			s := &Sharing{
				Owner: true,
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0, 1}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
					{Name: "Family", ReadOnly: true},
				},
			}
			err := s.checkGroupReadOnlyChangeConsistency(0, false)
			assert.ErrorIs(t, err, ErrGroupReadOnlyConflict)
		})

		t.Run("conflicting_but_revoked_group_skipped", func(t *testing.T) {
			s := &Sharing{
				Owner: true,
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0, 1}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
					{Name: "Family", ReadOnly: true, Revoked: true},
				},
			}
			err := s.checkGroupReadOnlyChangeConsistency(0, true)
			assert.NoError(t, err)
		})
	})

	t.Run("checkGroupMembersIndividualConsistency", func(t *testing.T) {
		t.Run("all_only_in_groups", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active: true,
				Owner:  true,
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
				},
				Rules: []Rule{
					{Title: "Test", DocType: "io.cozy.tests", Values: []string{uuidv7()}},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))
			err := s.checkGroupMembersIndividualConsistency(0, true)
			assert.NoError(t, err)
		})

		t.Run("individual_matches_target_true", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active: true,
				Owner:  true,
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: true, Groups: []int{0}, OnlyInGroups: false},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
				},
				Rules: []Rule{
					{Title: "Test", DocType: "io.cozy.tests", Values: []string{uuidv7()}},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))
			err := s.checkGroupMembersIndividualConsistency(0, true)
			assert.NoError(t, err)
		})

		t.Run("individual_matches_target_false", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active: true,
				Owner:  true,
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0}, OnlyInGroups: false},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
				},
				Rules: []Rule{
					{Title: "Test", DocType: "io.cozy.tests", Values: []string{uuidv7()}},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))
			err := s.checkGroupMembersIndividualConsistency(0, false)
			assert.NoError(t, err)
		})

		t.Run("individual_conflicts_downgrade", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active: true,
				Owner:  true,
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0}, OnlyInGroups: false},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
				},
				Rules: []Rule{
					{Title: "Test", DocType: "io.cozy.tests", Values: []string{uuidv7()}},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))
			err := s.checkGroupMembersIndividualConsistency(0, true)
			assert.ErrorIs(t, err, ErrGroupReadOnlyConflict)
		})

		t.Run("individual_conflicts_upgrade", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active: true,
				Owner:  true,
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: true, Groups: []int{0}, OnlyInGroups: false},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
				},
				Rules: []Rule{
					{Title: "Test", DocType: "io.cozy.tests", Values: []string{uuidv7()}},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))
			err := s.checkGroupMembersIndividualConsistency(0, false)
			assert.ErrorIs(t, err, ErrGroupReadOnlyConflict)
		})
	})

	t.Run("CheckMemberGroupReadOnlyConsistency", func(t *testing.T) {
		t.Run("member_not_in_any_group", func(t *testing.T) {
			s := &Sharing{
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
				},
			}
			err := s.CheckMemberGroupReadOnlyConsistency(1)
			assert.NoError(t, err)
		})

		t.Run("member_in_read_write_group_rejected", func(t *testing.T) {
			s := &Sharing{
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: true, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
				},
			}
			err := s.CheckMemberGroupReadOnlyConsistency(1)
			assert.ErrorIs(t, err, ErrGroupReadOnlyConflict)
		})

		t.Run("member_in_read_only_group_rejected", func(t *testing.T) {
			s := &Sharing{
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: true, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
				},
			}
			err := s.CheckMemberGroupReadOnlyConsistency(1)
			assert.ErrorIs(t, err, ErrGroupReadOnlyConflict)
		})

		t.Run("member_in_revoked_read_only_group_allowed", func(t *testing.T) {
			s := &Sharing{
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: true, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true, Revoked: true},
				},
			}
			err := s.CheckMemberGroupReadOnlyConsistency(1)
			assert.NoError(t, err)
		})
	})

	t.Run("AddReadOnlyFlagToGroup", func(t *testing.T) {
		t.Run("happy_path", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test group readonly",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0}, OnlyInGroups: true},
					{Status: MemberStatusMailNotSent, Name: "Charlie", ReadOnly: false, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.AddReadOnlyFlagToGroup(inst, 0)
			require.NoError(t, err)

			assert.True(t, s.Groups[0].ReadOnly)
			assert.True(t, s.Members[1].ReadOnly)
			assert.True(t, s.Members[2].ReadOnly)
		})

		t.Run("idempotent", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test idempotent",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.AddReadOnlyFlagToGroup(inst, 0)
			assert.NoError(t, err)
		})

		t.Run("non_owner_error", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       false,
				Description: "Test non-owner",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.AddReadOnlyFlagToGroup(inst, 0)
			assert.ErrorIs(t, err, ErrInvalidSharing)
		})

		t.Run("group_already_ro_is_noop", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test already ro",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: true, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.AddReadOnlyFlagToGroup(inst, 0)
			assert.NoError(t, err)
		})

		t.Run("group_revoked_error", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test revoked group",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false, Revoked: true},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.AddReadOnlyFlagToGroup(inst, 0)
			assert.ErrorIs(t, err, ErrInvalidSharing)
		})

		t.Run("conflict_error", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test conflict",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0, 1}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
					{Name: "Family", ReadOnly: false},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.AddReadOnlyFlagToGroup(inst, 0)
			assert.ErrorIs(t, err, ErrGroupReadOnlyConflict)
		})

		t.Run("conflict_skips_revoked_group", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test conflict skip revoked",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0, 1}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
					{Name: "Family", ReadOnly: true, Revoked: true},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.AddReadOnlyFlagToGroup(inst, 0)
			require.NoError(t, err)
			assert.True(t, s.Groups[0].ReadOnly)
			assert.True(t, s.Members[1].ReadOnly)
		})

		t.Run("member_already_ro_skipped", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test member already ro",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: true, Groups: []int{0}, OnlyInGroups: true},
					{Status: MemberStatusMailNotSent, Name: "Charlie", ReadOnly: false, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.AddReadOnlyFlagToGroup(inst, 0)
			require.NoError(t, err)
			assert.True(t, s.Groups[0].ReadOnly)
			assert.True(t, s.Members[1].ReadOnly)
			assert.True(t, s.Members[2].ReadOnly)
		})

		t.Run("partial_failure_keeps_group_flag_unchanged", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test partial failure keeps group flag",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0}, OnlyInGroups: true},
					{Status: MemberStatusReady, Name: "Charlie", ReadOnly: false, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.AddReadOnlyFlagToGroup(inst, 0)
			require.Error(t, err)
			assert.False(t, s.Groups[0].ReadOnly)
			assert.True(t, s.Members[1].ReadOnly)
			assert.False(t, s.Members[2].ReadOnly)
		})
	})

	t.Run("RemoveReadOnlyFlagFromGroup", func(t *testing.T) {
		t.Run("happy_path", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test group upgrade",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: true, Groups: []int{0}, OnlyInGroups: true},
					{Status: MemberStatusMailNotSent, Name: "Charlie", ReadOnly: true, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.RemoveReadOnlyFlagFromGroup(inst, 0)
			require.NoError(t, err)

			assert.False(t, s.Groups[0].ReadOnly)
			assert.False(t, s.Members[1].ReadOnly)
			assert.False(t, s.Members[2].ReadOnly)
		})

		t.Run("idempotent", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test idempotent upgrade",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: false, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: false},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.RemoveReadOnlyFlagFromGroup(inst, 0)
			assert.NoError(t, err)
		})

		t.Run("non_owner_error", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       false,
				Description: "Test non-owner upgrade",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: true, Groups: []int{0}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.RemoveReadOnlyFlagFromGroup(inst, 0)
			assert.ErrorIs(t, err, ErrInvalidSharing)
		})

		t.Run("group_revoked_error", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test upgrade revoked group",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true, Revoked: true},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.RemoveReadOnlyFlagFromGroup(inst, 0)
			assert.ErrorIs(t, err, ErrInvalidSharing)
		})

		t.Run("conflict_error", func(t *testing.T) {
			now := time.Now()
			s := &Sharing{
				Active:      true,
				Owner:       true,
				Description: "Test upgrade conflict",
				Members: []Member{
					{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
					{Status: MemberStatusMailNotSent, Name: "Bob", ReadOnly: true, Groups: []int{0, 1}, OnlyInGroups: true},
				},
				Groups: []Group{
					{Name: "Friends", ReadOnly: true},
					{Name: "Family", ReadOnly: true},
				},
				Rules: []Rule{
					{
						Title:   "Test",
						DocType: "io.cozy.tests",
						Values:  []string{uuidv7()},
					},
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			require.NoError(t, couchdb.CreateDoc(inst, s))

			err := s.RemoveReadOnlyFlagFromGroup(inst, 0)
			assert.ErrorIs(t, err, ErrGroupReadOnlyConflict)
		})
	})
}
