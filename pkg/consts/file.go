package consts

const (
	// DirType is the type attribute for directories
	DirType = "directory"
	// FileType is the type attribute for files
	FileType = "file"
)

const (
	// RootDirID is the root directory identifier
	RootDirID = "io.cozy.files.root-dir"
	// TrashDirID is the trash directory identifier
	TrashDirID = "io.cozy.files.trash-dir"
	// SharedWithMeDirID is the identifier of the directory where the sharings
	// will put their folders for the shared files
	SharedWithMeDirID = "io.cozy.files.shared-with-me-dir"
	// NoLongerSharedDirID is the identifier of the directory where the files &
	// folders removed from a sharing but still used via a reference are put
	NoLongerSharedDirID = "io.cozy.files.no-longer-shared-dir"
	// SharedDrivesDirID is the legacy container ID, retained for existing data.
	SharedDrivesDirID = "io.cozy.files.shared-drives-dir"
)

const (
	// ShortcutMimeType is the mime-type for the .url files.
	ShortcutMimeType = "application/internet-shortcut"
	// NoteMimeType is the mime-type for the .cozy-note files.
	NoteMimeType = "text/vnd.cozy.note+markdown"
	// NoteExtension is the extension for the .cozy-note files.
	NoteExtension = ".cozy-note"
	// DocsExtension is the extension for the .docs-note files.
	DocsExtension = ".docs-note"
	// MarkdownExtension is the extension for the markdown files.
	MarkdownExtension = ".md"
	// ImageClass is the class for the image files
	ImageClass = "image"
	// VideoClass is the class for the video files
	VideoClass = "video"
	// AudioClass is the class for the audio files
	AudioClass = "audio"
)

const (
	// CarbonCopyKey is the metadata key for a carbon copy (certified)
	CarbonCopyKey = "carbonCopy"
	// ElectronicSafeKey is the metadata key for an electronic safe (certified)
	ElectronicSafeKey = "electronicSafe"
	// FavoriteKey is the metadata key for a favorite.
	FavoriteKey = "favorite"
)

const (
	// NoteImageOriginalFormat is the format for the original image in a note.
	NoteImageOriginalFormat = "original"
	// NoteImageThumbFormat is the format for a resized image in a note.
	NoteImageThumbFormat = "thumb"
)
