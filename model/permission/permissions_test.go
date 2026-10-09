package permission

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPrefix(t *testing.T) prefixer.Prefixer {
	t.Helper()
	return prefixer.NewPrefixer(0, "test", t.Name())
}

func TestCheckDoctypeName(t *testing.T) {
	assert.NoError(t, CheckDoctypeName("io.cozy.files", false))
	assert.NoError(t, CheckDoctypeName("io.cozy.account_types", false))
	assert.Error(t, CheckDoctypeName("IO.COZY.FILES", false))
	assert.Error(t, CheckDoctypeName("io.cozy.account-types", false))
	assert.Error(t, CheckDoctypeName(".io.cozy.files", false))
	assert.Error(t, CheckDoctypeName("io.cozy.files.", false))
	assert.Error(t, CheckDoctypeName("io.cozy.files.*", false))
	assert.Error(t, CheckDoctypeName("io..cozy..files", false))
	assert.Error(t, CheckDoctypeName("*", false))

	assert.NoError(t, CheckDoctypeName("io.cozy.files", true))
	assert.NoError(t, CheckDoctypeName("io.cozy.banks.*", true))
	assert.NoError(t, CheckDoctypeName("io.cozy.files.*", true))
	assert.Error(t, CheckDoctypeName("io.cozy.*", true))
	assert.Error(t, CheckDoctypeName("com.bitwarden.*", true))
	assert.Error(t, CheckDoctypeName("*", true))
}

func TestVerbToString(t *testing.T) {
	vs := Verbs(GET, DELETE)
	assert.Equal(t, "GET,DELETE", vs.String())

	vs3 := ALL
	assert.Equal(t, "ALL", vs3.String())

	vs4 := VerbSplit("ALL")
	assert.Equal(t, "ALL", vs4.String())
}

func TestRuleToJSON(t *testing.T) {
	r := Rule{
		Type:  "io.cozy.contacts",
		Verbs: Verbs(GET, POST),
	}

	b, err := json.Marshal(r)
	assert.NoError(t, err)
	assert.Equal(t, `{"type":"io.cozy.contacts","verbs":["GET","POST"]}`, string(b))
}

func TestSetToJSON(t *testing.T) {
	s := Set{
		Rule{
			Title:       "images",
			Description: "Required for the background",
			Type:        "io.cozy.files",
			Verbs:       Verbs(GET),
			Values:      []string{"io.cozy.files.music-dir"},
		},
		Rule{
			Title:       "contacts",
			Description: "Required for autocompletion on @name",
			Type:        "io.cozy.contacts",
			Verbs:       Verbs(GET),
		},
		Rule{
			Title:       "mail",
			Description: "Required to send a congratulations email to your friends",
			Type:        "io.cozy.jobs",
			Selector:    "worker",
			Values:      []string{"sendmail"},
		},
	}

	b, err := json.Marshal(s)
	assert.NoError(t, err)
	assertEqualJSON(t, b, `{
    "images": {
      "type": "io.cozy.files",
      "description": "Required for the background",
      "verbs": ["GET"],
      "values": ["io.cozy.files.music-dir"]
    },
    "contacts": {
      "type": "io.cozy.contacts",
      "description": "Required for autocompletion on @name",
      "verbs": ["GET"]
    },
    "mail": {
      "type": "io.cozy.jobs",
      "description": "Required to send a congratulations email to your friends",
      "selector": "worker",
      "values": ["sendmail"]
    }
  }`)
}

func TestJSON2Set(t *testing.T) {
	jsonSet := []byte(`{
    "images": {
      "type": "io.cozy.files",
      "description": "Required for the background",
      "verbs": ["ALL"],
      "values": ["io.cozy.files.music-dir"]
    },
    "contacts": {
      "type": "io.cozy.contacts",
      "description": "Required for autocompletion on @name",
      "verbs": ["GET","PUT"]
    },
    "mail": {
      "type": "io.cozy.jobs",
      "description": "Required to send a congratulations email to your friends",
      "selector": "worker",
      "values": ["sendmail"]
    }
  }`)
	var s Set
	err := json.Unmarshal(jsonSet, &s)
	assert.NoError(t, err)
	assert.Len(t, s, 3)
	assert.Equal(t, "images", s[0].Title)
	assert.Equal(t, "contacts", s[1].Title)
	assert.Equal(t, "mail", s[2].Title)
}

