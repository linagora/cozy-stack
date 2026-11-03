package recents

// MarkSeeded exposes markSeeded to the tests of the recents_test package.
var MarkSeeded = markSeeded

// SeededMarkerID exposes seededMarkerID to the tests of the recents_test
// package.
const SeededMarkerID = seededMarkerID

// RecordActivityAt exposes recordActivity with a controlled date to the tests
// of the recents_test package.
var RecordActivityAt = recordActivity

// GetInstancesByIDs exposes the loader of the source instances to the tests
// of the recents_test package, to simulate a failure of CouchDB.
var GetInstancesByIDs = &getInstancesByIDs

// WorkSource exposes the recents-source worker to the tests of the
// recents_test package.
var WorkSource = workSource

// SourceBatchSize exposes sourceBatchSize to the tests of the recents_test
// package.
const SourceBatchSize = sourceBatchSize

// FollowTarget exposes followTarget, the decision of FollowMove, to the tests
// of the recents_test package: the jobs it pushes need a job system.
var FollowTarget = followTarget

// EachSourceBatch exposes eachSourceBatch to the tests of the recents_test
// package.
var EachSourceBatch = eachSourceBatch

// ErrUnknownMove exposes errUnknownMove to the tests of the recents_test
// package.
var ErrUnknownMove = errUnknownMove

// SourceMembers exposes sourceMembers, the members of PrepareDestroy and
// FollowMove, to the tests of the recents_test package.
var SourceMembers = sourceMembers

// DestroyTarget exposes destroyTarget, the decision of PrepareDestroy, to the
// tests of the recents_test package: the jobs it pushes need a job system.
var DestroyTarget = destroyTarget
