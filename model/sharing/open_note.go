package sharing

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/note"
	"github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
)

type apiNoteURL struct {
	DocID      string `json:"_id,omitempty"`
	NoteID     string `json:"note_id"`
	Protocol   string `json:"protocol"`
	Subdomain  string `json:"subdomain"`
	Instance   string `json:"instance"`
	Sharecode  string `json:"sharecode,omitempty"`
	PublicName string `json:"public_name,omitempty"`
}

func (n *apiNoteURL) ID() string                             { return n.DocID }
func (n *apiNoteURL) Rev() string                            { return "" }
func (n *apiNoteURL) DocType() string                        { return consts.NotesURL }
func (n *apiNoteURL) Clone() couchdb.Doc                     { cloned := *n; return &cloned }
func (n *apiNoteURL) SetID(id string)                        { n.DocID = id }
func (n *apiNoteURL) SetRev(rev string)                      {}
func (n *apiNoteURL) Relationships() jsonapi.RelationshipMap { return nil }
func (n *apiNoteURL) Included() []jsonapi.Object             { return nil }
func (n *apiNoteURL) Links() *jsonapi.LinksList              { return nil }
func (n *apiNoteURL) Fetch(field string) []string            { return nil }

// NoteOpener can be used to find the parameters for creating the URL where the
// note can be opened.
type NoteOpener struct {
	*FileOpener
}

// Open will return an NoteOpener for the given file.
func OpenNote(inst *instance.Instance, fileID string) (*NoteOpener, error) {
	file, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return nil, err
	}

	// Check that the file is a note
	if file, err = note.GetFile(inst, file); err != nil {
		return nil, err
	}

	opener, err := NewFileOpener(inst, file)
	if err != nil {
		return nil, err
	}
	return &NoteOpener{opener}, nil
}

// GetResult looks if the note can be opened locally or not, which code can be
// used in case of a shared note, and other parameters.. and returns the information.
func (o *NoteOpener) GetResult(memberIndex int, readOnly bool) (jsonapi.Object, error) {
	prepared, err := o.PrepareOpenFileRequest(memberIndex, readOnly)
	if err != nil {
		return nil, err
	}
	var result *apiNoteURL
	if prepared.Opts == nil {
		result, err = o.openLocalNote(prepared.MemberIndex, prepared.ReadOnly)
	} else {
		result, err = o.openSharedNote(prepared)
	}
	if err != nil {
		return nil, err
	}

	// Enforce DocID and PublicName with local values
	result.DocID = o.File.ID()
	if name, err := settings.PublicName(o.Inst); err == nil {
		result.PublicName = name
	}
	return result, nil
}

func (o *NoteOpener) openLocalNote(memberIndex int, readOnly bool) (*apiNoteURL, error) {
	// If the note came from another cozy via a sharing that is now revoked, we
	// may need to recreate the trigger.
	// This should be taken care of when revoking the sharing now but we leave
	// this call to make sure notes from previously revoked sharings will
	// continue to work.
	_ = note.SetupTrigger(o.Inst, o.File.ID())

	params, err := o.OpenLocalFileForMember(memberIndex, readOnly)
	if err != nil {
		return nil, err
	}
	doc := apiNoteURL{
		NoteID:    params.FileID,
		Protocol:  params.Protocol,
		Subdomain: params.Subdomain,
		Instance:  params.Instance,
		Sharecode: params.Sharecode,
	}
	return &doc, nil
}

func (o *NoteOpener) openSharedNote(prepared *PreparedRequest) (*apiNoteURL, error) {
	res, err := o.RequestSharedFile(prepared, "/notes/"+prepared.XoredID+"/open")
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return nil, ErrInternalServerError
	}
	var doc apiNoteURL
	if _, err := jsonapi.Bind(res.Body, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}