func TestHasSameRules(t *testing.T) {
	s := Set{
		Rule{
			Title:       "images",
			Description: "Required for the background",
			Type:        "io.cozy.files",
			Verbs:       Verbs(GET),
			Values:      []string{"io.cozy.files.music-dir"},
		},
		Rule{
			Title:       "contacts",
			Description: "Required for autocompletion on @name",
			Type:        "io.cozy.contacts",
			Verbs:       Verbs(GET),
		},
		Rule{
			Title:       "mail",
			Description: "Required to send a congratulations email to your friends",
			Type:        "io.cozy.jobs",
			Selector:    "worker",
			Values:      []string{"sendmail"},
		},
	}

	b, err := json.Marshal(s)
	assert.NoError(t, err)
	var other Set
	err = json.Unmarshal(b, &other)
	assert.NoError(t, err)
	assert.Len(t, other, 3)
	assert.True(t, s.HasSameRules(other))
}

func TestBadJSONSet(t *testing.T) {
	jsonSet := []byte(`{
    "contacts": {
      "type": "io.cozy.contacts",
      "description": "Required for autocompletion on @name",
      "verbs": ["BAD"]
    }
  }`)
	var s Set
	err := json.Unmarshal(jsonSet, &s)
	assert.Error(t, err)
	assert.Equal(t, ErrBadScope, err)
}

func TestJSONSetVerbParsing(t *testing.T) {
	var s Set
	jsonSet := []byte(`{
    "contacts": {
      "type": "io.cozy.contacts",
      "description": "Required for autocompletion on @name",
      "verbs": ["GET","PUT"]
    }
  }`)
	err := json.Unmarshal(jsonSet, &s)
	assert.NoError(t, err)
	assert.Len(t, s, 1)
	assert.EqualValues(t, VerbSet{"GET": struct{}{}, "PUT": struct{}{}}, s[0].Verbs)

	jsonSet = []byte(`{
    "contacts": {
      "type": "io.cozy.contacts",
      "description": "Required for autocompletion on @name",
      "verbs": ["ALL", "GET"]
    }
  }`)
	err = json.Unmarshal(jsonSet, &s)
	assert.NoError(t, err)
	assert.Len(t, s, 1)
	assert.EqualValues(t, VerbSet{}, s[0].Verbs)
}

func TestSetToString(t *testing.T) {
	s := Set{
		Rule{
			Title:       "contacts",
			Description: "Required for autocompletion on @name",
			Type:        "io.cozy.contacts",
		},
		Rule{
			Title:       "images",
			Description: "Required for the background",
			Type:        "io.cozy.files",
			Verbs:       Verbs(GET),
			Values:      []string{"io.cozy.files.music-dir"},
		},
		Rule{
			Title:    "sendmail",
			Type:     "io.cozy.jobs",
			Selector: "worker",
			Values:   []string{"sendmail"},
		},
	}

	out, err := s.MarshalScopeString()
	assert.NoError(t, err)
	assert.Equal(t, out, "io.cozy.contacts io.cozy.files:GET:io.cozy.files.music-dir io.cozy.jobs:ALL:sendmail:worker")
}

func TestStringToSet(t *testing.T) {
	_, err := UnmarshalRuleString("")
	assert.Error(t, err)

	_, err = UnmarshalRuleString("*")
	assert.Error(t, err)

	_, err = UnmarshalRuleString("type:verb:selec:value:wtf")
	assert.Error(t, err)

	set, err := UnmarshalScopeString("io.cozy.contacts io.cozy.files:GET:io.cozy.files.music-dir")

	assert.NoError(t, err)
	assert.Len(t, set, 2)
	assert.Equal(t, "io.cozy.contacts", set[0].Type)
	assert.Equal(t, "io.cozy.files", set[1].Type)
	assert.Len(t, set[1].Verbs, 1)
	assert.Equal(t, Verbs(GET), set[1].Verbs)
	assert.Len(t, set[1].Values, 1)
	assert.Equal(t, "io.cozy.files.music-dir", set[1].Values[0])

	rule, err := UnmarshalRuleString("io.cozy.events:GET:mygreatcalendar,othercalendar:calendar-id")
	assert.NoError(t, err)
	assert.Equal(t, "io.cozy.events", rule.Type)
	assert.Equal(t, Verbs(GET), rule.Verbs)
	assert.Len(t, rule.Values, 2)
	assert.Equal(t, "mygreatcalendar", rule.Values[0])
	assert.Equal(t, "othercalendar", rule.Values[1])
	assert.Equal(t, "calendar-id", rule.Selector)
}

