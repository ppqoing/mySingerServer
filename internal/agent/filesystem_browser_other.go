//go:build !windows

package agent

import (
	"context"

	"dedup/internal/proto"
)

type unsupportedFilesystemBrowser struct{}

func NewFilesystemBrowser() FilesystemBrowser {
	return unsupportedFilesystemBrowser{}
}

func (unsupportedFilesystemBrowser) Browse(
	context.Context,
	proto.FilesystemBrowseRequest,
) proto.FilesystemBrowseResponse {
	return proto.FilesystemBrowseResponse{ErrorCode: "browse_unsupported"}
}
