package jsonapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRelationshipMapClone(t *testing.T) {
	var empty RelationshipMap
	assert.Nil(t, empty.Clone())

	relationships := RelationshipMap{
		"file": {
			Data: map[string]interface{}{
				"_id": "file-1",
				"metadata": map[string]interface{}{
					"label": "original",
				},
			},
		},
	}

	cloned := relationships.Clone()
	clonedFile := cloned["file"]
	clonedData := clonedFile.Data.(map[string]interface{})
	clonedData["_id"] = "file-2"
	clonedData["metadata"].(map[string]interface{})["label"] = "changed"
	delete(cloned, "file")

	originalFile := relationships["file"]
	originalData := originalFile.Data.(map[string]interface{})
	assert.Contains(t, relationships, "file")
	assert.Equal(t, "file-1", originalData["_id"])
	assert.Equal(t, "original", originalData["metadata"].(map[string]interface{})["label"])
}