func TestAllowType(t *testing.T) {
	s := Set{Rule{Type: "io.cozy.contacts"}}
	assert.True(t, s.Allow(GET, &validable{doctype: "io.cozy.contacts"}))
	assert.True(t, s.Allow(DELETE, &validable{doctype: "io.cozy.contacts"}))
	assert.False(t, s.Allow(GET, &validable{doctype: "io.cozy.files"}))
}

func TestAllowWildcard(t *testing.T) {
	s := Set{Rule{Type: "io.cozy.bank.*"}}
	assert.True(t, s.Allow(GET, &validable{doctype: "io.cozy.bank"}))
	assert.True(t, s.Allow(DELETE, &validable{doctype: "io.cozy.bank.accounts"}))
	assert.True(t, s.Allow(DELETE, &validable{doctype: "io.cozy.bank.accounts.stats"}))
	assert.True(t, s.Allow(DELETE, &validable{doctype: "io.cozy.bank.settings"}))
	assert.False(t, s.Allow(GET, &validable{doctype: "io.cozy.files"}))
	assert.False(t, s.Allow(GET, &validable{doctype: "io.cozy.files.bank"}))
	assert.False(t, s.Allow(GET, &validable{doctype: "io.cozy.banks"}))
	assert.False(t, s.Allow(GET, &validable{doctype: "io.cozy.bankrupts"}))
}

func TestAllowMaximal(t *testing.T) {
	s := Set{Rule{Type: "*"}}
	assert.True(t, s.Allow(GET, &validable{doctype: "io.cozy.files"}))
	assert.True(t, s.Allow(DELETE, &validable{doctype: "io.cozy.files.versions"}))
}

func TestAllowVerbs(t *testing.T) {
	s := Set{Rule{Type: "io.cozy.contacts", Verbs: Verbs(GET)}}
	assert.True(t, s.Allow(GET, &validable{doctype: "io.cozy.contacts"}))
	assert.False(t, s.Allow(DELETE, &validable{doctype: "io.cozy.contacts"}))
	assert.False(t, s.Allow(GET, &validable{doctype: "io.cozy.files"}))
}

func TestAllowValues(t *testing.T) {
	s := Set{Rule{
		Type:   "io.cozy.contacts",
		Values: []string{"id1"},
	}}
	assert.True(t, s.Allow(POST, &validable{doctype: "io.cozy.contacts", id: "id1"}))
	assert.False(t, s.Allow(POST, &validable{doctype: "io.cozy.contacts", id: "id2"}))
}

func TestAllowValuesSelector(t *testing.T) {
	s := Set{Rule{
		Type:     "io.cozy.contacts",
		Selector: "foo",
		Values:   []string{"bar"},
	}}
	assert.True(t, s.Allow(GET, &validable{
		doctype: "io.cozy.contacts",
		values:  map[string]string{"foo": "bar"}}))

	assert.False(t, s.Allow(GET, &validable{
		doctype: "io.cozy.contacts",
		values:  map[string]string{"foo": "baz"}}))
}

func TestAllowWholeType(t *testing.T) {
	s := Set{Rule{Type: "io.cozy.contacts", Verbs: Verbs(GET)}}
	assert.True(t, s.AllowWholeType(GET, "io.cozy.contacts"))

	s2 := Set{Rule{Type: "io.cozy.contacts", Values: []string{"id1"}}}
	assert.False(t, s2.AllowWholeType(GET, "io.cozy.contacts"))
}

func TestAllowID(t *testing.T) {
	s := Set{Rule{Type: "io.cozy.contacts"}}
	assert.True(t, s.AllowID(GET, "io.cozy.contacts", "id1"))

	s2 := Set{Rule{Type: "io.cozy.contacts", Values: []string{"id1"}}}
	assert.True(t, s2.AllowID(GET, "io.cozy.contacts", "id1"))

	s3 := Set{Rule{Type: "io.cozy.contacts", Selector: "foo", Values: []string{"bar"}}}
	assert.False(t, s3.AllowID(GET, "io.cozy.contacts", "id1"))
}

func TestAllowCustomType(t *testing.T) {
	s := Set{Rule{Type: "io.cozy.files", Selector: "path", Values: []string{"/testp/"}}}

	y := &validableFile{"/testp/test"}
	n := &validableFile{"/not-testp/test"}

	assert.True(t, s.Allow(GET, y))
	assert.False(t, s.Allow(GET, n))
}

