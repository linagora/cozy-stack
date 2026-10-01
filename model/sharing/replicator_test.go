package sharing

import (
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/revision"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/assert"
)

// Some doctypes for the tests
const testDoctype = "io.cozy.sharing.tests"
const foos = "io.cozy.sharing.test.foos"
const bars = "io.cozy.sharing.test.bars"
const bazs = "io.cozy.sharing.test.bazs"

func TestReplicator(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	t.Run("SequenceNumber", func(t *testing.T) {
		// Start with an empty io.cozy.shared database
		_ = couchdb.ResetDB(inst, consts.Shared)

		s := &Sharing{SID: uuidv7(), Members: []Member{
			{Status: MemberStatusOwner, Name: "Alice"},
			{Status: MemberStatusReady, Name: "Bob"},
		}}
		nb := 5
		for i := 0; i < nb; i++ {
			createASharedRef(t, inst, s.SID)
		}
		m := &s.Members[1]

		rid, err := s.replicationID(m)
		assert.NoError(t, err)
		assert.Equal(t, "sharing-"+s.SID+"-1", rid)

		seq, err := s.getLastSeqNumber(inst, m, "replicator")
		assert.NoError(t, err)
		assert.Empty(t, seq)
		feed, err := s.callChangesFeed(inst, seq)
		assert.NoError(t, err)
		assert.NotEmpty(t, feed.Seq)
		assert.Equal(t, nb, revision.Generation(feed.Seq))
		err = s.UpdateLastSequenceNumber(inst, m, "replicator", feed.Seq)
		assert.NoError(t, err)

		seqU, err := s.getLastSeqNumber(inst, m, "upload")
		assert.NoError(t, err)
		assert.Empty(t, seqU)
		err = s.UpdateLastSequenceNumber(inst, m, "upload", "2-abc")
		assert.NoError(t, err)

		seq2, err := s.getLastSeqNumber(inst, m, "replicator")
		assert.NoError(t, err)
		assert.Equal(t, feed.Seq, seq2)

		err = s.UpdateLastSequenceNumber(inst, m, "replicator", "2-abc")
		assert.NoError(t, err)
		seq3, err := s.getLastSeqNumber(inst, m, "replicator")
		assert.NoError(t, err)
		assert.Equal(t, feed.Seq, seq3)
	})

	t.Run("InitialIndex", func(t *testing.T) {
		// Start with an empty io.cozy.shared database
		_ = couchdb.ResetDB(inst, consts.Shared)

		// Create some documents that are not shared
		for i := 0; i < 10; i++ {
			id := uuidv7()
			createDoc(t, inst, testDoctype, id, map[string]interface{}{"foo": id})
		}

		s := Sharing{SID: uuidv7()}

		// Rule 0 is local => no copy of documents
		settingsDocID := uuidv7()
		createDoc(t, inst, consts.Settings, settingsDocID, map[string]interface{}{"foo": settingsDocID})
		s.Rules = append(s.Rules, Rule{
			Title:   "A local rule",
			DocType: consts.Settings,
			Values:  []string{settingsDocID},
			Local:   true,
		})
		assert.NoError(t, s.InitialIndex(inst, s.Rules[len(s.Rules)-1], len(s.Rules)-1))
		nbShared := 0
		assertNbSharedRef(t, inst, nbShared)

		// Rule 1 is a unique shared document
		oneID := uuidv7()
		oneDoc := createDoc(t, inst, testDoctype, oneID, map[string]interface{}{"foo": "quuuuux"})
		s.Rules = append(s.Rules, Rule{
			Title:   "A unique document",
			DocType: testDoctype,
			Values:  []string{oneID},
		})
		assert.NoError(t, s.InitialIndex(inst, s.Rules[len(s.Rules)-1], len(s.Rules)-1))
		nbShared++
		assertNbSharedRef(t, inst, nbShared)
		oneRef := getSharedRef(t, inst, testDoctype, oneID)
		assert.NotNil(t, oneRef)
		assert.Equal(t, testDoctype+"/"+oneID, oneRef.SID)
		assert.Equal(t, &RevsTree{Rev: oneDoc.Rev()}, oneRef.Revisions)
		assert.Contains(t, oneRef.Infos, s.SID)
		assert.Equal(t, 1, oneRef.Infos[s.SID].Rule)

		// Rule 2 is with a selector
		twoIDs := []string{uuidv7(), uuidv7(), uuidv7()}
		for _, id := range twoIDs {
			createDoc(t, inst, testDoctype, id, map[string]interface{}{"foo": "bar"})
		}
		s.Rules = append(s.Rules, Rule{
			Title:    "the foo: bar documents",
			DocType:  testDoctype,
			Selector: "foo",
			Values:   []string{"bar"},
		})
		assert.NoError(t, s.InitialIndex(inst, s.Rules[len(s.Rules)-1], len(s.Rules)-1))
		nbShared += len(twoIDs)
		assertNbSharedRef(t, inst, nbShared)
		for _, id := range twoIDs {
			twoRef := getSharedRef(t, inst, testDoctype, id)
			assert.NotNil(t, twoRef)
			assert.Contains(t, twoRef.Infos, s.SID)
			assert.Equal(t, 2, twoRef.Infos[s.SID].Rule)
		}

		// Rule 3 is another rule with a selector
		threeIDs := []string{uuidv7(), uuidv7(), uuidv7()}
		for i, id := range threeIDs {
			u := "u"
			for j := 0; j < i; j++ {
				u += "u"
			}
			createDoc(t, inst, testDoctype, id, map[string]interface{}{"foo": "q" + u + "x"})
		}
		s.Rules = append(s.Rules, Rule{
			Title:    "the foo: baz documents",
			DocType:  testDoctype,
			Selector: "foo",
			Values:   []string{"qux", "quux", "quuux"},
		})
		assert.NoError(t, s.InitialIndex(inst, s.Rules[len(s.Rules)-1], len(s.Rules)-1))
		nbShared += len(threeIDs)
		assertNbSharedRef(t, inst, nbShared)
		for _, id := range threeIDs {
			threeRef := getSharedRef(t, inst, testDoctype, id)
			assert.NotNil(t, threeRef)
			assert.Contains(t, threeRef.Infos, s.SID)
			assert.Equal(t, 3, threeRef.Infos[s.SID].Rule)
		}

		// Another member accepts the sharing
		for r, rule := range s.Rules {
			assert.NoError(t, s.InitialIndex(inst, rule, r))
		}
		assertNbSharedRef(t, inst, nbShared)

		// A document is added
		addID := uuidv7()
		twoIDs = append(twoIDs, addID)
		createDoc(t, inst, testDoctype, addID, map[string]interface{}{"foo": "bar"})

		// A document is updated
		updateID := twoIDs[0]
		updateRef := getSharedRef(t, inst, testDoctype, updateID)
		updateRev := updateRef.Revisions.Rev
		updateDoc := updateDoc(t, inst, testDoctype, updateID, updateRev, map[string]interface{}{"foo": "bar", "updated": true})

		// A third member accepts the sharing
		for r, rule := range s.Rules {
			assert.NoError(t, s.InitialIndex(inst, rule, r))
		}
		nbShared++
		assertNbSharedRef(t, inst, nbShared)
		for _, id := range twoIDs {
			twoRef := getSharedRef(t, inst, testDoctype, id)
			assert.NotNil(t, twoRef)
			assert.Contains(t, twoRef.Infos, s.SID)
			assert.Equal(t, 2, twoRef.Infos[s.SID].Rule)
			if id == updateID {
				assert.Equal(t, updateRev, twoRef.Revisions.Rev)
				assert.Equal(t, updateDoc.Rev(), twoRef.Revisions.Branches[0].Rev)
			}
		}

		// Another sharing
		s2 := Sharing{SID: uuidv7()}
		s2.Rules = append(s2.Rules, Rule{
			Title:    "the foo: baz documents",
			DocType:  testDoctype,
			Selector: "foo",
			Values:   []string{"qux", "quux", "quuux"},
		})
		assert.NoError(t, s2.InitialIndex(inst, s2.Rules[len(s2.Rules)-1], len(s2.Rules)-1))
		assertNbSharedRef(t, inst, nbShared)
		for _, id := range threeIDs {
			threeRef := getSharedRef(t, inst, testDoctype, id)
			assert.NotNil(t, threeRef)
			assert.Contains(t, threeRef.Infos, s.SID)
			assert.Equal(t, 3, threeRef.Infos[s.SID].Rule)
			assert.Contains(t, threeRef.Infos, s2.SID)
			assert.Equal(t, 0, threeRef.Infos[s2.SID].Rule)
		}
	})

	t.Run("CallChangesFeed", func(t *testing.T) {
		// Start with an empty io.cozy.shared database
		_ = couchdb.ResetDB(inst, consts.Shared)

		foobars := "io.cozy.tests.foobars"
		id1 := uuidv7()
		id2 := uuidv7()
		s := Sharing{
			SID: uuidv7(),
			Rules: []Rule{
				{
					Title:   "foobars rule",
					DocType: foobars,
					Values:  []string{id1, id2},
				},
			},
		}
		ref1 := createSharedRef(t, inst, s.SID, foobars+"/"+id1, []string{"1-aaa"})
		ref2 := createSharedRef(t, inst, s.SID, foobars+"/"+id2, []string{"3-bbb"})
		appendRevisionToSharedRef(t, inst, ref1, "2-ccc")

		feed, err := s.callChangesFeed(inst, "")
		assert.NoError(t, err)
		assert.NotEmpty(t, feed.Seq)
		assert.Equal(t, 3, revision.Generation(feed.Seq))
		changes := &feed.Changes
		assert.Equal(t, []string{"1-aaa", "2-ccc"}, changes.Changed[ref1.SID])
		assert.Equal(t, []string{"3-bbb"}, changes.Changed[ref2.SID])
		expected := map[string]int{
			foobars + "/" + id1: 0,
			foobars + "/" + id2: 0,
		}
		assert.Equal(t, expected, feed.RuleIndexes)
		assert.False(t, feed.Pending)

		feed2, err := s.callChangesFeed(inst, feed.Seq)
		assert.NoError(t, err)
		assert.Equal(t, feed.Seq, feed2.Seq)
		changes = &feed2.Changes
		assert.Empty(t, changes.Changed)

		appendRevisionToSharedRef(t, inst, ref1, "3-ddd")
		feed3, err := s.callChangesFeed(inst, feed.Seq)
		assert.NoError(t, err)
		assert.NotEmpty(t, feed3.Seq)
		assert.Equal(t, 4, revision.Generation(feed3.Seq))
		changes = &feed3.Changes
		assert.Equal(t, []string{"1-aaa", "2-ccc", "3-ddd"}, changes.Changed[ref1.SID])
		assert.NotContains(t, changes.Changed, ref2.SID)
	})

	t.Run("GetMissingDocs", func(t *testing.T) {
		hellos := "io.cozy.tests.hellos"
		_ = couchdb.CreateDB(inst, hellos)

		id1 := uuidv7()
		doc1 := createDoc(t, inst, hellos, id1, map[string]interface{}{"hello": id1})
		id2 := uuidv7()
		doc2 := createDoc(t, inst, hellos, id2, map[string]interface{}{"hello": id2})
		doc2b := updateDoc(t, inst, hellos, id2, doc2.Rev(), map[string]interface{}{"hello": id2, "bis": true})
		id3 := uuidv7()
		doc3 := createDoc(t, inst, hellos, id3, map[string]interface{}{"hello": id3})
		doc3b := updateDoc(t, inst, hellos, id3, doc3.Rev(), map[string]interface{}{"hello": id3, "bis": true})
		s := Sharing{
			SID: uuidv7(),
			Rules: []Rule{
				{
					Title:   "hellos rule",
					DocType: hellos,
					Values:  []string{id1, id2, id3},
				},
			},
		}

		missings := &Missings{
			hellos + "/" + id1: MissingEntry{
				Missing: []string{doc1.Rev()},
			},
			hellos + "/" + id2: MissingEntry{
				Missing: []string{doc2.Rev(), doc2b.Rev()},
			},
			hellos + "/" + id3: MissingEntry{
				Missing: []string{doc3b.Rev()},
			},
		}
		changes := &Changes{
			Changed: make(Changed),
			Removed: make(Removed),
		}
		results, err := s.getMissingDocs(inst, missings, changes)
		assert.NoError(t, err)
		assert.Contains(t, *results, hellos)
		assert.Len(t, (*results)[hellos], 4)

		var one, two, twob, three map[string]interface{}
		for i, doc := range (*results)[hellos] {
			switch doc["_id"] {
			case id1:
				one = (*results)[hellos][i]
			case id2:
				if _, ok := doc["bis"]; ok {
					twob = (*results)[hellos][i]
				} else {
					two = (*results)[hellos][i]
				}
			case id3:
				three = (*results)[hellos][i]
			}
		}
		assert.NotNil(t, twob)
		assert.NotNil(t, three)

		assert.NotNil(t, one)
		assert.Equal(t, doc1.Rev(), one["_rev"])
		assert.Equal(t, id1, one["hello"])
		assert.Equal(t, float64(1), one["_revisions"].(map[string]interface{})["start"])
		assert.Equal(t, stripGenerations(doc1.Rev()), one["_revisions"].(map[string]interface{})["ids"])

		assert.NotNil(t, two)
		assert.Equal(t, doc2.Rev(), two["_rev"])
		assert.Equal(t, id2, two["hello"])
		assert.Equal(t, float64(1), two["_revisions"].(map[string]interface{})["start"])
		assert.Equal(t, stripGenerations(doc2.Rev()), two["_revisions"].(map[string]interface{})["ids"])

		assert.NotNil(t, twob)
		assert.Equal(t, doc2b.Rev(), twob["_rev"])
		assert.Equal(t, id2, twob["hello"])
		assert.Equal(t, float64(2), twob["_revisions"].(map[string]interface{})["start"])
		assert.Equal(t, stripGenerations(doc2b.Rev(), doc2.Rev()), twob["_revisions"].(map[string]interface{})["ids"])

		assert.NotNil(t, three)
		assert.Equal(t, doc3b.Rev(), three["_rev"])
		assert.Equal(t, id3, three["hello"])
		assert.Equal(t, float64(2), three["_revisions"].(map[string]interface{})["start"])
		assert.Equal(t, stripGenerations(doc3b.Rev(), doc3.Rev()), three["_revisions"].(map[string]interface{})["ids"])
	})

	t.Run("ApplyBulkDocs", func(t *testing.T) {
		// Start with an empty io.cozy.shared database
		_ = couchdb.ResetDB(inst, consts.Shared)
		_ = couchdb.CreateDB(inst, foos)

		s := Sharing{
			SID: uuidv7(),
			Rules: []Rule{
				{
					Title:    "foos rule",
					DocType:  foos,
					Selector: "hello",
					Values:   []string{"world"},
				},
				{
					Title:    "bars rule",
					DocType:  bars,
					Selector: "hello",
					Values:   []string{"world"},
				},
				{
					Title:    "bazs rule",
					DocType:  bazs,
					Selector: "hello",
					Values:   []string{"world"},
				},
			},
		}
		s2 := Sharing{
			SID: uuidv7(),
			Rules: []Rule{
				{
					Title:    "bars rule",
					DocType:  bars,
					Selector: "hello",
					Values:   []string{"world"},
				},
			},
		}

		// Add a new document
		fooOneID := uuidv7()
		payload := DocsByDoctype{
			foos: DocsList{
				{
					"_id":  fooOneID,
					"_rev": "1-abc",
					"_revisions": map[string]interface{}{
						"start": float64(1),
						"ids":   []interface{}{"abc"},
					},
					"hello":  "world",
					"number": "one",
				},
			},
		}
		err := s.ApplyBulkDocs(inst, payload)
		assert.NoError(t, err)
		nbShared := 1
		assertNbSharedRef(t, inst, nbShared)
		doc := getDoc(t, inst, foos, fooOneID)
		assert.Equal(t, "1-abc", doc.Rev())
		assert.Equal(t, "one", doc.Get("number"))
		ref := getSharedRef(t, inst, foos, fooOneID)
		assert.Equal(t, &RevsTree{Rev: "1-abc"}, ref.Revisions)
		assert.Contains(t, ref.Infos, s.SID)
		assert.Equal(t, 0, ref.Infos[s.SID].Rule)

		// Update a document
		payload = DocsByDoctype{
			foos: DocsList{
				{
					"_id":  fooOneID,
					"_rev": "2-def",
					"_revisions": map[string]interface{}{
						"start": float64(2),
						"ids":   []interface{}{"def", "abc"},
					},
					"hello":  "world",
					"number": "one bis",
				},
			},
		}
		err = s.ApplyBulkDocs(inst, payload)
		assert.NoError(t, err)
		assertNbSharedRef(t, inst, nbShared)
		doc = getDoc(t, inst, foos, fooOneID)
		assert.Equal(t, "2-def", doc.Rev())
		assert.Equal(t, "one bis", doc.Get("number"))
		ref = getSharedRef(t, inst, foos, fooOneID)
		expected := &RevsTree{
			Rev: "1-abc",
			Branches: []RevsTree{
				{Rev: "2-def"},
			},
		}
		assert.Equal(t, expected, ref.Revisions)
		assert.Contains(t, ref.Infos, s.SID)
		assert.Equal(t, 0, ref.Infos[s.SID].Rule)

		// Create a reference for another sharing, on a database that does not exist
		barZeroID := uuidv7()
		payload = DocsByDoctype{
			bars: DocsList{
				{
					"_id":  barZeroID,
					"_rev": "1-111",
					"_revisions": map[string]interface{}{
						"start": float64(1),
						"ids":   []interface{}{"111"},
					},
					"hello":  "world",
					"number": "zero",
				},
			},
		}
		err = s2.ApplyBulkDocs(inst, payload)
		assert.NoError(t, err)
		nbShared++
		assertNbSharedRef(t, inst, nbShared)
		doc = getDoc(t, inst, bars, barZeroID)
		assert.Equal(t, "1-111", doc.Rev())
		assert.Equal(t, "zero", doc.Get("number"))
		ref = getSharedRef(t, inst, bars, barZeroID)
		assert.Equal(t, &RevsTree{Rev: "1-111"}, ref.Revisions)
		assert.Contains(t, ref.Infos, s2.SID)
		assert.Equal(t, 0, ref.Infos[s2.SID].Rule)

		// Add documents for two doctypes at the same time
		barTwoID := uuidv7()
		bazThreeID := uuidv7()
		bazFourID := uuidv7()
		payload = DocsByDoctype{
			bars: DocsList{
				{
					"_id":  barTwoID,
					"_rev": "2-caa",
					"_revisions": map[string]interface{}{
						"start": float64(2),
						"ids":   []interface{}{"caa", "baa"},
					},
					"hello":  "world",
					"number": "two",
				},
			},
			bazs: DocsList{
				{
					"_id":  bazThreeID,
					"_rev": "1-ddd",
					"_revisions": map[string]interface{}{
						"start": float64(1),
						"ids":   []interface{}{"ddd"},
					},
					"hello":  "world",
					"number": "three",
				},
				{
					"_id":  bazFourID,
					"_rev": "1-eee",
					"_revisions": map[string]interface{}{
						"start": float64(1),
						"ids":   []interface{}{"eee"},
					},
					"hello":  "world",
					"number": "four",
				},
			},
		}
		err = s.ApplyBulkDocs(inst, payload)
		assert.NoError(t, err)
		nbShared += 3
		assertNbSharedRef(t, inst, nbShared)
		doc = getDoc(t, inst, bars, barTwoID)
		assert.Equal(t, "2-caa", doc.Rev())
		assert.Equal(t, "two", doc.Get("number"))
		ref = getSharedRef(t, inst, bars, barTwoID)
		assert.Equal(t, &RevsTree{Rev: "2-caa"}, ref.Revisions)
		assert.Contains(t, ref.Infos, s.SID)
		assert.Equal(t, 1, ref.Infos[s.SID].Rule)
		doc = getDoc(t, inst, bazs, bazThreeID)
		assert.Equal(t, "1-ddd", doc.Rev())
		assert.Equal(t, "three", doc.Get("number"))
		ref = getSharedRef(t, inst, bazs, bazThreeID)
		assert.Equal(t, &RevsTree{Rev: "1-ddd"}, ref.Revisions)
		assert.Contains(t, ref.Infos, s.SID)
		assert.Equal(t, 2, ref.Infos[s.SID].Rule)
		doc = getDoc(t, inst, bazs, bazFourID)
		assert.Equal(t, "1-eee", doc.Rev())
		assert.Equal(t, "four", doc.Get("number"))
		ref = getSharedRef(t, inst, bazs, bazFourID)
		assert.Equal(t, &RevsTree{Rev: "1-eee"}, ref.Revisions)
		assert.Contains(t, ref.Infos, s.SID)
		assert.Equal(t, 2, ref.Infos[s.SID].Rule)

		// And a mix of all cases
		fooFiveID := uuidv7()
		barSixID := uuidv7()
		barSevenID := uuidv7()
		barEightID := uuidv7()
		barEightRev := createDoc(t, inst, bars, barEightID, map[string]interface{}{"hello": "world", "number": "8"}).Rev()
		payload = DocsByDoctype{
			foos: DocsList{
				{
					"_id":  fooOneID,
					"_rev": "3-fab",
					"_revisions": map[string]interface{}{
						"start": float64(3),
						"ids":   []interface{}{"fab", "def", "abc"},
					},
					"hello":  "world",
					"number": "one ter",
				},
				{
					"_id":  fooFiveID,
					"_rev": "1-aab",
					"_revisions": map[string]interface{}{
						"start": float64(1),
						"ids":   []interface{}{"aab"},
					},
					"hello":  "world",
					"number": "five",
				},
			},
			bars: DocsList{
				{
					"_id":  barSixID,
					"_rev": "1-aac",
					"_revisions": map[string]interface{}{
						"start": float64(1),
						"ids":   []interface{}{"aac"},
					},
					"hello":  "world",
					"number": "six",
				},
				{
					"_id":  barSevenID,
					"_rev": "1-bad",
					"_revisions": map[string]interface{}{
						"start": float64(1),
						"ids":   []interface{}{"bad"},
					},
					"not":    "shared",
					"number": "seven",
				},
				{
					"_id":  barEightID,
					"_rev": barEightRev,
					"_revisions": map[string]interface{}{
						"start": float64(1),
						"ids":   []interface{}{strings.Replace(barEightRev, "1-", "", 1)},
					},
					"hello":  "world",
					"number": "8 bis",
				},
				{
					"_id":  barZeroID,
					"_rev": "2-222",
					"_revisions": map[string]interface{}{
						"start": float64(2),
						"ids":   []interface{}{"222", "111"},
					},
					"hello":  "world",
					"number": "zero bis",
				},
				{
					"_id":  barTwoID,
					"_rev": "3-daa",
					"_revisions": map[string]interface{}{
						"start": float64(3),
						"ids":   []interface{}{"daa", "caa"},
					},
					"hello":  "world",
					"number": "two bis",
				},
			},
			bazs: DocsList{
				{
					"_id":  bazThreeID,
					"_rev": "3-ddf",
					"_revisions": map[string]interface{}{
						"start": float64(3),
						"ids":   []interface{}{"ddf", "dde", "ddd"},
					},
					"hello":  "world",
					"number": "three bis",
				},
			},
		}
		err = s.ApplyBulkDocs(inst, payload)
		assert.NoError(t, err)
		nbShared += 2 // fooFiveID and barSixID
		assertNbSharedRef(t, inst, nbShared)
		doc = getDoc(t, inst, foos, fooOneID)
		assert.Equal(t, "3-fab", doc.Rev())
		assert.Equal(t, "one ter", doc.Get("number"))
		ref = getSharedRef(t, inst, foos, fooOneID)
		expected = &RevsTree{Rev: "1-abc"}
		expected.Add("2-def")
		expected.Add("3-fab")
		assert.Equal(t, expected, ref.Revisions)
		assert.Contains(t, ref.Infos, s.SID)
		assert.Equal(t, 0, ref.Infos[s.SID].Rule)
		doc = getDoc(t, inst, foos, fooFiveID)
		assert.Equal(t, "1-aab", doc.Rev())
		assert.Equal(t, "five", doc.Get("number"))
		ref = getSharedRef(t, inst, foos, fooFiveID)
		assert.Equal(t, &RevsTree{Rev: "1-aab"}, ref.Revisions)
		assert.Contains(t, ref.Infos, s.SID)
		assert.Equal(t, 0, ref.Infos[s.SID].Rule)
		doc = getDoc(t, inst, bazs, bazThreeID)
		assert.Equal(t, "3-ddf", doc.Rev())
		assert.Equal(t, "three bis", doc.Get("number"))
		ref = getSharedRef(t, inst, bazs, bazThreeID)
		expected = &RevsTree{Rev: "1-ddd"}
		expected.Add("2-dde")
		expected.Add("3-ddf")
		assert.Equal(t, expected, ref.Revisions)
		assert.Contains(t, ref.Infos, s.SID)
		assert.Equal(t, 2, ref.Infos[s.SID].Rule)
		doc = getDoc(t, inst, bars, barSixID)
		assert.Equal(t, "1-aac", doc.Rev())
		assert.Equal(t, "six", doc.Get("number"))
		ref = getSharedRef(t, inst, bars, barSixID)
		assert.Equal(t, &RevsTree{Rev: "1-aac"}, ref.Revisions)
		assert.Contains(t, ref.Infos, s.SID)
		assert.Equal(t, 1, ref.Infos[s.SID].Rule)
		doc = getDoc(t, inst, bars, barTwoID)
		assert.Equal(t, "3-daa", doc.Rev())
		assert.Equal(t, "two bis", doc.Get("number"))
		ref = getSharedRef(t, inst, bars, barTwoID)
		expected = &RevsTree{Rev: "2-caa"}
		expected.Add("3-daa")
		assert.Equal(t, expected, ref.Revisions)
		assert.Contains(t, ref.Infos, s.SID)
		assert.Equal(t, 1, ref.Infos[s.SID].Rule)
		// New document rejected because it doesn't match the rules
		assertNoDoc(t, inst, bars, barSevenID)
		// Existing document with no shared reference
		doc = getDoc(t, inst, bars, barEightID)
		assert.Equal(t, barEightRev, doc.Rev())
		assert.Equal(t, "8", doc.Get("number"))
		// Existing document with a shared reference, but not for the good sharing
		doc = getDoc(t, inst, bars, barZeroID)
		assert.Equal(t, "1-111", doc.Rev())
		assert.Equal(t, "zero", doc.Get("number"))
	})
}

