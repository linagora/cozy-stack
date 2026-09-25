package sharing

import (
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	jwt "github.com/golang-jwt/jwt/v5"
)

func TestOnlyOfficeDocumentConfig(t *testing.T) {
	tests := []struct {
		name         string
		class        string
		documentType string
		fileType     string
	}{
		{name: "letter.docx", class: "text", documentType: "word", fileType: "docx"},
		{name: "budget.XLSX", class: "spreadsheet", documentType: "cell", fileType: "xlsx"},
		{name: "slides.PPTX", class: "slide", documentType: "slide", fileType: "pptx"},
		{name: "document.PDF", class: "pdf", documentType: "pdf", fileType: "pdf"},
	}

	for _, tt := range tests {
		t.Run(tt.class, func(t *testing.T) {
			file := &vfs.FileDoc{DocName: tt.name, Class: tt.class}
			if !isOfficeDocument(file) {
				t.Fatalf("expected class %q to be supported", tt.class)
			}
			if got := documentType(file); got != tt.documentType {
				t.Errorf("documentType() = %q, want %q", got, tt.documentType)
			}
			if got := fileType(file); got != tt.fileType {
				t.Errorf("fileType() = %q, want %q", got, tt.fileType)
			}
		})
	}
}

func TestOnlyOfficeConfigSignature(t *testing.T) {
	const secret = "inbox-secret"
	doc := apiOfficeURL{OO: &onlyOffice{Type: "pdf"}}
	doc.OO.Doc.FileType = "pdf"
	doc.OO.Doc.Key = "document-key"
	doc.OO.Doc.Permissions.Edit = true

	token, err := doc.sign(&config.Office{InboxSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	claims := &onlyOffice{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Valid {
		t.Fatal("expected a valid OnlyOffice configuration signature")
	}
	if claims.Type != "pdf" || !claims.Doc.Permissions.Edit {
		t.Fatalf("unexpected signed claims: documentType=%q edit=%t", claims.Type, claims.Doc.Permissions.Edit)
	}
}