func TestSubset(t *testing.T) {
	s := Set{Rule{Type: "io.cozy.events"}}

	s2 := Set{Rule{Type: "io.cozy.events"}}
	assert.True(t, s2.IsSubSetOf(s))

	s3 := Set{Rule{Type: "io.cozy.events", Values: []string{"foo", "bar"}}}
	assert.True(t, s3.IsSubSetOf(s))

	s4 := Set{Rule{Type: "io.cozy.events", Values: []string{"foo"}}}
	assert.True(t, s4.IsSubSetOf(s3))
	assert.False(t, s3.IsSubSetOf(s4))

	s5 := Set{Rule{Type: "io.cozy.events", Selector: "calendar", Values: []string{"foo", "bar"}}}
	s6 := Set{Rule{Type: "io.cozy.events", Selector: "calendar", Values: []string{"foo"}}}
	assert.True(t, s6.IsSubSetOf(s5))
	assert.False(t, s5.IsSubSetOf(s6))
}

func TestShareSetPermissions(t *testing.T) {
	setFiles := Set{Rule{Type: "io.cozy.files"}}
	setFilesWildCard := Set{Rule{Type: "io.cozy.files.*"}}
	setEvents := Set{Rule{Type: "io.cozy.events"}}

	parent := &Permission{Type: TypeCLI, Permissions: setEvents}
	err := CheckSetPermissions(setFiles, parent)
	assert.Error(t, err)

	parent.Type = TypeWebapp
	err = CheckSetPermissions(setFiles, parent)
	assert.Error(t, err)

	parent.Permissions = setFiles
	err = CheckSetPermissions(setFiles, parent)
	assert.NoError(t, err)

	parent.Type = TypeShareInteract
	err = CheckSetPermissions(setFiles, parent)
	assert.NoError(t, err)

	parent.Type = TypeWebapp
	err = CheckSetPermissions(setFilesWildCard, parent)
	assert.Error(t, err)

	parent.Permissions = setFilesWildCard
	err = CheckSetPermissions(setFilesWildCard, parent)
	assert.NoError(t, err)

	// share-interact subset ignores selector/values and only checks type+verbs,
	// as file boundary checks are enforced by shared-drive endpoints.
	childFileRule := Set{Rule{
		Type:   "io.cozy.files",
		Verbs:  Verbs(GET),
		Values: []string{"child-file-id"},
	}}
	parent = &Permission{
		Type: TypeShareInteract,
		Permissions: Set{Rule{
			Type:     "io.cozy.files",
			Verbs:    Verbs(GET),
			Selector: "id",
			Values:   []string{"drive-root-id"},
		}},
	}
	err = CheckSetPermissions(childFileRule, parent)
	assert.NoError(t, err)

	// For non share-interact parents, values remain part of subset checks.
	parent.Type = TypeWebapp
	err = CheckSetPermissions(childFileRule, parent)
	assert.Error(t, err)

	// share-interact still enforces verbs coverage.
	parent.Type = TypeShareInteract
	childFileRule[0].Verbs = Verbs(POST)
	err = CheckSetPermissions(childFileRule, parent)
	assert.Error(t, err)
}

func TestShareSetPermissionsAllVerbs(t *testing.T) {
	for _, verbField := range []string{`"verbs":["ALL"],`, `"verbs":[],`, `"verbs":null,`, ``} {
		t.Run(verbField, func(t *testing.T) {
			var rule Rule
			require.NoError(t, json.Unmarshal([]byte(`{`+verbField+`"type":"io.cozy.files","values":["file-id"]}`), &rule))
			for _, parentType := range []string{TypeWebapp, TypeShareInteract} {
				parent := &Permission{
					Type: parentType,
					Permissions: Set{Rule{
						Type: consts.Files, Verbs: Verbs(GET), Values: []string{"file-id"},
					}},
				}
				assert.ErrorIs(t, CheckSetPermissions(Set{rule}, parent), ErrNotSubset, parentType)
				parent.Permissions[0].Verbs = ALL
				assert.NoError(t, CheckSetPermissions(Set{rule}, parent), parentType)
			}
		})
	}
}

