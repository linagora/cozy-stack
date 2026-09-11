package vfss3

import (
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
)

func TestMakeObjectKey(t *testing.T) {
	// Standard 32-char docID and 16-char internalID
	key := MakeObjectKey("alice.example.com/", "abcdefghijklmnopqrstuvwxyz012345", "0123456789abcdef")
	assert.Equal(t, "alice.example.com/abcdefghijklmnopqrstuv/wxyz0/12345/0123456789abcdef", key)

	// Non-standard lengths
	key = MakeObjectKey("alice.example.com/", "short", "id")
	assert.Equal(t, "alice.example.com/short/id", key)
}

func TestMakeDocID(t *testing.T) {
	// Standard 51-char object name
	docID, internalID := makeDocID("abcdefghijklmnopqrstuv/wxyz0/12345/0123456789abcdef")
	assert.Equal(t, "abcdefghijklmnopqrstuvwxyz012345", docID)
	assert.Equal(t, "0123456789abcdef", internalID)

	// Non-standard
	docID, internalID = makeDocID("short/id")
	assert.Equal(t, "short", docID)
	assert.Equal(t, "id", internalID)
}

func TestObjectToFileDoc(t *testing.T) {
	obj := minio.ObjectInfo{Key: "files/alice/document/version", ContentType: "text/plain", Size: 7}
	doc := objectToFileDoc(obj, "document/version")
	assert.Equal(t, "document", doc.DocID)
	assert.Equal(t, "version", doc.InternalID)
	assert.EqualValues(t, 7, doc.ByteSize)
}
