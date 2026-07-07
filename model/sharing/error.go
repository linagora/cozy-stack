package sharing

import "errors"

var (
	// ErrNoRules is used when a sharing is created without a rule
	ErrNoRules = errors.New("A sharing must have rules")
	// ErrNoRecipients is used when a sharing is created without a recipient
	ErrNoRecipients = errors.New("A sharing must have recipients")
	// ErrTooManyMembers is used when a sharing has too many members
	ErrTooManyMembers = errors.New("There are too many members for this sharing")
	// ErrInvalidURL is used for invalid URL of a Cozy instance
	ErrInvalidURL = errors.New("The Cozy URL is invalid")
	// ErrInvalidRule is used when a rule is invalid when the sharing is
	// created
	ErrInvalidRule = errors.New("A rule is invalid")
	// ErrInvalidSharing is used when an action cannot be made on a sharing,
	// because this sharing is not the expected state
	ErrInvalidSharing = errors.New("Sharing is not in the expected state")
	// ErrMemberNotFound is used when trying to find a member, but there is no
	// member with the expected value for the criterion
	ErrMemberNotFound = errors.New("The member was not found")
	// ErrInvitationNotSent is used when the invitation shortcut or mail failed
	// to be sent
	ErrInvitationNotSent = errors.New("The invitation cannot be sent")
	// ErrRequestFailed is used when a cozy tries to create a sharing request
	// on another cozy, but it failed
	ErrRequestFailed = errors.New("The sharing request failed")
	// ErrNoOAuthClient is used when the owner of the Cozy has not yet
	// registered to the recipient as an OAuth client.
	ErrNoOAuthClient = errors.New("No OAuth client was found")
	// ErrInternalServerError is used for CouchDB errors
	ErrInternalServerError = errors.New("Internal Server Error")
	// ErrClientError is used when an OAuth client has made a request, and the
	// response was a 4xx error
	ErrClientError = errors.New("OAuth client request was in error")
	// ErrMissingID is used when _id is missing on a doc for a bulk operation
	ErrMissingID = errors.New("An identifier is missing")
	// ErrMissingRev is used when _rev is missing on a doc for a bulk operation
	ErrMissingRev = errors.New("A revision is missing")
	// ErrMissingFileMetadata is used when uploading a file and the key is not
	// in the cache (so no metadata and the upload can't succeed)
	ErrMissingFileMetadata = errors.New("The metadata for this file were not found")
	// ErrFolderNotFound is used when informations about a folder is asked,
	// but this folder was not found
	ErrFolderNotFound = errors.New("This folder was not found")
	// ErrDriveRootNotFound is used when a drive root identifier can point to
	// either a file or a folder, but nothing was found.
	ErrDriveRootNotFound = errors.New("This file or folder was not found")
	// ErrFileNotFound is used when informations about a file is asked,
	// but this file was not found
	ErrFileNotFound = errors.New("This file was not found")
	// ErrSafety is used when an operation is aborted due to the safety principal
	ErrSafety = errors.New("Operation aborted")
	// ErrAlreadyAccepted is used when someone tries to accept twice a sharing
	// on the same cozy instance
	ErrAlreadyAccepted = errors.New("Sharing already accepted by this recipient")
	// ErrCannotOpenFile is used when opening a file fails
	ErrCannotOpenFile = errors.New("The file cannot be opened")
	// ErrGroupCannotBeAddedTwice is used when trying to add a group to a
	// sharing, but the group is already active for this sharing.
	ErrGroupCannotBeAddedTwice = errors.New("The group cannot be added twice to the same sharing")
	// ErrMemberAlreadyAdded is used when trying to add a group with a member
	// already in the sharing as an individual with different rights (read-only
	// vs read-write).
	ErrMemberAlreadyAdded = errors.New("A group member cannot be added as they are already in the sharing")
	// ErrMemberAlreadyInGroup is used when trying to add a group with a member
	// already in another group of the sharing with different rights.
	ErrMemberAlreadyInGroup = errors.New("A group member cannot be added as they are already in another group of the sharing")
	// ErrFolderAlreadyShared is used when the folder already has an existing sharing
	ErrFolderAlreadyShared = errors.New("Folder already has an existing sharing")
	// ErrNotADirectory is used when the provided folder_id is not a directory
	ErrNotADirectory = errors.New("Provided ID is not a directory")
	// ErrFileAlreadyShared is used when the file already has an existing sharing
	ErrFileAlreadyShared = errors.New("File already has an existing sharing")
	// ErrNotAFile is used when the provided file_id is not a file
	ErrNotAFile = errors.New("Provided ID is not a file")
	// ErrFileInTrash is used when trying to share a trashed file
	ErrFileInTrash = errors.New("Cannot share trashed file")
	// ErrSystemFolder is used when trying to share a system folder
	ErrSystemFolder = errors.New("Cannot share system folder")
	// ErrGroupReadOnlyConflict is used when changing a group's read-only flag
	// would conflict with another group's state for a member belonging to both.
	ErrGroupReadOnlyConflict = errors.New("Cannot change group read-only flag: a member also belongs to another group with a conflicting read-only state")
)