func TestGetForShareInteractRepairsDuplicateDocs(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	db := testPrefix(t)
	require.NoError(t, couchdb.ResetDB(db, consts.Permissions))

	const sharingID = "sharing-duplicate-interact-permissions"
	perms := Permission{
		Permissions: Set{{
			Title:  "Shared drive",
			Type:   consts.Files,
			Values: []string{"shared-drive-root"},
			Verbs:  ALL,
		}},
	}

	err := couchdb.CreateDoc(db, &Permission{
		Type:        TypeShareInteract,
		Permissions: perms.Permissions,
		Codes: map[string]string{
			"alice@example.test": "alice-token",
		},
		SourceID: consts.Sharings + "/" + sharingID,
	})
	require.NoError(t, err)
	err = couchdb.CreateDoc(db, &Permission{
		Type:        TypeShareInteract,
		Permissions: perms.Permissions,
		Codes: map[string]string{
			"bob@example.test": "bob-token",
		},
		SourceID: consts.Sharings + "/" + sharingID,
	})
	require.NoError(t, err)

	repaired, err := GetForShareInteract(db, sharingID)
	require.NoError(t, err)
	require.Equal(t, ShareInteractPermissionID(sharingID), repaired.ID())
	require.Equal(t, map[string]string{
		"alice@example.test": "alice-token",
		"bob@example.test":   "bob-token",
	}, repaired.Codes)

	all, err := getShareInteractPermissions(db, sharingID)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, ShareInteractPermissionID(sharingID), all[0].ID())

	repaired, err = GetForShareInteract(db, sharingID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"alice@example.test": "alice-token",
		"bob@example.test":   "bob-token",
	}, repaired.Codes)

	all, err = getShareInteractPermissions(db, sharingID)
	require.NoError(t, err)
	require.Len(t, all, 1)
}

func TestGetForShareInteractRepairsExpiredDuplicateDocs(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	db := testPrefix(t)
	require.NoError(t, couchdb.ResetDB(db, consts.Permissions))

	const sharingID = "sharing-expired-duplicate-interact-permissions"
	rules := Set{{
		Title:  "Shared drive",
		Type:   consts.Files,
		Values: []string{"shared-drive-root"},
		Verbs:  ALL,
	}}
	expiredAt := time.Now().Add(-time.Hour).Format(time.RFC3339)

	err := couchdb.CreateNamedDoc(db, &Permission{
		PID:         ShareInteractPermissionID(sharingID),
		Type:        TypeShareInteract,
		Permissions: rules,
		Codes: map[string]string{
			"alice@example.test": "alice-token",
		},
		SourceID: consts.Sharings + "/" + sharingID,
	})
	require.NoError(t, err)
	err = couchdb.CreateDoc(db, &Permission{
		Type:        TypeShareInteract,
		Permissions: rules,
		Codes: map[string]string{
			"expired@example.test": "expired-token",
		},
		ExpiresAt: expiredAt,
		SourceID:  consts.Sharings + "/" + sharingID,
	})
	require.NoError(t, err)

	repaired, err := GetForShareInteract(db, sharingID)
	require.NoError(t, err)
	require.Equal(t, ShareInteractPermissionID(sharingID), repaired.ID())
	require.Equal(t, map[string]string{
		"alice@example.test": "alice-token",
	}, repaired.Codes)

	all, err := getShareInteractPermissions(db, sharingID)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, ShareInteractPermissionID(sharingID), all[0].ID())
}

func TestCreateShareInteractSetUsesCanonicalDoc(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	db := testPrefix(t)
	require.NoError(t, couchdb.ResetDB(db, consts.Permissions))

	const sharingID = "sharing-canonical-interact-permissions"
	md := metadata.New()
	md.UpdatedAt = time.Now().Add(-time.Hour)
	perms := Permission{
		Permissions: Set{{
			Title:  "Shared drive",
			Type:   consts.Files,
			Values: []string{"shared-drive-root"},
			Verbs:  ALL,
		}},
		Metadata: md,
	}

	first, err := CreateShareInteractSet(db, sharingID, map[string]string{
		"alice@example.test": "alice-token",
	}, perms)
	require.NoError(t, err)
	require.Equal(t, ShareInteractPermissionID(sharingID), first.ID())

	second, err := CreateShareInteractSet(db, sharingID, map[string]string{
		"bob@example.test": "bob-token",
	}, perms)
	require.NoError(t, err)
	require.Equal(t, ShareInteractPermissionID(sharingID), second.ID())
	require.Equal(t, map[string]string{
		"alice@example.test": "alice-token",
		"bob@example.test":   "bob-token",
	}, second.Codes)
	require.NotNil(t, first.Metadata)
	require.NotNil(t, second.Metadata)
	require.True(t, second.Metadata.UpdatedAt.After(first.Metadata.UpdatedAt))

	all, err := getShareInteractPermissions(db, sharingID)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, ShareInteractPermissionID(sharingID), all[0].ID())
}

