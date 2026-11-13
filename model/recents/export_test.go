package recents

// RecordActivityAt exposes recordActivity with a controlled date to the tests
// of the recents_test package.
var RecordActivityAt = recordActivity

// MarkSeeded exposes markSeeded to the tests of the recents_test package.
var MarkSeeded = markSeeded

// SeededMarkerID exposes seededMarkerID to the tests of the recents_test
// package.
const SeededMarkerID = seededMarkerID