func uuidv7() string {
	return uuid.Must(uuid.NewV7()).String()
}

func createASharedRef(t *testing.T, inst *instance.Instance, id string) {
	ref := SharedRef{
		SID:       testDoctype + "/" + uuidv7(),
		Revisions: &RevsTree{Rev: "1-aaa"},
		Infos: map[string]SharedInfo{
			id: {Rule: 0},
		},
	}
	err := couchdb.CreateNamedDocWithDB(inst, &ref)
	assert.NoError(t, err)
}

func createDoc(t *testing.T, inst *instance.Instance, doctype, id string, attrs map[string]interface{}) *couchdb.JSONDoc {
	attrs["_id"] = id
	doc := couchdb.JSONDoc{
		M:    attrs,
		Type: doctype,
	}
	err := couchdb.CreateNamedDocWithDB(inst, &doc)
	assert.NoError(t, err)
	return &doc
}

func updateDoc(t *testing.T, inst *instance.Instance, doctype, id, rev string, attrs map[string]interface{}) *couchdb.JSONDoc {
	doc := couchdb.JSONDoc{
		M:    attrs,
		Type: doctype,
	}
	doc.SetID(id)
	doc.SetRev(rev)
	err := couchdb.UpdateDoc(inst, &doc)
	assert.NoError(t, err)
	return &doc
}