func TestCreateShareInteractSetReactivatesExpiredCanonicalDoc(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	db := testPrefix(t)
	require.NoError(t, couchdb.ResetDB(db, consts.Permissions))

	const sharingID = "sharing-expired-canonical-interact-permissions"
	rules := Set{{
		Title:  "Shared drive",
		Type:   consts.Files,
		Values: []string{"shared-drive-root"},
		Verbs:  ALL,
	}}
	expiredAt := time.Now().Add(-time.Hour).Format(time.RFC3339)

	err := couchdb.CreateNamedDoc(db, &Permission{
		PID:         ShareInteractPermissionID(sharingID),
		Type:        TypeShareInteract,
		Permissions: rules,
		Codes: map[string]string{
			"alice@example.test": "alice-token",
		},
		ExpiresAt: expiredAt,
		SourceID:  consts.Sharings + "/" + sharingID,
	})
	require.NoError(t, err)

	updated, err := CreateShareInteractSet(db, sharingID, map[string]string{
		"bob@example.test": "bob-token",
	}, Permission{Permissions: rules})
	require.NoError(t, err)
	require.Nil(t, updated.ExpiresAt)
	require.Equal(t, map[string]string{
		"alice@example.test": "alice-token",
		"bob@example.test":   "bob-token",
	}, updated.Codes)

	stored, err := GetForShareInteract(db, sharingID)
	require.NoError(t, err)
	require.Nil(t, stored.ExpiresAt)
}

func TestCreateShareInteractSetDoesNotRepairLegacyDuplicates(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	db := testPrefix(t)
	require.NoError(t, couchdb.ResetDB(db, consts.Permissions))

	const sharingID = "sharing-create-with-legacy-duplicate"
	rules := Set{{
		Title:  "Shared drive",
		Type:   consts.Files,
		Values: []string{"shared-drive-root"},
		Verbs:  ALL,
	}}

	err := couchdb.CreateDoc(db, &Permission{
		Type:        TypeShareInteract,
		Permissions: rules,
		Codes: map[string]string{
			"alice@example.test": "alice-token",
		},
		SourceID: consts.Sharings + "/" + sharingID,
	})
	require.NoError(t, err)

	created, err := CreateShareInteractSet(db, sharingID, map[string]string{
		"bob@example.test": "bob-token",
	}, Permission{Permissions: rules})
	require.NoError(t, err)
	require.Equal(t, ShareInteractPermissionID(sharingID), created.ID())
	require.Equal(t, map[string]string{
		"bob@example.test": "bob-token",
	}, created.Codes)

	all, err := getShareInteractPermissions(db, sharingID)
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func TestCreateShareSetBlocklist(t *testing.T) {
	s := Set{Rule{Type: "io.cozy.notifications"}}
	subdoc := Permission{
		Permissions: s,
	}
	parent := &Permission{Type: TypeWebapp, Permissions: s}
	_, err := CreateShareSet(nil, parent, "", nil, nil, subdoc, nil, false)
	assert.Error(t, err)
	e, ok := err.(*echo.HTTPError)
	assert.True(t, ok)
	assert.Equal(t, "reserved doctype io.cozy.notifications unwritable", e.Message)

	s = Set{Rule{Type: "*"}}
	subdoc = Permission{
		Permissions: s,
	}
	parent = &Permission{Type: TypeWebapp, Permissions: s}
	_, err = CreateShareSet(nil, parent, "", nil, nil, subdoc, nil, false)
	assert.Error(t, err)
}

func assertEqualJSON(t *testing.T, value []byte, expected string) {
	expectedBytes := new(bytes.Buffer)
	err := json.Compact(expectedBytes, []byte(expected))
	assert.NoError(t, err)
	assert.Equal(t, expectedBytes.String(), string(value))
}

type validable struct {
	id      string
	doctype string
	values  map[string]string
}

func (t *validable) ID() string      { return t.id }
func (t *validable) DocType() string { return t.doctype }
func (t *validable) Fetch(field string) []string {
	return []string{t.values[field]}
}

type validableFile struct {
	path string
}

func (t *validableFile) ID() string      { return t.path }
func (t *validableFile) DocType() string { return "io.cozy.files" }
func (t *validableFile) Fetch(field string) []string {
	if field != "path" {
		return nil
	}
	var prefixes []string
	parts := strings.Split(t.path, "/")
	for i := 1; i < len(parts); i++ {
		prefix := strings.Join(parts[:i], "/") + "/"
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}
