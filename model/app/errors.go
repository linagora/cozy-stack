package app

import "errors"

var (
	// ErrInvalidSlugName is used when the given slug name is not valid
	ErrInvalidSlugName = errors.New("Invalid slug name")
	// ErrAlreadyExists is used when an application with the specified slug name
	// is already installed.
	ErrAlreadyExists = errors.New("Application with same slug already exists")
	// ErrNotFound is used when no application with specified slug name is
	// installed.
	// Used by Cloudery, don't modify it
	ErrNotFound = errors.New("Application is not installed")
	// ErrNotSupportedSource is used when the source transport or
	// protocol is not supported
	ErrNotSupportedSource = errors.New("Invalid or not supported source scheme")
	// ErrManifestNotReachable is used when the manifest of the
	// application is not reachable
	ErrManifestNotReachable = errors.New("Application manifest is not reachable")
	// ErrSourceNotReachable is used when the given source for
	// application is not reachable
	ErrSourceNotReachable = errors.New("Application source is not reachable")
	// ErrBadManifest when the manifest is not valid or malformed
	ErrBadManifest = errors.New("Application manifest is invalid or malformed")
	// ErrBadState is used when trying to use the application while in a
	// state that is not appropriate for the given operation.
	ErrBadState = errors.New("Application is not in valid state to perform this operation")
	// ErrMissingSource is used when installing an application, but there is no
	// source URL
	ErrMissingSource = errors.New("The source URL for the app is missing")
	// ErrBadChecksum is used when the application checksum does not match the
	// specified one.
	ErrBadChecksum = errors.New("Application checksum does not match")
	// ErrLinkedAppExists is used when an OAuth client is linked to this app
	ErrLinkedAppExists = errors.New("A linked OAuth client exists for this app")
)