func getSharedRef(t *testing.T, inst *instance.Instance, doctype, id string) *SharedRef {
	var ref SharedRef
	err := couchdb.GetDoc(inst, consts.Shared, doctype+"/"+id, &ref)
	assert.NoError(t, err)
	return &ref
}

func assertNbSharedRef(t *testing.T, inst *instance.Instance, expected int) {
	nb, err := couchdb.CountAllDocs(inst, consts.Shared)
	if err != nil {
		time.Sleep(1 * time.Second)
		nb, err = couchdb.CountAllDocs(inst, consts.Shared)
	}
	assert.NoError(t, err)
	assert.Equal(t, expected, nb)
}

func createSharedRef(t *testing.T, inst *instance.Instance, sharingID, sid string, revisions []string) *SharedRef {
	tree := &RevsTree{Rev: revisions[0]}
	sub := tree
	for _, rev := range revisions[1:] {
		sub.Branches = []RevsTree{
			{Rev: rev},
		}
		sub = &sub.Branches[0]
	}
	ref := SharedRef{
		SID:       sid,
		Revisions: tree,
		Infos: map[string]SharedInfo{
			sharingID: {Rule: 0},
		},
	}
	err := couchdb.CreateNamedDocWithDB(inst, &ref)
	assert.NoError(t, err)
	return &ref
}

func appendRevisionToSharedRef(t *testing.T, inst *instance.Instance, ref *SharedRef, revision string) {
	ref.Revisions.Add(revision)
	err := couchdb.UpdateDoc(inst, ref)
	assert.NoError(t, err)
}

func stripGenerations(revs ...string) []interface{} {
	res := make([]interface{}, len(revs))
	for i, rev := range revs {
		parts := strings.SplitN(rev, "-", 2)
		res[i] = parts[1]
	}
	return res
}

func getDoc(t *testing.T, inst *instance.Instance, doctype, id string) *couchdb.JSONDoc {
	var doc couchdb.JSONDoc
	err := couchdb.GetDoc(inst, doctype, id, &doc)
	assert.NoError(t, err)
	return &doc
}

func assertNoDoc(t *testing.T, inst *instance.Instance, doctype, id string) {
	var doc couchdb.JSONDoc
	err := couchdb.GetDoc(inst, doctype, id, &doc)
	assert.Error(t, err)
}
