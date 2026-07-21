package sharing

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
)

// apiEditorURL holds the parameters to build the URL where a file can be
// opened by an editor. It uses the io.cozy.files doctype, as no dedicated
// doctype is needed for editors that only need to open a regular file.
type apiEditorURL struct {
	DocID      string `json:"_id,omitempty"`
	FileID     string `json:"file_id"`
	Protocol   string `json:"protocol"`
	Subdomain  string `json:"subdomain"`
	Instance   string `json:"instance"`
	Sharecode  string `json:"sharecode,omitempty"`
	PublicName string `json:"public_name,omitempty"`
}

func (e *apiEditorURL) ID() string                             { return e.DocID }
func (e *apiEditorURL) Rev() string                            { return "" }
func (e *apiEditorURL) DocType() string                        { return consts.Files }
func (e *apiEditorURL) Clone() couchdb.Doc                     { cloned := *e; return &cloned }
func (e *apiEditorURL) SetID(id string)                        { e.DocID = id }
func (e *apiEditorURL) SetRev(rev string)                      {}
func (e *apiEditorURL) Relationships() jsonapi.RelationshipMap { return nil }
func (e *apiEditorURL) Included() []jsonapi.Object             { return nil }
func (e *apiEditorURL) Links() *jsonapi.LinksList              { return nil }
func (e *apiEditorURL) Fetch(field string) []string            { return nil }

// EditorOpener can be used to find the parameters for creating the URL where a
// file can be opened by an editor.
type EditorOpener struct {
	*FileOpener
}

// OpenEditor returns an EditorOpener for the given file.
func OpenEditor(inst *instance.Instance, fileID string) (*EditorOpener, error) {
	file, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return nil, err
	}

	opener, err := NewFileOpener(inst, file)
	if err != nil {
		return nil, err
	}
	return &EditorOpener{opener}, nil
}

// GetResult looks if the file can be opened locally or not, which code can be
// used in case of a shared file, and other parameters, and returns the
// information.
func (o *EditorOpener) GetResult(memberIndex int, readOnly bool) (jsonapi.Object, error) {
	prepared, err := o.PrepareOpenFileRequest(memberIndex, readOnly)
	if err != nil {
		return nil, err
	}
	var result *apiEditorURL
	if prepared.Opts == nil {
		result, err = o.openLocalFile(prepared.MemberIndex, prepared.ReadOnly)
	} else {
		result, err = o.openSharedFile(prepared)
	}
	if err != nil {
		return nil, err
	}

	// Keep JSON:API data.id local to the caller. FileID remains the file id on
	// the instance returned in Instance, which can differ for cozy-to-cozy shares.
	result.DocID = o.File.ID()
	if name, err := settings.PublicName(o.Inst); err == nil {
		result.PublicName = name
	}
	return result, nil
}

func (o *EditorOpener) openLocalFile(memberIndex int, readOnly bool) (*apiEditorURL, error) {
	params, err := o.OpenLocalFileForMember(memberIndex, readOnly)
	if err != nil {
		return nil, err
	}
	doc := apiEditorURL{
		FileID:    params.FileID,
		Protocol:  params.Protocol,
		Subdomain: params.Subdomain,
		Instance:  params.Instance,
		Sharecode: params.Sharecode,
	}
	return &doc, nil
}

func (o *EditorOpener) openSharedFile(prepared *PreparedRequest) (*apiEditorURL, error) {
	res, err := o.RequestSharedFile(prepared, "/editor/"+prepared.XoredID+"/open")
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return nil, ErrInternalServerError
	}
	var doc apiEditorURL
	if _, err := jsonapi.Bind(res.Body, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}
